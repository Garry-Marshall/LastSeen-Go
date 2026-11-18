package db

import (
	"path/filepath"
	"testing"
)

func TestRemoveGuildDataReportsFound(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !d.AddGuild(1, "G", 10) {
		t.Fatal("AddGuild failed")
	}
	if r, ok := d.RemoveGuildData(1); !ok || !r.Found {
		t.Fatalf("first removal = %+v, %v; want found", r, ok)
	}
	// A guild that isn't stored (removed before, or never added).
	if r, ok := d.RemoveGuildData(1); !ok || r.Found {
		t.Fatalf("second removal = %+v, %v; want not found", r, ok)
	}
}
