package bot

import (
	"runtime"
	"runtime/pprof"
	"strings"
)

// procStats is what /about shows about this process.
type procStats struct {
	memMB   float64
	cpuPct  float64 // over a 0.1s sample, like cpu_percent(0.1)
	threads int
}

// goVersion is the toolchain version without the "go" prefix ("1.25.1").
func goVersion() string { return strings.TrimPrefix(runtime.Version(), "go") }

// osThreads counts the OS threads the Go runtime has created.
func osThreads() int {
	if p := pprof.Lookup("threadcreate"); p != nil {
		return p.Count()
	}
	return 0
}
