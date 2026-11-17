package db

import (
	"time"

	"fmt"
	"lastseen/internal/tzdb"

	"lastseen/internal/pyfmt"
)

var sprintf = fmt.Sprintf

// roundTo is Python's round(x, n).
func roundTo(x float64, n int) float64 { return pyfmt.RoundTo(x, n) }

// pyRound is Python's round(x) for a float.
func pyRound(x float64) int64 { return pyfmt.Round(x) }

// LoadTZ is the guild's time zone (UTC when unset or unknown).
func LoadTZ(name string) *time.Location { return tzdb.Load(name) }
