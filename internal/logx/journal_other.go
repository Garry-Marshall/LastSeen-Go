//go:build !linux

package logx

// stderrIsJournal: the systemd journal only exists on Linux.
func stderrIsJournal() bool { return false }
