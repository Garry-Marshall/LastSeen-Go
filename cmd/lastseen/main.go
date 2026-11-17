// Command lastseen runs the LastSeen Discord bot.
//
// It is the Go port of the Python bot's main.py: same startup sequence, log
// lines and exit codes (0 clean stop, 1 crash = restart, 2 configuration
// error = do not restart). Run it from the bot's folder: .env, the database,
// locales/, logs/ and backups/ are resolved relative to the working directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"lastseen/internal/bot"
	"lastseen/internal/config"
	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/logx"
)

var log = logx.Get("__main__")

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("LastSeen %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	code, details := runSafely()
	writeCrashLog(code, details)
	logx.Close()
	os.Exit(code)
}

// runSafely runs the bot, turning a panic into a crash exit with its stack.
func runSafely() (code int, details string) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Fatal error occurred: %v\n%s", r, debug.Stack())
			code, details = bot.ExitCrash, fmt.Sprintf("panic: %v\n\n%s", r, debug.Stack())
		}
	}()
	return run()
}

func run() (int, string) {
	log.Info("Loading configuration...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("Configuration error:")
		log.Error("%v", err)
		if errors.Is(err, config.ErrMissingEnv) {
			log.Error("\nPlease follow these steps:")
			log.Error("1. Edit the .env file")
			log.Error("2. Add your Discord bot token")
			log.Error("3. Restart the bot")
		}
		return bot.ExitConfig, ""
	}

	if err := logx.Setup(cfg.LogFolder, cfg.LogLevel, cfg.LogsDaysToKeep); err != nil {
		log.Error("Failed to set up log file in %s: %v", cfg.LogFolder, err)
	}
	log.Info("Configuration loaded successfully")

	log.Info("Creating bot instance...")
	locale.Load("locales")
	database, err := db.Open(cfg.DBFile, bot.DBWorkers)
	if err != nil {
		log.Error("Fatal error occurred: failed to open database %s: %v", cfg.DBFile, err)
		return bot.ExitCrash, fmt.Sprintf("failed to open database %s: %v\n", cfg.DBFile, err)
	}
	// Reclaim space freed by removed guilds and pruned activity. Only here,
	// before connecting: VACUUM blocks all writes while it runs.
	database.VacuumIfFragmented()
	b := bot.New(cfg, database)
	defer b.Close()

	// Ctrl+C (and SIGTERM from systemd) stops the bot cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("Setting up bot...")
	err = b.Run(ctx)
	if err == nil {
		log.Info("Bot stopped by user")
		return bot.ExitOK, ""
	}
	var ee *bot.ExitError
	if errors.As(err, &ee) && ee.Code == bot.ExitConfig {
		return bot.ExitConfig, "" // already logged by the bot
	}
	code := bot.ExitCrash
	if ee != nil {
		code = ee.Code
	}
	log.Error("Fatal error occurred: %v", err)
	return code, err.Error() + "\n"
}

// writeCrashLog appends the failure to logs/crash.log when the bot exits
// abnormally, independently of the rotating log, so early crashes are kept.
func writeCrashLog(code int, details string) {
	if code == bot.ExitOK {
		return
	}
	dir := "logs"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if details == "" {
		details = "No traceback available.\n"
	}
	sep := strings.Repeat("=", 60)
	ts := time.Now().UTC().Format("2006-01-02 15:04:05") + " UTC"
	fmt.Fprintf(f, "\n%s\nCrash at %s (exit code %d)\n%s\n%s\n", sep, ts, code, sep, details)
}
