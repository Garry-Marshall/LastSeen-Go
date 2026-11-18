//go:build linux

package logx

import (
	"fmt"
	"os"
	"syscall"
)

// stderrIsJournal reports whether stderr is connected to the systemd journal.
// systemd sets JOURNAL_STREAM to "<device>:<inode>" of that connection.
// Comparing it with stderr itself keeps a process that merely inherited the
// variable (with stderr redirected elsewhere) from being fooled.
func stderrIsJournal() bool {
	v := os.Getenv("JOURNAL_STREAM")
	if v == "" {
		return false
	}
	var dev, ino uint64
	if _, err := fmt.Sscanf(v, "%d:%d", &dev, &ino); err != nil {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stderr.Fd()), &st); err != nil {
		return false
	}
	return uint64(st.Dev) == dev && uint64(st.Ino) == ino
}
