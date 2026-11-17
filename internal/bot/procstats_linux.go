package bot

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// readProcStats reads RSS and thread count from /proc/self/status and the
// CPU share from /proc/self/stat over 0.1s.
func readProcStats() procStats {
	var ps procStats
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "VmRSS:":
				if kb, err := strconv.ParseFloat(f[1], 64); err == nil {
					ps.memMB = kb / 1024
				}
			case "Threads:":
				ps.threads, _ = strconv.Atoi(f[1])
			}
		}
	}
	if ps.threads == 0 {
		ps.threads = osThreads()
	}
	t0, ok0 := cpuTicks()
	start := time.Now()
	time.Sleep(100 * time.Millisecond)
	t1, ok1 := cpuTicks()
	if ok0 && ok1 {
		const clkTck = 100 // USER_HZ on every mainstream Linux
		ps.cpuPct = float64(t1-t0) / clkTck / time.Since(start).Seconds() * 100
	}
	return ps
}

// cpuTicks is utime+stime of this process in clock ticks.
func cpuTicks() (int64, bool) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	// The command name may contain spaces; fields resume after the last ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	k, err2 := strconv.ParseInt(f[12], 10, 64)
	return u + k, err1 == nil && err2 == nil
}
