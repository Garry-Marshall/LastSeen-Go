//go:build !linux && !windows

package bot

import "time"

func processCPUTime() (time.Duration, bool) { return 0, false }
