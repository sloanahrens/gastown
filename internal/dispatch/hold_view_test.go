package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// viewOf is the view the daemon reads for townRoot: the test key, a fixed
// clock and the real town directory listing.
func viewOf(townRoot string, now time.Time) HoldState {
	return holdView(townRoot, testKey, now, listRigESTOPs)
}

// findSource returns the source the view names name, among the town-wide holds
// and the rig ESTOPs.
func findSource(state HoldState, name string) (HoldSource, bool) {
	for _, source := range append(append([]HoldSource(nil), state.Sources...), state.Rigs...) {
		if source.Name == name {
			return source, true
		}
	}
	return HoldSource{}, false
}

// makeUnstatable makes path a symbolic link to itself. Stat fails on it with
// ELOOP: the file is neither present nor missing, so the hold it names cannot be
// ruled out and must fail closed.
func makeUnstatable(t *testing.T, path string) {
	t.Helper()
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
}

// Each hold source, set alone, is named by the view with a reason, and the
// summary is not clear. The town-wide sources also make OperatorHold refuse; a
// rig ESTOP holds its own rig, so OperatorHold stays open. With no hold set the
// summary is clear and OperatorHold allows.
func TestHoldView_OneRowPerHoldSource(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name     string
		set      func(t *testing.T, townRoot string)
		source   string // the name the view gives the source that holds
		townWide bool   // whether OperatorHold refuses dispatch for it
	}{
		{"operator hold file", func(t *testing.T, townRoot string) {
			if err := os.WriteFile(HoldFilePath(townRoot), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "operator hold file", true},
		{"town ESTOP", func(t *testing.T, townRoot string) {
			if err := estop.Activate(townRoot, estop.TriggerManual, "test"); err != nil {
				t.Fatal(err)
			}
		}, "town ESTOP", true},
		{"rig-only ESTOP", func(t *testing.T, townRoot string) {
			if err := estop.ActivateRig(townRoot, "gastown", estop.TriggerManual, "test"); err != nil {
				t.Fatal(err)
			}
		}, "rig ESTOP gastown", false},
		{"boot hold", func(t *testing.T, townRoot string) {
			writeBootHealth(t, townRoot, "waiting for infrastructure\n")
		}, "boot hold", true},
		{"waned Ring", func(t *testing.T, townRoot string) {
			ringtest.Waned(t, townRoot, ringKey, now)
		}, "Ring", true},
		{"no hold", func(*testing.T, string) {}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			tt.set(t, townRoot)
			state := viewOf(townRoot, now)
			summary := state.Summary()
			if tt.source == "" {
				if summary != "clear" {
					t.Errorf("summary with no hold = %q, want clear", summary)
				}
			} else {
				source, ok := findSource(state, tt.source)
				if !ok || source.Reason == "" {
					t.Fatalf("view does not name %q as holding: %+v", tt.source, state)
				}
				if summary == "clear" {
					t.Errorf("summary = clear with %q set, want not clear", tt.source)
				}
			}
			reason := operatorHold(townRoot, testKey, now)
			if tt.townWide && reason == "" {
				t.Errorf("OperatorHold = \"\" with %s set, want it held", tt.name)
			}
			if !tt.townWide && reason != "" {
				t.Errorf("OperatorHold = %q with %s set, want it open: a rig ESTOP holds its rig only", reason, tt.name)
			}
		})
	}
}

// A hold path or ESTOP sentinel that cannot be stat'ed for a reason other than
// absence holds: the summary says so and OperatorHold refuses. A rig ESTOP that
// cannot be read is not clear, but it holds only its rig.
func TestHoldView_UnreadableSourcesFailClosed(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name     string
		path     func(townRoot string) string
		source   string
		want     string
		townWide bool
	}{
		{"unreadable operator hold file", HoldFilePath, "operator hold file", "operator dispatch hold unreadable", true},
		{"unreadable town ESTOP", estop.FilePath, "town ESTOP", "ESTOP unreadable", true},
		{"unreadable rig ESTOP", func(townRoot string) string {
			return estop.RigFilePath(townRoot, "gastown")
		}, "rig ESTOP gastown", "rig ESTOP unreadable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			makeUnstatable(t, tt.path(townRoot))
			state := viewOf(townRoot, now)
			source, ok := findSource(state, tt.source)
			if !ok || !strings.Contains(source.Reason, tt.want) {
				t.Fatalf("view source %q = %+v, want reason naming %q", tt.source, source, tt.want)
			}
			if summary := state.Summary(); summary == "clear" || !strings.Contains(summary, "treating as held") {
				t.Errorf("summary = %q, want a not-clear fail-closed reason", summary)
			}
			reason := operatorHold(townRoot, testKey, now)
			if tt.townWide && !strings.Contains(reason, "treating as held") {
				t.Errorf("OperatorHold = %q, want a fail-closed refusal", reason)
			}
			if !tt.townWide && reason != "" {
				t.Errorf("OperatorHold = %q for an unreadable rig ESTOP, want it open for the town", reason)
			}
		})
	}
}

