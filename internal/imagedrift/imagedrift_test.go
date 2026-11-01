package imagedrift

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func storeIn(t *testing.T) Store {
	t.Helper()
	return StoreIn(filepath.Join(t.TempDir(), "run", FileName))
}

func TestHeldForTheRefusedAgentWithinTheHold(t *testing.T) {
	t.Parallel()
	s := storeIn(t)
	if err := s.Write(t0, "sbx-flash"); err != nil {
		t.Fatalf("%s: Write: %v", t.Name(), err)
	}
	for _, tc := range []struct {
		name  string
		now   time.Time
		agent string
		want  bool
	}{
		{"just after", t0.Add(time.Second), "sbx-flash", true},
		{"just inside the hold", t0.Add(HoldFor - time.Second), "sbx-flash", true},
		{"at the hold", t0.Add(HoldFor), "sbx-flash", false},
		{"another agent", t0.Add(time.Second), "sbx-haiku", false},
		{"a marker from the future beyond the skew", t0.Add(-2 * FutureSkew), "sbx-flash", false},
		{"a marker slightly ahead", t0.Add(-FutureSkew / 2), "sbx-flash", true},
	} {
		got, _, err := s.Held(tc.now, tc.agent)
		if err != nil || got != tc.want {
			t.Errorf("%s: %s: held = %v err %v, want %v", t.Name(), tc.name, got, err, tc.want)
		}
	}
}

func TestZeroStoreAndMissingFileHoldNothing(t *testing.T) {
	t.Parallel()
	if held, _, err := (Store{}).Held(t0, "a"); held || err != nil {
		t.Errorf("%s: zero store held %v err %v", t.Name(), held, err)
	}
	if err := (Store{}).Write(t0, "a"); err != nil {
		t.Errorf("%s: zero store Write: %v", t.Name(), err)
	}
	if err := (Store{}).ClearFor("a"); err != nil {
		t.Errorf("%s: zero store ClearFor: %v", t.Name(), err)
	}
	s := storeIn(t)
	if held, _, err := s.Held(t0, "a"); held || err != nil {
		t.Errorf("%s: missing file held %v err %v", t.Name(), held, err)
	}
	if err := s.ClearFor("a"); err != nil {
		t.Errorf("%s: ClearFor on a missing file: %v", t.Name(), err)
	}
}

func TestWriteNeedsAnAgent(t *testing.T) {
	t.Parallel()
	if err := storeIn(t).Write(t0, ""); err == nil {
		t.Errorf("%s: Write without an agent returned no error", t.Name())
	}
}

func TestUnreadableMarkerIsAnErrorAndClearRepairsIt(t *testing.T) {
	t.Parallel()
	s := storeIn(t)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		t.Fatalf("%s: %v", t.Name(), err)
	}
	if err := os.WriteFile(s.path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("%s: %v", t.Name(), err)
	}
	if held, _, err := s.Held(t0, "a"); held || err == nil {
		t.Errorf("%s: a broken marker: held %v err %v, want an error", t.Name(), held, err)
	}
	if err := s.ClearFor("a"); err != nil {
		t.Fatalf("%s: ClearFor: %v", t.Name(), err)
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Errorf("%s: the broken marker is still there: %v", t.Name(), err)
	}
}

func TestClearForRemovesOnlyTheAgentsOwnMarker(t *testing.T) {
	t.Parallel()
	s := storeIn(t)
	if err := s.Write(t0, "sbx-flash"); err != nil {
		t.Fatalf("%s: Write: %v", t.Name(), err)
	}
	if err := s.ClearFor("sbx-haiku"); err != nil {
		t.Fatalf("%s: ClearFor other: %v", t.Name(), err)
	}
	if held, _, _ := s.Held(t0, "sbx-flash"); !held {
		t.Errorf("%s: another agent's success cleared the marker", t.Name())
	}
	if err := s.ClearFor("sbx-flash"); err != nil {
		t.Fatalf("%s: ClearFor own: %v", t.Name(), err)
	}
	if held, _, _ := s.Held(t0, "sbx-flash"); held {
		t.Errorf("%s: the agent's own success left the marker", t.Name())
	}
}

// A marker path that cannot be read as a file is an error from both readers,
// and a removal that fails is returned.
func TestReadAndRemoveFailuresAreReturned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, FileName)
	if err := os.MkdirAll(filepath.Join(marker, "inside"), 0o755); err != nil {
		t.Fatalf("%s: %v", t.Name(), err)
	}
	s := StoreIn(marker)
	if _, _, err := s.Held(t0, "a"); err == nil {
		t.Errorf("%s: Held on a directory returned no error", t.Name())
	}
	if err := s.ClearFor("a"); err == nil {
		t.Errorf("%s: ClearFor on a directory returned no error", t.Name())
	}
	if err := s.remove(); err == nil {
		t.Errorf("%s: remove of a non-empty directory returned no error", t.Name())
	}
}
