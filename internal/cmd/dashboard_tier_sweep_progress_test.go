package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// tierSweepProgressGolden is the record the runner writes with integration done and race running.
const tierSweepProgressGolden = `{"rig":"gastown","sha":"6193c7a7","tiers":[{"tier":"integration","verdict":"GREEN","passed":82,"failed":0,"skipped":0,"secs":1129}],"running":"race"}`

func tierSweepProgressFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTierSweepProgressGoldenRecord(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(tierSweepProgressFile(t, tierSweepProgressGolden))
	want := TierSweepProgress{Rig: "gastown", SHA: "6193c7a7", Running: "race",
		Tiers: []dashboard.TierSweepRunStage{{Tier: "integration", Verdict: "GREEN", Passed: 82, Secs: 1129}}}
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("readTierSweepProgress(golden) = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
}

func TestReadTierSweepProgressMissingFileIsNoProgress(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(filepath.Join(t.TempDir(), "progress.json"))
	if err != nil || ok || !reflect.DeepEqual(got, TierSweepProgress{}) {
		t.Fatalf("readTierSweepProgress(missing) = %+v, %v, %v; want zero, false, nil", got, ok, err)
	}
}

func TestReadTierSweepProgressEmptyTiersList(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(tierSweepProgressFile(t, `{"rig":"gastown","sha":"6193c7a7","tiers":[],"running":"integration"}`))
	if err != nil || !ok || len(got.Tiers) != 0 || got.Running != "integration" {
		t.Fatalf("readTierSweepProgress(empty tiers) = %+v, %v, %v; want no tiers, running integration, true, nil", got, ok, err)
	}
}

// TestReadTierSweepProgressRefusesUnreadableFile: an existing but unreadable record is an error naming its path.
func TestReadTierSweepProgressRefusesUnreadableFile(t *testing.T) {
	t.Parallel()
	dirAtPath := filepath.Join(t.TempDir(), "progress.json")
	if err := os.Mkdir(dirAtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"corrupt JSON":      tierSweepProgressFile(t, `{"rig":"gastown","tiers":[`),
		"empty file":        tierSweepProgressFile(t, ""),
		"directory at path": dirAtPath,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok, err := readTierSweepProgress(path)
			if err == nil || ok || !strings.Contains(err.Error(), path) {
				t.Errorf("readTierSweepProgress() ok = %v, err = %v; want false and an error naming %s", ok, err, path)
			}
		})
	}
}

// sweepRunFixture is one run directory under a rig's sweep-runs directory: its name,
// whether it holds a progress.json, and how long before sweepRunsTestClock that file
// was last written.
type sweepRunFixture struct {
	name       string
	progress   bool
	agoMinutes int
}

var sweepRunsTestClock = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// sweepRunsRootWith builds the sweep-runs root at root, holding runs under rig.
func sweepRunsRootWith(t *testing.T, root, rig string, runs []sweepRunFixture) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		dir := filepath.Join(root, rig, r.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if !r.progress {
			continue
		}
		path := filepath.Join(dir, "progress.json")
		if err := os.WriteFile(path, []byte(tierSweepProgressGolden), 0o644); err != nil {
			t.Fatal(err)
		}
		at := sweepRunsTestClock.Add(-time.Duration(r.agoMinutes) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindSweepProgressNewestMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		runs    []sweepRunFixture
		sha8    string
		wantRun string // the run directory the match must be in; "" for no match
	}{
		// The literal run name is copied from tierSweepRunnerRunID (package daemon,
		// unexported; no exported seam exists to check it against, and none is built
		// here) and is not checked against it, so the row changes when the daemon's
		// run name does.
		{"one match", []sweepRunFixture{{"gastown-6193c7a7-integration-race", true, 5}}, "6193c7a7", "gastown-6193c7a7-integration-race"},
		{"two matches take the newer", []sweepRunFixture{
			{"gastown-6193c7a7-integration-race", true, 90},
			{"gastown-6193c7a7-integration", true, 5},
		}, "6193c7a7", "gastown-6193c7a7-integration"},
		{"another sha8 is not matched", []sweepRunFixture{{"gastown-6193c7a8-integration-race", true, 5}}, "6193c7a7", ""},
		{"a sha8 prefix is not matched", []sweepRunFixture{{"gastown-6193c7a7x-integration-race", true, 5}}, "6193c7a7", ""},
		{"a run with no progress.json is not matched", []sweepRunFixture{{"gastown-6193c7a7-integration-race", false, 0}}, "6193c7a7", ""},
		{"no rig directory under the root", nil, "6193c7a7", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := sweepRunsRootWith(t, t.TempDir(), "gastown", tc.runs)
			got, ok, err := findSweepProgress(root, "gastown", tc.sha8)
			if tc.wantRun == "" {
				if got != "" || ok || err != nil {
					t.Fatalf("findSweepProgress() = %q, %v, %v; want \"\", false, nil", got, ok, err)
				}
				return
			}
			want := filepath.Join(root, "gastown", tc.wantRun, "progress.json")
			if err != nil || !ok || got != want {
				t.Fatalf("findSweepProgress() = %q, %v, %v; want %q, true, nil", got, ok, err, want)
			}
		})
	}
}

