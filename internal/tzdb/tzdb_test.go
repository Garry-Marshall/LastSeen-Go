package tzdb

import (
	"archive/zip"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The list must match the time zone database of the Go toolchain building
// the bot; after a Go upgrade adds or removes zones, run:
//
//	go generate ./internal/tzdb
func TestZoneListMatchesGo(t *testing.T) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skipf("go env GOROOT: %v", err)
	}
	zr, err := zip.OpenReader(filepath.Join(strings.TrimSpace(string(out)), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Skipf("Go's zoneinfo.zip: %v", err)
	}
	defer zr.Close()
	var want []string
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() && f.Name != "Factory" {
			want = append(want, f.Name)
		}
	}
	slices.Sort(want)
	if !slices.Equal(allZones, want) {
		t.Errorf("zones_gen.go is out of date (%d zones, Go has %d): run go generate ./internal/tzdb",
			len(allZones), len(want))
	}
}

func TestEveryZoneLoads(t *testing.T) {
	for _, z := range allZones {
		if got := Load(z).String(); got != z {
			t.Errorf("Load(%q) = %q", z, got)
		}
	}
}

func TestValidAndFallback(t *testing.T) {
	for name, want := range map[string]bool{
		"Europe/Brussels": true, "UTC": true, "US/Eastern": true,
		"europe/brussels": false, "Factory": false, "Local": false, "": false, "Mars/Olympus": false,
	} {
		if got := Valid(name); got != want {
			t.Errorf("Valid(%q) = %v, want %v", name, got, want)
		}
	}
	if got := Load("Mars/Olympus").String(); got != "UTC" {
		t.Errorf("Load of an unknown zone = %q, want UTC", got)
	}
}

func TestSuggestions(t *testing.T) {
	got := Suggestions("brus", 5)
	if !slices.Equal(got, []string{"Europe/Brussels"}) {
		t.Errorf("Suggestions(brus) = %v", got)
	}
	if got := Suggestions("america/", 3); len(got) != 3 || !slices.IsSorted(got) {
		t.Errorf("Suggestions(america/, 3) = %v, want 3 sorted names", got)
	}
}
