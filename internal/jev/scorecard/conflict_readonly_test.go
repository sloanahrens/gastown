package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/land"
)

// OpenReadOnly has a read-only form for the conflict battery only; any other
// battery is refused rather than opened with a fetch.
func TestOpenReadOnlyRefusesAnotherBattery(t *testing.T) {
	t.Parallel()
	if _, err := OpenReadOnly("drift", t.TempDir()); err == nil {
		t.Fatal("a battery with no read-only form was opened")
	}
}

// A read-only label reads the clone it is given. When that clone cannot open,
// the label fails rather than deciding the pair from nothing.
func TestConflictReadOnlyLabelFailsWhenTheCloneCannotOpen(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeReadOnlyLandings(t, town,
		land.LandingRecord{BeadID: "x", Rig: conflict.RigName, Base: readOnlyBase, Head: readOnlyHead, LandedCommit: readOnlyHead, LandedAt: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)},
		land.LandingRecord{BeadID: "y", Rig: conflict.RigName, Base: readOnlyBase, Head: readOnlyHead, LandedCommit: readOnlyHead, LandedAt: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)},
	)
	a := NewConflictReadOnly(town, func() (ConflictRepo, error) {
		return nil, errors.New("no rig registry")
	})

	item := Item{ID: "x+y", Refs: map[string]string{refConflictBead: "x", refConflictOther: "y"}}
	if _, err := a.Label(context.Background(), item); err == nil {
		t.Fatal("a clone that cannot open labeled the pair")
	}
}

// Commit ids the read-only label test names. The clone is never asked about
// them, since it cannot open.
const (
	readOnlyBase = "1111111111111111111111111111111111111111"
	readOnlyHead = "2222222222222222222222222222222222222222"
)

// writeReadOnlyLandings writes the rig's landing records, one line each.
func writeReadOnlyLandings(t *testing.T, town string, recs ...land.LandingRecord) {
	t.Helper()
	file, err := land.RigLandingsFile(town, conflict.RigName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range recs {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(file.Path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
