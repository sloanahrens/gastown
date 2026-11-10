package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// churnMismatch is the verify mismatch for rel with reason.
func churnMismatch(reason, rel string) townstate.Mismatch {
	return townstate.Mismatch{Path: rel, Reason: reason}
}

// statAt is a stat seam that finds every host file, with modification time at.
func statAt(at time.Time) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		return townStateInfo{name: filepath.Base(p), mtime: at}, nil
	}
}

// statMissing is a stat seam that finds no host file.
func statMissing(p string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
}

// statFails is a stat seam whose every stat fails for a reason other than absence.
func statFails(p string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrPermission}
}

// TestSeedChurnSortTable: a rehearsal's verify mismatch is churn when the running
// town made it while the copy ran, and a problem otherwise. One row is one mismatch.
func TestSeedChurnSortTable(t *testing.T) {
	t.Parallel()
	const town = "/town"
	start := time.Date(2026, 11, 9, 6, 40, 0, 0, time.UTC)
	before := start.Add(-time.Hour)
	after := start.Add(time.Hour)
	cases := []struct {
		name  string
		m     townstate.Mismatch
		stat  func(string) (fs.FileInfo, error)
		churn bool
	}{
		{
			name:  "a changed host file newer than the copy window is churn",
			m:     churnMismatch(townstate.ReasonChanged, "gastown/src/main.go"),
			stat:  statAt(after),
			churn: true,
		},
		{
			name:  "an extra host file newer than the copy window is churn",
			m:     churnMismatch(townstate.ReasonExtra, "gastown/crew/new.md"),
			stat:  statAt(after),
			churn: true,
		},
		{
			name:  "a ref file deleted from the host under a rig's bare repository is churn",
			m:     churnMismatch(townstate.ReasonExtra, "gastown/.repo.git/refs/heads/feature"),
			stat:  statMissing,
			churn: true,
		},
		{
			name:  "a reflog under a rig's bare repository is churn",
			m:     churnMismatch(townstate.ReasonMissing, "gastown/.repo.git/logs/HEAD"),
			stat:  statMissing,
			churn: true,
		},
		{
			name:  "a new worktree under a rig's bare repository is churn",
			m:     churnMismatch(townstate.ReasonExtra, "gastown/.repo.git/worktrees/a/HEAD"),
			stat:  statMissing,
			churn: true,
		},
		{
			name:  "the open-bead baseline is churn",
			m:     churnMismatch(townstate.ReasonChanged, "daemon/wisp_open_baseline_daemon.json"),
			stat:  statAt(before),
			churn: true,
		},
		{
			name:  "last-touched is churn at any depth",
			m:     churnMismatch(townstate.ReasonChanged, "gastown/last-touched"),
			stat:  statAt(before),
			churn: true,
		},
		{
			name:  "FETCH_HEAD under a rig's bare repository is churn",
			m:     churnMismatch(townstate.ReasonMissing, "gastown/.repo.git/FETCH_HEAD"),
			stat:  statMissing,
			churn: true,
		},
		{
			name:  "an older file that looks unchanged is a problem",
			m:     churnMismatch(townstate.ReasonChanged, "settings/config.json"),
			stat:  statAt(before),
			churn: false,
		},
		{
			name:  "an unrelated path missing from the host is a problem",
			m:     churnMismatch(townstate.ReasonMissing, "daemon/maintenance_state.json"),
			stat:  statMissing,
			churn: false,
		},
		{
			name:  "a changed file whose stat fails is a problem",
			m:     churnMismatch(townstate.ReasonChanged, "crew/notes.md"),
			stat:  statFails,
			churn: false,
		},
		{
			name:  "a ref file whose stat fails is a problem",
			m:     churnMismatch(townstate.ReasonChanged, "gastown/.repo.git/refs/heads/main"),
			stat:  statFails,
			churn: false,
		},
		{
			name:  "a path under a rig's bare repository outside refs, logs and worktrees is a problem",
			m:     churnMismatch(townstate.ReasonExtra, "gastown/.repo.git/objects/ab/cd"),
			stat:  statMissing,
			churn: false,
		},
		{
			name:  "a refs directory outside a bare repository is a problem",
			m:     churnMismatch(townstate.ReasonExtra, "gastown/refs/heads/x"),
			stat:  statMissing,
			churn: false,
		},
		{
			name:  "a claim name that climbs out of the town is a problem even when newer than the copy window",
			m:     churnMismatch(townstate.ReasonExtra, "../outside/recent.json"),
			stat:  statAt(after),
			churn: false,
		},
		{
			name:  "a non-regular entry is a problem even when newer than the copy window",
			m:     churnMismatch(townstate.ReasonNotRegular, "gastown/last-touched"),
			stat:  statAt(after),
			churn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			churn, problems := sortSeedChurn(town, start, []townstate.Mismatch{tc.m}, tc.stat)
			if got := len(churn) == 1; got != tc.churn {
				t.Errorf("churn = %v, want %v (churn %v, problems %v)", got, tc.churn, churn, problems)
			}
			if len(churn)+len(problems) != 1 {
				t.Errorf("the mismatch was sorted into %d churn and %d problems, want exactly one", len(churn), len(problems))
			}
		})
	}
}

// hostStat is the stat seam a churn run injects. Absence is the real tree's. A host
// file named in newer, by its town-relative path, was written after the copy window
// opened; every other host file is older than it.
func hostStat(town string, copyStart time.Time, newer ...string) func(string) (fs.FileInfo, error) {
	later := make(map[string]bool, len(newer))
	for _, rel := range newer {
		later[filepath.Join(town, filepath.FromSlash(rel))] = true
	}
	return func(p string) (fs.FileInfo, error) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		at := copyStart.Add(-time.Hour)
		if later[p] {
			at = copyStart.Add(time.Hour)
		}
		return townStateInfo{name: info.Name(), dir: info.IsDir(), mtime: at}, nil
	}
}

