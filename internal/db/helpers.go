package db

import (
	"time"

	"fmt"
	"lastseen/internal/tzdb"

	"lastseen/internal/textfmt"
)

var sprintf = fmt.Sprintf

// roundTo rounds x to n decimals, ties to even.
func roundTo(x float64, n int) float64 { return textfmt.RoundTo(x, n) }

// roundHalfEven rounds x to a whole number, ties to even (2.5 -> 2).
func roundHalfEven(x float64) int64 { return textfmt.Round(x) }

// weekdayFromMonday is the day of the week counted from Monday (0) to
// Sunday (6).
func weekdayFromMonday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// LoadTZ is the guild's time zone (UTC when unset or unknown).
func LoadTZ(name string) *time.Location { return tzdb.Load(name) }
