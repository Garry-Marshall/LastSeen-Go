// Package tzdb validates and loads guild time zones.
//
// A name is valid when it is in the IANA time zone database as shipped with
// Go: zones_gen.go lists those names, generated from the Go toolchain's own
// copy of the database (see gen_zones.go). The zone data itself is built into
// the binary through time/tzdata, so every listed zone loads on Windows (which
// has no system time zone database) as well as on Linux.
package tzdb

//go:generate go run gen_zones.go

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

// Valid reports whether name is a known time zone name (case-sensitive,
// e.g. "Europe/Brussels").
func Valid(name string) bool {
	index()
	return valid[name]
}

// Suggestions returns up to n zone names containing s, case-insensitively,
// in alphabetical order.
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

// Load returns the location for a valid zone name, or UTC for anything else.
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