// churnListing makes the pod's claim differ from the host the way a copy taken while
// the town runs does, when the listing runs: the daemon's maintenance state changed
// on the host after the copy opened, so the claim holds the older bytes, and a ref
// was created under rig's bare repository after the copy, so the claim holds a file
// the host lacks.
func churnListing(t *testing.T, fv *fakeVolume, rig string) func(argv []string) {
	return func(argv []string) {
		if !slices.Equal(argv, townstate.ListArgv) {
			return
		}
		writePodFile(t, fv.dir, "daemon/maintenance_state.json", "the copy's older bytes\n")
		writePodFile(t, fv.dir, rig+"/.repo.git/refs/heads/feature", "a ref made during the copy\n")
	}
}

// writePodFile writes content at rel under the claim's directory dir.
func writePodFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Errorf("making the claim directory for %s: %v", rel, err)
		return
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Errorf("writing %s into the claim: %v", rel, err)
	}
}

// TestTownStateSeedRehearsalToleratesChurnAndRunsTheSettingsStep: a rehearsal whose
// verify finds only churn prints it, exits clean, and runs the settings step and the
// profile.
func TestTownStateSeedRehearsalToleratesChurnAndRunsTheSettingsStep(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	copyStart := time.Date(2026, 11, 9, 6, 40, 0, 0, time.UTC)
	s := newSeedSession(t, town)
	s.env.rehearsal = true
	s.env.now = (&manualClock{at: copyStart}).now
	s.env.statHost = hostStat(town, copyStart, "daemon/maintenance_state.json")
	s.fv.onRun = churnListing(t, s.fv, "gastown")
	if err := runTownStateSeedIn(s.env); err != nil {
		t.Fatalf("rehearsal seed with only churn: %v\n%s", err, s.out.String())
	}
	out := s.out.String()
	for _, want := range []string{
		"changed daemon/maintenance_state.json: changed since the copy",
		"extra gastown/.repo.git/refs/heads/feature: changed since the copy",
		"matches the host copy set",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if !slices.ContainsFunc(s.kube.calls, func(a []string) bool { return podArgvIs(a, withTown(seedSettingsArgv, s.env.town)) }) {
		t.Errorf("the settings step did not run after the churn: %q", s.kube.calls)
	}
	if !strings.Contains(out, "profiled settings/config.json") {
		t.Errorf("the profile did not run after the churn:\n%s", out)
	}
}

// TestTownStateSeedRehearsalStillFailsOnARealProblem: one difference that is neither
// newer than the copy nor in the churn list fails the rehearsal, with the churn still
// printed, and the run never reaches the settings step.
func TestTownStateSeedRehearsalStillFailsOnARealProblem(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	copyStart := time.Date(2026, 11, 9, 6, 40, 0, 0, time.UTC)
	s := newSeedSession(t, town)
	s.env.rehearsal = true
	s.env.now = (&manualClock{at: copyStart}).now
	s.env.statHost = hostStat(town, copyStart, "daemon/maintenance_state.json")
	churn := churnListing(t, s.fv, "gastown")
	s.fv.onRun = func(argv []string) {
		churn(argv)
		if slices.Equal(argv, townstate.ListArgv) {
			writePodFile(t, s.fv.dir, "daemon/nested/notes.json", "edited in the pod\n")
		}
	}
	err := runTownStateSeedIn(s.env)
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("seed = %v, want exit 1\n%s", err, s.out.String())
	}
	out := s.out.String()
	if !strings.Contains(out, "changed daemon/nested/notes.json\n") {
		t.Errorf("the real difference is not named as a problem:\n%s", out)
	}
	if !strings.Contains(out, "changed since the copy") {
		t.Errorf("the churn is not printed beside the problem:\n%s", out)
	}
	if !strings.Contains(err.Error(), "partial and not to be used") {
		t.Errorf("error does not say the claim is partial: %v", err)
	}
	if i := slices.IndexFunc(s.kube.calls, func(a []string) bool { return podArgvIs(a, withTown(seedSettingsArgv, s.env.town)) }); i >= 0 {
		t.Errorf("the settings step ran after a real problem: %q", s.kube.calls[i])
	}
	if strings.Contains(out, "profiled ") {
		t.Errorf("a refused rehearsal was reported as profiled:\n%s", out)
	}
}

// TestTownStateSeedCutoverStaysStrictOnChurn: the cut-over form fails on the same
// churn that a rehearsal tolerates, and names it as a difference.
func TestTownStateSeedCutoverStaysStrictOnChurn(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, kube, out := cutoverSession(t, town)
	fv.onRun = churnListing(t, fv, "alpha")
	err := runTownStateSeedIn(env)
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("cut-over seed = %v, want exit 1\n%s", err, out.String())
	}
	for _, want := range []string{
		"changed daemon/maintenance_state.json\n",
		"extra alpha/.repo.git/refs/heads/feature\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not name %q as a difference:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "changed since the copy") {
		t.Errorf("the cut-over form printed churn:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "partial and not to be used") {
		t.Errorf("error does not say the claim is partial: %v", err)
	}
	if i := slices.IndexFunc(kube.calls, func(a []string) bool { return podArgvIs(a, withTown(seedSettingsArgv, town)) }); i >= 0 {
		t.Errorf("the cut-over settings step ran after a failed verify: %q", kube.calls[i])
	}
}