// A town root that does not exist is its own state, and holds. The view names
// it, and the rig ESTOPs under it are unknown: the listing is not read, so they
// are neither reported as listed nor as clear.
func TestHoldView_MissingTownRootHolds(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "missing")
	calls := 0
	count := func(string) ([]string, error) {
		calls++
		return nil, nil
	}
	now := time.Now()
	state := holdView(townRoot, testKey, now, count)

	source, ok := findSource(state, "town root")
	if !ok || !strings.Contains(source.Reason, "town root missing") {
		t.Fatalf("view = %+v, want the town root named as missing", state)
	}
	if summary := state.Summary(); !strings.HasPrefix(summary, "town root missing") {
		t.Errorf("summary = %q, want the missing town root named first", summary)
	}
	if !strings.Contains(state.RigsNote, "unknown") || len(state.Rigs) != 0 {
		t.Errorf("rig ESTOPs = %+v, note %q; want none listed and the note saying they are unknown", state.Rigs, state.RigsNote)
	}
	if calls != 0 {
		t.Errorf("rig listing called %d times under a missing town root, want 0", calls)
	}
	if reason := operatorHold(townRoot, testKey, now); reason == "" {
		t.Error("OperatorHold on a missing town root = \"\", want it held")
	}
}

// The rig ESTOP listing reads the town directory once per view, however many
// rig ESTOPs the town holds.
func TestHoldView_ListsRigEstopsOncePerView(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigs := []string{"gastown", "om", "beads"}
	for _, rig := range rigs {
		if err := estop.ActivateRig(townRoot, rig, estop.TriggerManual, "test"); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	list := func(root string) ([]string, error) {
		calls++
		return listRigESTOPs(root)
	}
	state := holdView(townRoot, testKey, time.Now(), list)
	if calls != 1 {
		t.Errorf("rig listing called %d times for one view, want 1", calls)
	}
	if len(state.Rigs) != len(rigs) {
		t.Errorf("view names %d rig ESTOPs, want %d: %+v", len(state.Rigs), len(rigs), state.Rigs)
	}
}

// listRigESTOPs names the rigs that have an ESTOP.<rig> file in the town
// directory, and nothing else. A town directory that cannot be read is an error,
// not an empty listing, and the view then says the rig ESTOPs are unlisted.
func TestListRigESTOPs(t *testing.T) {
	t.Parallel()
	t.Run("names each ESTOP.<rig> file only", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		for _, name := range []string{"ESTOP", "ESTOP.gastown", "ESTOP.om", "ESTOP.", "seat-refill.hold"} {
			if err := os.WriteFile(filepath.Join(townRoot, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rigs, err := listRigESTOPs(townRoot)
		if err != nil {
			t.Fatalf("listRigESTOPs: %v", err)
		}
		slices.Sort(rigs)
		if want := []string{"gastown", "om"}; !slices.Equal(rigs, want) {
			t.Errorf("listRigESTOPs = %q, want %q", rigs, want)
		}
	})
	t.Run("an unreadable town directory is an error", func(t *testing.T) {
		t.Parallel()
		// A regular file in place of the town root makes ReadDir fail with
		// ENOTDIR, which is not an empty town.
		townRoot := filepath.Join(t.TempDir(), "town")
		if err := os.WriteFile(townRoot, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := listRigESTOPs(townRoot); err == nil {
			t.Error("listRigESTOPs on an unreadable town directory = nil error, want one")
		}
		if state := holdView(townRoot, testKey, time.Now(), listRigESTOPs); !strings.Contains(state.RigsNote, "rig ESTOPs unlisted") {
			t.Errorf("RigsNote = %q, want the rig ESTOPs named as unlisted", state.RigsNote)
		}
	})
}

// A rig listing that cannot be read leaves the rig ESTOPs unknown, so the
// summary is not clear even though no town-wide hold holds.
func TestHoldView_UnlistedRigsAreNotClear(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	list := func(string) ([]string, error) { return nil, errors.New("denied") }
	state := holdView(townRoot, testKey, time.Now(), list)
	if state.Held() != "" {
		t.Fatalf("Held() = %q with no town-wide hold, want \"\"", state.Held())
	}
	if summary := state.Summary(); summary == "clear" || !strings.Contains(summary, "denied") {
		t.Errorf("summary = %q, want it not clear and naming the listing error", summary)
	}
}
