package pyfmt

import "testing"

// Expected values were produced by CPython 3.12.
func TestValue(t *testing.T) {
	cases := []struct {
		v    any
		spec string
		want string
	}{
		{1234567, ",", "1,234,567"},
		{-1234, ",", "-1,234"},
		{999, ",", "999"},
		{0, ",", "0"},
		{5, "+,", "+5"},
		{-1234, "+,", "-1,234"},
		{0, "+,", "+0"},
		{1234.5, ",", "1,234.5"},
		{12.0, ",", "12.0"},
		{12.0, "", "12.0"},
		{0.1, "", "0.1"},
		{1e16, "", "1e+16"},
		{123456789.0, "", "123456789.0"},
		{0.0001, "", "0.0001"},
		{0.00001, "", "1e-05"},
		{5, "02d", "05"},
		{23, "02d", "23"},
		{7, "02", "07"},
		{66.666, ".1f", "66.7"},
		{0.125, ".2f", "0.12"},
		{2.5, ".0f", "2"},
		{3.5, ".0f", "4"},
		{3, ".1f", "3.0"},
		{1234.5678, ",.2f", "1,234.57"},
		{42.0, ">3.0f", " 42"},
		{100.0, ">3.0f", "100"},
		{"abc", "<6", "abc   "},
		{"abc", ">6", "   abc"},
		{"abc", "^7", "  abc  "},
		{"ab", "", "ab"},
		{1234, ">5,", "1,234"},
		{12, ">5,", "   12"},
		{12345, ">6,", "12,345"},
		{7, "<6", "7     "},
		{"Mo", "<4", "Mo  "},
		{"█░", "<4", "█░  "},
		{true, "", "True"},
		{nil, "", "None"},
		{12.5, "%", "1250.000000%"},
		{0.5, ".1%", "50.0%"},
	}
	for _, c := range cases {
		got, err := Value(c.v, c.spec)
		if err != nil {
			t.Errorf("Value(%#v, %q) error: %v", c.v, c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("Value(%#v, %q) = %q, want %q", c.v, c.spec, got, c.want)
		}
	}
}

func TestValueErrors(t *testing.T) {
	for _, c := range []struct {
		v    any
		spec string
	}{
		{"x", ","},
		{"x", "+"},
		{5, ".1"},
		{nil, ","},
		{5, "z"},
	} {
		if got, err := Value(c.v, c.spec); err == nil {
			t.Errorf("Value(%#v, %q) = %q, want error", c.v, c.spec, got)
		}
	}
}

func TestFormat(t *testing.T) {
	got, err := Format("a {{x}} {y} {n:,} {f:.1f}", map[string]any{"y": "b", "n": 12345, "f": 2.25})
	if err != nil || got != "a {x} b 12,345 2.2" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, tmpl := range []string{"{missing}", "{0}", "oops }", "oops {", "{n:,}"} {
		if _, err := Format(tmpl, map[string]any{"n": "str"}); err == nil {
			t.Errorf("Format(%q) succeeded, want error", tmpl)
		}
	}
	// Unused args are fine, like str.format(**kwargs).
	if got, err := Format("plain", map[string]any{"x": 1}); err != nil || got != "plain" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRound(t *testing.T) {
	for _, c := range []struct {
		x    float64
		want int64
	}{{2.5, 2}, {3.5, 4}, {-0.5, 0}, {-1.5, -2}, {0.4999, 0}, {14.6, 15}} {
		if got := Round(c.x); got != c.want {
			t.Errorf("Round(%v) = %d, want %d", c.x, got, c.want)
		}
	}
	for _, c := range []struct {
		x    float64
		n    int
		want float64
	}{{2.675, 2, 2.67}, {0.125, 2, 0.12}, {12.34, 1, 12.3}, {66.66666, 1, 66.7}} {
		if got := RoundTo(c.x, c.n); got != c.want {
			t.Errorf("RoundTo(%v, %d) = %v, want %v", c.x, c.n, got, c.want)
		}
	}
}
