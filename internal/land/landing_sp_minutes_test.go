package land

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLandingRecordSPMinutesWritten: a landing writes the minutes its sp:
// estimate was computed as and the reason the bead was slung to the landings
// file, and leaves both out of the bead's LANDING RECORD block (gt-iwzoje).
func TestLandingRecordSPMinutesWritten(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	minutes := 10.8
	f.work.SPMinutes = &minutes
	f.work.SPSlung = "rework"
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	line := f.landingLines()[0]
	var rec LandingRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SPMinutes == nil || *rec.SPMinutes != minutes {
		t.Errorf("landings file sp_minutes = %v, want %v: %s", rec.SPMinutes, minutes, line)
	}
	if rec.SPSlung != "rework" {
		t.Errorf("landings file sp_slung = %q, want rework: %s", rec.SPSlung, line)
	}
	bead, err := f.bd.Show(f.work.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bead.Notes, "sp_minutes") || strings.Contains(bead.Notes, "sp_slung") {
		t.Errorf("LANDING RECORD block carries the landings-file-only sp fields:\n%s", bead.Notes)
	}
}

// TestLandingRecordSPMinutesOmitted: a landing with neither the minutes nor the
// slung reason writes no sp_minutes or sp_slung key, and a landings line
// written before these fields existed still reads back with them absent
// (gt-iwzoje).
func TestLandingRecordSPMinutesOmitted(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	line := f.landingLines()[0]
	if strings.Contains(line, `"sp_minutes"`) {
		t.Errorf("landings line has an sp_minutes key with none set: %s", line)
	}
	if strings.Contains(line, `"sp_slung"`) {
		t.Errorf("landings line has an sp_slung key with none set: %s", line)
	}

	// An old landings line, written before sp_minutes and sp_slung existed,
	// still reads; the absent keys are not an error and read as none.
	old := `{"bead":"gt-old","rig":"gastown","head":"h0","landed_commit":"c0"}` + "\n"
	lf, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(lf.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lf.Path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := lf.Find("gt-old", "h0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("an old landings line did not read back")
	}
	if rec.SPMinutes != nil || rec.SPSlung != "" {
		t.Errorf("old line read sp_minutes = %v, sp_slung = %q, want none", rec.SPMinutes, rec.SPSlung)
	}
}
