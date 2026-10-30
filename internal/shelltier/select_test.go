package shelltier

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// fakeGit is the repository the due decision reads: the paths that changed
// since a record's commit, or the error the diff reports.
type fakeGit struct {
	changed []string
	err     error
}

func (g fakeGit) DiffNameOnly(_, _ string) ([]string, error) { return g.changed, g.err }

func (g fakeGit) Worktree() ([]string, error) { return nil, nil }

// noRunScript is an entry whose StubsGo names a reason, so only its own
// triggers decide whether it runs, not the Go tree.
func noRunScript(path string, triggers ...string) Entry {
	return Entry{Path: path, Triggers: triggers, StubsGo: "none: names no go command"}
}

func utcAt(d, h, m int) time.Time {
	return time.Date(2026, time.October, d, h, m, 0, 0, time.UTC)
}

func paths(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

func TestSelectRunsAnEntryWithNoRecord(t *testing.T) {
	t.Parallel()
	entries := []Entry{noRunScript("scripts/a_test.sh"), noRunScript("scripts/b_test.sh")}
	run, skipped, unreadable := Select(entries, tiertrigger.State{}, fakeGit{}, utcAt(29, 12, 0), time.UTC)
	if got := paths(run); !slices.Equal(got, []string{"scripts/a_test.sh", "scripts/b_test.sh"}) {
		t.Fatalf("run = %v, want both entries", got)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", paths(skipped))
	}
	if len(unreadable) != 0 {
		t.Fatalf("unreadable = %v, want none", unreadable)
	}
}

func TestSelectRunsWhenATriggerPathChanged(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	st.Ran(entry.Path, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	g := fakeGit{changed: []string{"scripts/install-gt.sh"}}
	run, skipped, _ := Select([]Entry{entry}, st, g, utcAt(29, 12, 0), time.UTC)
	if got := paths(run); !slices.Equal(got, []string{entry.Path}) {
		t.Fatalf("run = %v, want %s", got, entry.Path)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", paths(skipped))
	}
}

func TestSelectSkipsWhenOnlyUnrelatedPathsChanged(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	st.Ran(entry.Path, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	g := fakeGit{changed: []string{"docs/README.md", "scripts/other.sh"}}
	run, skipped, unreadable := Select([]Entry{entry}, st, g, utcAt(29, 12, 0), time.UTC)
	if len(run) != 0 {
		t.Fatalf("run = %v, want none", paths(run))
	}
	if got := paths(skipped); !slices.Equal(got, []string{entry.Path}) {
		t.Fatalf("skipped = %v, want %s", got, entry.Path)
	}
	if len(unreadable) != 0 {
		t.Fatalf("unreadable = %v, want none", unreadable)
	}
}

func TestSelectRunsWhenTheDailyWindowHasPassed(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	// The last green run was yesterday, before the 03:30 window, and nothing changed since.
	st.Ran(entry.Path, "1111111111111111111111111111111111111111", utcAt(28, 9, 0))
	run, skipped, _ := Select([]Entry{entry}, st, fakeGit{}, utcAt(29, 4, 0), time.UTC)
	if got := paths(run); !slices.Equal(got, []string{entry.Path}) {
		t.Fatalf("run = %v, want %s", got, entry.Path)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", paths(skipped))
	}
}

func TestSelectRunsAndReportsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	st.Ran(entry.Path, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	diffErr := errors.New("bad object")
	g := fakeGit{err: diffErr}
	run, skipped, unreadable := Select([]Entry{entry}, st, g, utcAt(29, 12, 0), time.UTC)
	if got := paths(run); !slices.Equal(got, []string{entry.Path}) {
		t.Fatalf("run = %v, want %s", got, entry.Path)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", paths(skipped))
	}
	if err, ok := unreadable[entry.Path]; !ok || !errors.Is(err, diffErr) {
		t.Fatalf("unreadable[%s] = %v, want the diff error", entry.Path, err)
	}
}

func TestSelectSkipsARecordedRunUntilATriggerChanges(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	Record(st, entry, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	run, skipped, _ := Select([]Entry{entry}, st, fakeGit{changed: []string{"docs/README.md"}}, utcAt(29, 12, 0), time.UTC)
	if len(run) != 0 {
		t.Fatalf("run = %v, want none after a recorded green run", paths(run))
	}
	if got := paths(skipped); !slices.Equal(got, []string{entry.Path}) {
		t.Fatalf("skipped = %v, want %s", got, entry.Path)
	}
}

func TestSelectRunsOnlyTheUnrecordedEntryInAMixedSweep(t *testing.T) {
	t.Parallel()
	recorded := noRunScript("scripts/recorded_test.sh")
	fresh := noRunScript("scripts/fresh_test.sh")
	st := tiertrigger.State{}
	Record(st, recorded, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	run, skipped, unreadable := Select([]Entry{recorded, fresh}, st, fakeGit{}, utcAt(29, 12, 0), time.UTC)
	if got := paths(run); !slices.Equal(got, []string{fresh.Path}) {
		t.Fatalf("run = %v, want %s", got, fresh.Path)
	}
	if got := paths(skipped); !slices.Equal(got, []string{recorded.Path}) {
		t.Fatalf("skipped = %v, want %s", got, recorded.Path)
	}
	if len(unreadable) != 0 {
		t.Fatalf("unreadable = %v, want none", unreadable)
	}
}

func TestSelectPanicsOnNilLocWhenAnEntryHasARecord(t *testing.T) {
	t.Parallel()
	entry := noRunScript("scripts/install-gt_test.sh", "scripts/install-gt.sh")
	st := tiertrigger.State{}
	Record(st, entry, "1111111111111111111111111111111111111111", utcAt(29, 9, 0))
	defer func() {
		if recover() == nil {
			t.Fatal("Select with a nil loc returned, want a panic")
		}
	}()
	Select([]Entry{entry}, st, fakeGit{}, utcAt(29, 12, 0), nil)
}

func TestStatePathIsSeparateFromTheContainerState(t *testing.T) {
	t.Parallel()
	const town = "/town"
	got := StatePath(town)
	if base := filepath.Base(got); base != "shell-state.json" {
		t.Fatalf("StatePath base = %q, want shell-state.json", base)
	}
	container := tiertrigger.StatePath(town)
	if got == container {
		t.Fatalf("StatePath = %q is the container state path", got)
	}
	if filepath.Dir(got) != filepath.Dir(container) {
		t.Fatalf("StatePath dir = %q, want the container state dir %q", filepath.Dir(got), filepath.Dir(container))
	}
}