// TestFindSweepProgressFindsRunUnderGlobRoot: a root whose name holds [ and * is read as a path, not a pattern.
func TestFindSweepProgressFindsRunUnderGlobRoot(t *testing.T) {
	t.Parallel()
	root := sweepRunsRootWith(t, filepath.Join(t.TempDir(), "[sweep*]"), "gastown",
		[]sweepRunFixture{{"gastown-6193c7a7-integration-race", true, 5}})
	got, ok, err := findSweepProgress(root, "gastown", "6193c7a7")
	want := filepath.Join(root, "gastown", "gastown-6193c7a7-integration-race", "progress.json")
	if err != nil || !ok || got != want {
		t.Fatalf("findSweepProgress() = %q, %v, %v; want %q, true, nil", got, ok, err, want)
	}
}

func TestFindSweepProgressRefuses(t *testing.T) {
	t.Parallel()
	// danglingProgress builds a run whose progress.json is a symlink to nothing: the entry
	// exists, and the stat that orders the matches fails.
	danglingProgress := func(t *testing.T) string {
		root := sweepRunsRootWith(t, t.TempDir(), "gastown", []sweepRunFixture{{"gastown-6193c7a7-integration-race", false, 0}})
		link := filepath.Join(root, "gastown", "gastown-6193c7a7-integration-race", "progress.json")
		if err := os.Symlink(filepath.Join(root, "missing.json"), link); err != nil {
			t.Fatal(err)
		}
		return root
	}
	// unreadableRig makes the rig directory unreadable; the cleanup restores it so t.TempDir can be removed.
	unreadableRig := func(t *testing.T) string {
		root := sweepRunsRootWith(t, t.TempDir(), "gastown", []sweepRunFixture{{"gastown-6193c7a7-integration-race", true, 5}})
		rigDir := filepath.Join(root, "gastown")
		info, err := os.Stat(rigDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rigDir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(rigDir, info.Mode().Perm()); err != nil {
				t.Error(err)
			}
		})
		return root
	}
	cases := map[string]struct {
		root func(t *testing.T) string
		rig  string
		sha8 string
	}{
		"missing root":                             {func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, "gastown", "6193c7a7"},
		"unreadable rig directory":                 {unreadableRig, "gastown", "6193c7a7"},
		"rig that climbs out of the root":          {func(t *testing.T) string { return t.TempDir() }, "../gastown", "6193c7a7"},
		"empty sha8":                               {func(t *testing.T) string { return t.TempDir() }, "gastown", ""},
		"run whose progress.json cannot be stat'd": {danglingProgress, "gastown", "6193c7a7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tc.root(t)
			got, ok, err := findSweepProgress(root, tc.rig, tc.sha8)
			if err == nil || ok || got != "" {
				t.Errorf("findSweepProgress(%q, %q, %q) = %q, %v, %v; want an error", root, tc.rig, tc.sha8, got, ok, err)
			}
		})
	}
}
