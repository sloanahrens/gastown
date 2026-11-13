// Package landtest holds the fixture for a rig's landing log. The pool's
// occupancy, the patrol's seat cap and the polecat classifier all read the
// log, so the tests of all three write it through this package rather than a
// copy each.
//
// It is a fixture: nothing outside a test imports it.
package landtest

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// WriteLandingLog writes recs, in order, to rig's landing log under townRoot,
// through the writer the landing worker uses, so a reader under test sees the
// production file shape. No recs writes no file.
//
// Records append, so a caller that writes twice to one town gets both sets.
func WriteLandingLog(t testing.TB, townRoot, rig string, recs ...land.LandingRecord) {
	t.Helper()
	if len(recs) == 0 {
		return
	}
	lf, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	for _, rec := range recs {
		if err := lf.Append(rec); err != nil {
			t.Fatalf("appending a landing record: %v", err)
		}
	}
}

// MakeLandingLogUnreadable puts a directory where rig's landing log goes, so a
// reader of the log fails instead of finding no record.
func MakeLandingLogUnreadable(t testing.TB, townRoot, rig string) {
	t.Helper()
	lf, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	if err := os.MkdirAll(lf.Path, 0o700); err != nil {
		t.Fatalf("making the landing log unreadable: %v", err)
	}
}
