package bot

import (
	"syscall"
	"time"
)

// processCPUTime is the user+kernel CPU time used by this process.
func processCPUTime() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, false
	}
	ticks := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((ticks(k) + ticks(u)) * 100), true // 100ns units
}
