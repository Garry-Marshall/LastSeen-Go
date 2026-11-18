// Package logx is the bot's logger. Every line names the part of the bot it
// comes from:
//
//	2026-01-27 14:03:11 [INFO] tracking: message
//
// Lines go to the console (stderr) and to logs/bot.log, which rotates at UTC
// midnight into logs/YYYY-MM-DD.log (the date of the day that ended).
//
// Under systemd the console is the journal, which timestamps every entry
// itself. There the timestamp is left out and each line starts with its
// syslog priority, so journalctl colours errors and warnings and
// "journalctl -p warning" filters on them.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Level is a message's severity.
type Level int

const (
	Debug Level = iota
	Info
	Warning
	Error
)

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warning:
		return "WARNING"
	}
	return "ERROR"
}

// priority is the syslog priority prefix the journal reads from a line.
func (l Level) priority() string {
	switch l {
	case Debug:
		return "<7>"
	case Info:
		return "<6>"
	case Warning:
		return "<4>"
	}
	return "<3>"
}

// ParseLevel maps a DEBUG_LEVEL value (case-insensitive) to a Level; unknown
// values give Info.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return Debug
	case "WARNING", "WARN":
		return Warning
	case "ERROR", "CRITICAL", "FATAL":
		return Error
	}
	return Info
}

var (
	mu       sync.Mutex
	minLevel           = Info
	console  io.Writer = os.Stderr
	journal            = stderrIsJournal()
	file     *rotatingFile
)

// Logger writes lines tagged with a component name ("tracking", "db"...).
type Logger struct{ name string }

// Get returns the logger for a component.
func Get(name string) *Logger { return &Logger{name: name} }

func (l *Logger) Debug(format string, args ...any) { l.log(Debug, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.log(Info, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.log(Warning, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.log(Error, format, args...) }

func (l *Logger) log(level Level, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	write(level, l.name, msg)
}

// fileLine is the full line, as written to logs/bot.log.
func fileLine(now time.Time, level Level, name, msg string) string {
	return fmt.Sprintf("%s [%s] %s: %s\n", now.Format("2006-01-02 15:04:05"), level, name, msg)
}

// journalLine drops the timestamp (the journal adds its own) and prefixes
// every line of the message with its priority, so a multi-line message keeps
// its level throughout.
func journalLine(level Level, name, msg string) string {
	p := level.priority()
	return fmt.Sprintf("%s[%s] %s: %s\n", p, level, name, strings.ReplaceAll(msg, "\n", "\n"+p))
}

func write(level Level, name, msg string) {
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if level < minLevel {
		return
	}
	line := fileLine(now, level, name, msg)
	if journal {
		_, _ = io.WriteString(console, journalLine(level, name, msg))
	} else {
		_, _ = io.WriteString(console, line)
	}
	if file != nil {
		file.write(now, line)
	}
}

// Setup sets the level and starts writing to dir/bot.log, after deleting
// dated log files older than keepDays.
func Setup(dir string, level Level, keepDays int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	CleanupOldLogs(dir, keepDays)
	rf, err := openRotating(dir, keepDays)
	if err != nil {
		return err
	}
	mu.Lock()
	minLevel = level
	if file != nil {
		file.close()
	}
	file = rf
	mu.Unlock()
	Get("config").Info("Logging configured: %s", filepath.Join(dir, "bot.log"))
	return nil
}

// Close closes the log file.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		file.close()
		file = nil
	}
}

// datePattern finds the date in a rotated log's name: YYYY-MM-DD.log, or
// bot.log.YYYY-MM-DD as the Python bot named them.
var datePattern = regexp.MustCompile(`^(?:.*\.)?(\d{4}-\d{2}-\d{2})`)

// CleanupOldLogs deletes dated log files older than keepDays; bot.log itself
// is never touched.
func CleanupOldLogs(dir string, keepDays int) {
	log := Get("config")
	matches, err := filepath.Glob(filepath.Join(dir, "*.log*"))
	if err != nil {
		log.Error("Error during log cleanup: %v", err)
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays)
	deleted := 0
	for _, p := range matches {
		name := filepath.Base(p)
		if name == "bot.log" {
			continue
		}
		m := datePattern.FindStringSubmatch(name)
		if m == nil {
			log.Debug("Skipping file without date pattern: %s", name)
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", m[1], time.UTC)
		if err != nil {
			log.Debug("Skipping non-date log file: %s", name)
			continue
		}
		if d.Before(cutoff) {
			if err := os.Remove(p); err != nil {
				log.Warn("Failed to delete log file %s: %v", name, err)
				continue
			}
			deleted++
			log.Info("Deleted old log file: %s", name)
		}
	}
	if deleted > 0 {
		log.Info("Cleaned up %d old log file(s)", deleted)
	}
}

type rotatingFile struct {
	dir        string
	keepDays   int
	f          *os.File
	rolloverAt time.Time
}

func nextUTCMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func openRotating(dir string, keepDays int) (*rotatingFile, error) {
	path := filepath.Join(dir, "bot.log")
	rf := &rotatingFile{dir: dir, keepDays: keepDays}
	start := time.Now()
	if st, err := os.Stat(path); err == nil {
		start = st.ModTime() // an existing file belongs to the day it was last written
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	rf.f = f
	rf.rolloverAt = nextUTCMidnight(start)
	return rf, nil
}

// write is called with mu held.
func (r *rotatingFile) write(now time.Time, line string) {
	if !now.Before(r.rolloverAt) {
		r.rotate(now)
	}
	if r.f != nil {
		_, _ = r.f.WriteString(line)
	}
}

func (r *rotatingFile) rotate(now time.Time) {
	path := filepath.Join(r.dir, "bot.log")
	ended := r.rolloverAt.AddDate(0, 0, -1).Format("2006-01-02")
	r.f.Close()
	dated := filepath.Join(r.dir, ended+".log")
	if _, err := os.Stat(dated); err == nil {
		// Never overwrite an existing day's log: append to it instead.
		if data, err := os.ReadFile(path); err == nil {
			if df, err := os.OpenFile(dated, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				_, _ = df.Write(data)
				df.Close()
				_ = os.Remove(path)
			}
		}
	} else {
		_ = os.Rename(path, dated)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		r.f = nil
		return
	}
	r.f = f
	r.rolloverAt = nextUTCMidnight(now)
	go CleanupOldLogs(r.dir, r.keepDays)
}

func (r *rotatingFile) close() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}

// SlogHandler routes a library's log/slog output (disgo's) into this logger
// under one component name. Records below min are dropped: pass slog.LevelInfo
// to keep a library's very chatty debug output out even when the bot itself
// logs at debug level.
type SlogHandler struct {
	name  string
	min   slog.Level
	attrs []slog.Attr
}

// NewSlogHandler returns a slog.Handler writing as component name.
func NewSlogHandler(name string, min slog.Level) *SlogHandler {
	return &SlogHandler{name: name, min: min}
}

func (h *SlogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.min }

func (h *SlogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	add := func(a slog.Attr) bool {
		if a.Key == "name" {
			return true
		}
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	lvl := Info
	switch {
	case r.Level >= slog.LevelError:
		lvl = Error
	case r.Level >= slog.LevelWarn:
		lvl = Warning
	case r.Level < slog.LevelInfo:
		lvl = Debug
	}
	write(lvl, h.name, b.String())
	return nil
}

func (h *SlogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &n
}

func (h *SlogHandler) WithGroup(string) slog.Handler { return h }
