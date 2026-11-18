package logx

import (
	"testing"
	"time"
)

func TestLineFormats(t *testing.T) {
	now := time.Date(2026, 1, 27, 14, 3, 11, 0, time.Local)
	if got, want := fileLine(now, Info, "tracking", "hello"),
		"2026-01-27 14:03:11 [INFO] tracking: hello\n"; got != want {
		t.Errorf("fileLine = %q, want %q", got, want)
	}
	if got, want := journalLine(Warning, "db", "slow query"),
		"<4>[WARNING] db: slow query\n"; got != want {
		t.Errorf("journalLine = %q, want %q", got, want)
	}
	// Every line of a multi-line message keeps the priority.
	if got, want := journalLine(Error, "main", "crash\nstack"),
		"<3>[ERROR] main: crash\n<3>stack\n"; got != want {
		t.Errorf("journalLine (multi-line) = %q, want %q", got, want)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]Level{
		"debug": Debug, " INFO ": Info, "warning": Warning, "warn": Warning,
		"error": Error, "critical": Error, "": Info, "nonsense": Info,
	} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
