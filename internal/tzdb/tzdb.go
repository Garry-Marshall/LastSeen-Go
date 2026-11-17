// Package tzdb validates and loads guild time zones the way the Python bot
// did with pytz: a name is valid when it is in pytz.all_timezones (embedded
// in zones_gen.go), and the zone data comes from Go's embedded tzdata so it
// works the same on Windows and Linux.
package tzdb

import (
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // zone data inside the binary (Windows has no zoneinfo)
)

var (
	once  sync.Once
	valid map[string]bool
	cache sync.Map // name -> *time.Location
)

func index() {
	once.Do(func() {
		valid = make(map[string]bool, len(allZones))
		for _, z := range allZones {
			valid[z] = true
		}
	})
}

// Valid reports whether name is in pytz.all_timezones.
func Valid(name string) bool {
	index()
	return valid[name]
}

// All is pytz.all_timezones (do not modify).
func All() []string { return allZones }

// Suggestions is the first n zones containing s, case-insensitively
// ([tz for tz in pytz.all_timezones if s.lower() in tz.lower()][:n]).
func Suggestions(s string, n int) []string {
	low := strings.ToLower(s)
	var out []string
	for _, z := range allZones {
		if strings.Contains(strings.ToLower(z), low) {
			out = append(out, z)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// Load returns the location for a valid zone name, or UTC (like the Python
// bot's `pytz.timezone(tz) if tz in pytz.all_timezones else pytz.UTC`).
func Load(name string) *time.Location {
	if !Valid(name) {
		return time.UTC
	}
	if l, ok := cache.Load(name); ok {
		return l.(*time.Location)
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	cache.Store(name, l)
	return l
}
