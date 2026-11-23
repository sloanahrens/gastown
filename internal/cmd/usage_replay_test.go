package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

func replayDeps(town, dir string, store usageattrib.ReplayStore) usageReplayDeps {
	return usageReplayDeps{
		townRoot:  func() (string, error) { return town, nil },
		usageDir:  func(string) string { return dir },
		openStore: func(string) (usageattrib.ReplayStore, error) { return store, nil },
	}
}

func replayRecFile(t *testing.T, dir, bead string) {
	t.Helper()
	rec := dashboard.UsageRecord{Bead: bead, Kind: dashboard.UsageKindWork, Workspace: "ws-a",
		End: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	if err := usageattrib.Write(dir, rec, []string{"s-" + bead}); err != nil {
		t.Fatal(err)
	}
}

// replayRun runs `gt usage replay <args>` and returns stdout, stderr and the exit code.
func replayRun(t *testing.T, deps usageReplayDeps, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := newUsageReplayCmd(deps)
	c.SetArgs(args)
	c.SetOut(&out)
	c.SetErr(&errOut)
	code, _ := CodeOfErr(t, c)
	return out.String(), errOut.String(), code
}

func replayRows(t *testing.T, store *beadsfake.Fake) int {
	t.Helper()
	rows, err := store.ReadUsageRecords(t.Context(), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestUsageReplayDryRunThenApply(t *testing.T) {
	t.Parallel()
	dir, store := t.TempDir(), beadsfake.New()
	replayRecFile(t, dir, "gt-a")
	replayRecFile(t, dir, "gt-b")
	deps := replayDeps(t.TempDir(), dir, store)
	out, _, code := replayRun(t, deps)
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "would write: 2") {
		t.Fatalf("dry run exit %d, stdout %q; want a dry run naming 2 rows", code, out)
	}
	if n := replayRows(t, store); n != 0 {
		t.Fatalf("the dry run wrote %d rows, want none", n)
	}
	out, _, code = replayRun(t, deps, "--apply")
	if code != 0 || !strings.Contains(out, "files: 2") || !strings.Contains(out, "written: 2") {
		t.Fatalf("apply exit %d, stdout %q; want 2 files and 2 written", code, out)
	}
	if n := replayRows(t, store); n != 2 {
		t.Fatalf("apply wrote %d rows, want 2", n)
	}
}

func TestUsageReplayFailedFilePrintsItsLineAndExits1(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	replayRecFile(t, dir, "gt-ok")
	bad := usageattrib.WorkRecordPath(dir, "gt-bad")
	writeAttribFile(t, bad, "not a record")
	out, errOut, code := replayRun(t, replayDeps(t.TempDir(), dir, beadsfake.New()))
	if code != 1 || !strings.Contains(out, "failed: 1") || !strings.Contains(errOut, bad) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming %s", code, out, errOut, bad)
	}
}

func TestUsageReplayHelpNamesTheDryRunDefault(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	c := newUsageReplayCmd(replayDeps(t.TempDir(), t.TempDir(), beadsfake.New()))
	c.SetOut(&out)
	if err := c.Help(); err != nil {
		t.Fatal(err)
	}
	if help := strings.ToLower(out.String()); !strings.Contains(help, "dry run") || !strings.Contains(help, "apply") {
		t.Errorf("help = %q, want the dry-run default and --apply", out.String())
	}
}

func TestUsageReplayUsesTheTownStoreFromAnyDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	replayRecFile(t, filepath.Join(constants.TownRuntimePath(town), "usage"), "gt-a")
	deps := defaultUsageReplayDeps()
	deps.townRoot = func() (string, error) { return town, nil }
	var from string
	deps.openStore = func(root string) (usageattrib.ReplayStore, error) { from = root; return beadsfake.New(), nil }
	if _, _, code := replayRun(t, deps); code != 0 || from != town {
		t.Fatalf("exit %d from store root %q; want 0 and the town root %q", code, from, town)
	}
}

func TestUsageReplayRunCouldNotBeMade(t *testing.T) {
	t.Parallel()
	errTown, errStore := errors.New("no town here"), errors.New("dolt is down")
	cases := []struct {
		name, want string
		mutate     func(*testing.T, *usageReplayDeps)
	}{
		{"town not found", "finding the town", func(_ *testing.T, d *usageReplayDeps) { d.townRoot = func() (string, error) { return "", errTown } }},
		{"store cannot open", "opening the usage_records store", func(_ *testing.T, d *usageReplayDeps) {
			d.openStore = func(string) (usageattrib.ReplayStore, error) { return nil, errStore }
		}},
		{"records directory missing", "records directory", func(t *testing.T, d *usageReplayDeps) {
			d.usageDir = func(string) string { return filepath.Join(t.TempDir(), "missing") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := replayDeps(t.TempDir(), t.TempDir(), beadsfake.New())
			tc.mutate(t, &deps)
			if _, errOut, code := replayRun(t, deps); code != 2 || !strings.Contains(errOut, tc.want) {
				t.Fatalf("exit %d, stderr %q; want exit 2 naming %q", code, errOut, tc.want)
			}
		})
	}
}
