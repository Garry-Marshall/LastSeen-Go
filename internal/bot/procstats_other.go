//go:build !linux

package bot

import (
	"runtime"
	"time"
)

// readProcStats approximates psutil off Linux (the bot runs on Linux in
// production; this keeps /about working on a Windows dev machine).
func readProcStats() procStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ps := procStats{memMB: float64(ms.Sys) / 1024 / 1024, threads: osThreads()}
	t0, ok0 := processCPUTime()
	start := time.Now()
	time.Sleep(100 * time.Millisecond)
	t1, ok1 := processCPUTime()
	if ok0 && ok1 {
		ps.cpuPct = (t1 - t0).Seconds() / time.Since(start).Seconds() * 100
	}
	return ps
}
