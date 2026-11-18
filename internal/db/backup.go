package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"

	"lastseen/internal/textfmt"
)

// CreateBackup copies the database page by page (SQLite's online backup API)
// into backupFolder/lastseen_backup_<local timestamp>.db, then runs a full
// integrity check on the copy. Because pages are copied verbatim, corruption
// in the live database shows up in the copy; such a backup is deleted and ""
// is returned, so the caller skips rotation and keeps the older backups.
func (d *DB) CreateBackup(backupFolder string) string {
	if err := os.MkdirAll(backupFolder, 0o755); err != nil {
		log.Error("Failed to create database backup: %v", err)
		return ""
	}
	path := filepath.Join(backupFolder, "lastseen_backup_"+time.Now().Format("20060102_150405")+".db")
	if err := d.backupTo(path); err != nil {
		os.Remove(path)
		log.Error("Failed to create database backup: %v", err)
		return ""
	}
	problems, err := integrityCheck(path)
	if err != nil {
		os.Remove(path)
		log.Error("Failed to create database backup: %v", err)
		return ""
	}
	if len(problems) != 1 || problems[0] != "ok" {
		os.Remove(path)
		log.Error("Database integrity check failed; backup discarded, old backups kept: %s", strings.Join(problems, "; "))
		return ""
	}
	log.Info("Database backup created successfully: %s", filepath.Base(path))
	return path
}

type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

func (d *DB) backupTo(path string) error {
	conn, err := d.sql.Conn(ctx())
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return errors.New("driver does not support the backup API")
		}
		bk, err := b.NewBackup(path)
		if err != nil {
			return err
		}
		for {
			more, err := bk.Step(-1)
			if err != nil {
				bk.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return bk.Finish()
	})
}

func integrityCheck(path string) ([]string, error) {
	c, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rows, err := c.QueryContext(ctx(), "PRAGMA integrity_check(10)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func backupFiles(folder string) ([]os.FileInfo, error) {
	matches, err := filepath.Glob(filepath.Join(folder, "lastseen_backup_*.db"))
	if err != nil {
		return nil, err
	}
	var out []os.FileInfo
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// LatestBackupTime is the newest backup's modification time (ok false if none).
func LatestBackupTime(backupFolder string) (time.Time, bool) {
	files, err := backupFiles(backupFolder)
	if err != nil {
		log.Error("Failed to read backup folder %s: %v", backupFolder, err)
		return time.Time{}, false
	}
	var latest time.Time
	for _, f := range files {
		if f.ModTime().After(latest) {
			latest = f.ModTime()
		}
	}
	return latest, len(files) > 0
}

// CleanupOldBackups keeps the retentionCount newest backups and deletes the
// rest; returns the number deleted.
func CleanupOldBackups(backupFolder string, retentionCount int) int {
	if _, err := os.Stat(backupFolder); err != nil {
		return 0
	}
	files, err := backupFiles(backupFolder)
	if err != nil {
		log.Error("Failed to cleanup old backups: %v", err)
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().After(files[j].ModTime()) })
	deleted := 0
	if len(files) > retentionCount {
		for _, f := range files[retentionCount:] {
			if err := os.Remove(filepath.Join(backupFolder, f.Name())); err != nil {
				log.Warn("Failed to delete backup %s: %v", f.Name(), err)
				continue
			}
			deleted++
			log.Info("Deleted old backup: %s", f.Name())
		}
	}
	if deleted > 0 {
		log.Info("Cleaned up %d old backup(s), keeping %d most recent", deleted, min(len(files), retentionCount))
	}
	return deleted
}

// VacuumIfFragmented runs VACUUM when enough of the file is free pages.
// VACUUM holds the write lock for its whole run, so call it at startup,
// before connecting to Discord. Returns true if VACUUM ran.
func (d *DB) VacuumIfFragmented() bool {
	conn, err := d.sql.Conn(ctx())
	if err != nil {
		log.Error("VACUUM failed (the bot continues without it): %v", err)
		return false
	}
	defer conn.Close()
	pragma := func(name string) (int64, error) { return scalarInt(conn, "PRAGMA "+name) }
	fail := func(err error) bool {
		log.Error("VACUUM failed (the bot continues without it): %v", err)
		return false
	}
	pageSize, err := pragma("page_size")
	if err != nil {
		return fail(err)
	}
	pages, err := pragma("page_count")
	if err != nil {
		return fail(err)
	}
	free, err := pragma("freelist_count")
	if err != nil {
		return fail(err)
	}
	mb := func(p int64) float64 { return float64(p*pageSize) / (1024 * 1024) }
	freeMB := mb(free)
	if pages == 0 {
		log.Info("Empty database; no VACUUM needed")
		return false
	}
	ratio := float64(free) / float64(pages)
	desc := textfmt.Value(freeMB, ".1f") + " MB free (" + textfmt.Value(ratio, ".0%") + " of file)"
	if ratio < vacuumMinFreeRatio || freeMB < vacuumMinFreeMB {
		log.Info("Database has %s; no VACUUM needed", desc)
		return false
	}
	log.Info("Database has %s; running VACUUM before connecting...", desc)
	start := time.Now()
	if _, err := conn.ExecContext(ctx(), "VACUUM"); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fail(err)
	}
	after, err := pragma("page_count")
	if err != nil {
		return fail(err)
	}
	log.Info("VACUUM completed in %ss: %s MB -> %s MB", textfmt.Value(time.Since(start).Seconds(), ".1f"),
		textfmt.Value(mb(pages), ".1f"), textfmt.Value(mb(after), ".1f"))
	return true
}
