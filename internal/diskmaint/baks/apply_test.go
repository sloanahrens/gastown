package baks

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// applyLedger returns a ledger in a temporary directory, so a test can read back
// the lines an apply appended.
func applyLedger(t *testing.T) *diskmaint.Ledger {
	t.Helper()
	return diskmaint.NewLedger(filepath.Join(t.TempDir(), "maint.jsonl"), 0)
}

// applyTarget returns a target that reads cfgDir and binDir and records to l.
func applyTarget(cfgDir, binDir string, l *diskmaint.Ledger) *Target {
	return NewTarget(Config{
		ConfigDirs: []string{cfgDir},
		BinaryDirs: []string{binDir},
		Ledger:     l,
		Run:        notOpen,
	})
}

// applyPolicy is the policy a test hands an apply plan: a run allowed to remove.
// The policy travels with the plan, so a test about Apply states it there.
var applyPolicy = diskmaint.Policy{Apply: true, Now: testNow}

// notOpen answers lsof's "nothing holds this file" for every path, so a test
// about something else does not have to name each file it removes.
func notOpen(context.Context, []string, string, ...string) ([]byte, error) {
	return nil, exitErr(1)
}

// fileAction is a delete-file action for path, sized as its file is.
func fileAction(path string, size int64) diskmaint.Action {
	return diskmaint.Action{Kind: deleteKind, Ref: path, Bytes: size, Why: "test"}
}

// assertKept fails the test when path is gone.
func assertKept(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s was removed or cannot be read: %v", path, err)
	}
}

// assertGone fails the test when path is still there.
func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s was not removed (Lstat: %v)", path, err)
	}
}

// lastEntries returns the newest n ledger lines, failing the test on a read error.
func lastEntries(t *testing.T, l *diskmaint.Ledger, n int) []diskmaint.LedgerEntry {
	t.Helper()
	entries, err := l.Tail(n)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	return entries
}

// TestApplyDryRunRemovesNothing is the dry-run rule: with Policy.Apply false an
// Apply removes no file, records its one ledger line and reports the dry run
// rather than reading as a clean apply.
func TestApplyDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	old := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  diskmaint.Policy{Now: testNow},
		Actions: []diskmaint.Action{fileAction(old, 10)},
	})
	if !errors.Is(err, errDryRun) {
		t.Fatalf("Apply error = %v, want errDryRun", err)
	}
	assertKept(t, old)
	if len(res.Done) != 0 || len(res.Failed) != 0 || res.BytesFreed != 0 {
		t.Errorf("Result = %+v, want nothing done, failed or freed", res)
	}
	entries := lastEntries(t, l, 2)
	if len(entries) != 1 || entries[0].Done != 0 || len(entries[0].Errors) != 1 {
		t.Errorf("ledger = %+v, want one line recording the dry run", entries)
	}
}

// TestApplyDeletesPlannedFiles checks the success path: a planned .bak copy and
// a stand-alone bd.<sha> build are removed and counted, and a live file the plan
// does not name is left alone.
func TestApplyDeletesPlannedFiles(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 20)
	sha := writeBackup(t, binDir, "bd.abcdef1", 10*24*time.Hour, 7)
	live := writeBackup(t, cfgDir, "app.toml", 42*24*time.Hour, 5)

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 20), fileAction(sha, 7)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertGone(t, bak)
	assertGone(t, sha)
	assertKept(t, live)
	if len(res.Done) != 2 || len(res.Failed) != 0 || res.BytesFreed != 27 {
		t.Errorf("Result = %+v, want two done, none failed and 27 bytes freed", res)
	}
}

// TestApplyRefusesSymlinkDirAndOutsidePaths drives each path the removal must
// never take: a symlink whose target is a real backup, an empty directory named
// like a backup, a backup outside the configured directories, a nested backup
// in an unlisted subdirectory, and a ref that only reaches a configured
// directory through "..". Each is a Failure, and each stays on disk.
func TestApplyRefusesSymlinkDirAndOutsidePaths(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	l := applyLedger(t)

	victim := writeBackup(t, outside, "victim.bak", 40*24*time.Hour, 10)
	link := filepath.Join(cfgDir, "link.bak")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	emptyDir := filepath.Join(cfgDir, "dir.bak")
	mustMkdir(t, emptyDir)
	stray := writeBackup(t, outside, "stray.bak", 40*24*time.Hour, 10)
	nested := filepath.Join(cfgDir, "sub")
	mustMkdir(t, nested)
	nestedBak := writeBackup(t, nested, "nested.bak", 40*24*time.Hour, 10)
	// The name reaches a real backup in cfgDir only through "..", so a ref that
	// is not clean must be refused rather than cleaned and removed.
	realBak := writeBackup(t, cfgDir, "via.bak", 40*24*time.Hour, 10)
	dotdot := cfgDir + string(filepath.Separator) + ".." + string(filepath.Separator) +
		filepath.Base(cfgDir) + string(filepath.Separator) + "via.bak"

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target: planTarget,
		Policy: applyPolicy,
		Actions: []diskmaint.Action{
			fileAction(link, 10),
			fileAction(emptyDir, 0),
			fileAction(stray, 10),
			fileAction(nestedBak, 10),
			fileAction(dotdot, 10),
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Done) != 0 || len(res.Failed) != 5 {
		t.Fatalf("Result = %+v, want five failures and nothing done", res)
	}
	for _, path := range []string{link, emptyDir, victim, stray, nestedBak, realBak} {
		assertKept(t, path)
	}
}

// TestApplyRefusesNonBackupName: a live config file, a text file and a bd name
// that is not a sha are inside a configured directory but are not backups, so
// each is refused and kept.
func TestApplyRefusesNonBackupName(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	live := writeBackup(t, cfgDir, "app.toml", 42*24*time.Hour, 5)
	notes := writeBackup(t, cfgDir, "notes.txt", 42*24*time.Hour, 5)
	badSha := writeBackup(t, binDir, "bd.zz", 42*24*time.Hour, 5)

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target: planTarget,
		Policy: applyPolicy,
		Actions: []diskmaint.Action{
			fileAction(live, 5),
			fileAction(notes, 5),
			fileAction(badSha, 5),
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Done) != 0 || len(res.Failed) != 3 {
		t.Fatalf("Result = %+v, want three failures and nothing done", res)
	}
	for _, path := range []string{live, notes, badSha} {
		assertKept(t, path)
	}
}

// TestApplyRefusesAnotherKind: an action this target does not carry is a
// Failure, and the file it names stays.
func TestApplyRefusesAnotherKind(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	res, err := applyTarget(cfgDir, binDir, applyLedger(t)).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{{Kind: "delete-manifest", Ref: bak, Bytes: 10}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Done) != 0 || len(res.Failed) != 1 {
		t.Fatalf("Result = %+v, want one failure and nothing done", res)
	}
	assertKept(t, bak)
}

// TestApplyContinuesAfterFailure: the removal of the middle file fails, and the
// files on either side of it are still removed.
func TestApplyContinuesAfterFailure(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	first := writeBackup(t, cfgDir, "a.toml.bak-01", 40*24*time.Hour, 10)
	middle := writeBackup(t, cfgDir, "b.toml.bak-01", 40*24*time.Hour, 10)
	last := writeBackup(t, cfgDir, "c.toml.bak-01", 40*24*time.Hour, 10)

	target := applyTarget(cfgDir, binDir, l)
	target.remove = func(path string) error {
		if path == middle {
			return errors.New("device busy")
		}
		return os.Remove(path)
	}
	res, err := target.Apply(context.Background(), diskmaint.Plan{
		Target: planTarget,
		Policy: applyPolicy,
		Actions: []diskmaint.Action{
			fileAction(first, 10),
			fileAction(middle, 10),
			fileAction(last, 10),
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertGone(t, first)
	assertKept(t, middle)
	assertGone(t, last)
	if len(res.Done) != 2 || len(res.Failed) != 1 || res.Failed[0].Action.Ref != middle {
		t.Errorf("Result = %+v, want two done and the middle file failed", res)
	}
	if res.BytesFreed != 20 {
		t.Errorf("BytesFreed = %d, want 20", res.BytesFreed)
	}
}

// TestApplyAppendsOneLedgerEntry: each Apply appends exactly one line, including
// for an empty plan that removes nothing.
func TestApplyAppendsOneLedgerEntry(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	a := writeBackup(t, cfgDir, "a.toml.bak-01", 40*24*time.Hour, 10)
	b := writeBackup(t, cfgDir, "b.toml.bak-01", 40*24*time.Hour, 15)
	target := applyTarget(cfgDir, binDir, l)

	if _, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(a, 10), fileAction(b, 15)},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries := lastEntries(t, l, 5)
	if len(entries) != 1 {
		t.Fatalf("after one apply the ledger holds %d lines, want 1", len(entries))
	}
	got := entries[0]
	if got.Target != planTarget || got.Mode != "apply" || got.Planned != 2 || got.Done != 2 || got.BytesFreed != 25 {
		t.Errorf("ledger line = %+v, want an apply of two planned and two done, 25 bytes freed", got)
	}

	if _, err := target.Apply(context.Background(), diskmaint.Plan{Target: planTarget, Policy: applyPolicy}); err != nil {
		t.Fatalf("empty Apply: %v", err)
	}
	entries = lastEntries(t, l, 5)
	if len(entries) != 2 || entries[1].Planned != 0 || len(entries[1].Errors) != 0 {
		t.Errorf("ledger = %+v, want a second clean line for the empty plan", entries)
	}
}

// TestApplyRefusedPlanRemovesNothing: a plan that could not read its inputs is
// reported as an error, removes nothing and is still recorded with its refusal.
func TestApplyRefusedPlanRemovesNothing(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
		Refused: "unreadable backup state: " + cfgDir + ": cannot be read",
	})
	if !errors.Is(err, errPlanRefused) {
		t.Fatalf("Apply error = %v, want errPlanRefused", err)
	}
	assertKept(t, bak)
	if len(res.Done) != 0 {
		t.Errorf("Result.Done = %v, want nothing", res.Done)
	}
	entries := lastEntries(t, l, 2)
	if len(entries) != 1 || !strings.Contains(entries[0].Refused, "unreadable backup state") {
		t.Errorf("ledger = %+v, want one line carrying the refusal", entries)
	}
}

// TestApplyNeedsALedger: a target with no ledger does not start, so nothing is
// removed by a run that could not record it.
func TestApplyNeedsALedger(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	_, err := applyTarget(cfgDir, binDir, nil).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if !errors.Is(err, errNoLedger) {
		t.Fatalf("Apply error = %v, want errNoLedger", err)
	}
	assertKept(t, bak)
}

// TestApplyStopsOnACancelledContext: a context canceled before the run stops it
// before the first file, and the cancellation is the error.
func TestApplyStopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := applyTarget(cfgDir, binDir, applyLedger(t)).Apply(ctx, diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	assertKept(t, bak)
}

// TestApplyReportsAFileThatIsGone: a planned file that is no longer there cannot
// have its state read, so it is a Failure naming that, and the run goes on to
// the file after it.
func TestApplyReportsAFileThatIsGone(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	gone := filepath.Join(cfgDir, "gone.toml.bak-01")
	kept := writeBackup(t, cfgDir, "kept.toml.bak-01", 40*24*time.Hour, 10)

	res, err := applyTarget(cfgDir, binDir, applyLedger(t)).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(gone, 0), fileAction(kept, 10)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Ref != gone || !strings.Contains(res.Failed[0].Err, "the file state cannot be read") {
		t.Fatalf("Failed = %+v, want the missing file reported as unreadable", res.Failed)
	}
	if len(res.Done) != 1 {
		t.Errorf("Done = %v, want the file after the missing one removed", res.Done)
	}
	assertGone(t, kept)
}

// TestApplyRefusesADirectorySwappedForTheFile: a directory that replaces the
// file between the check and the removal is refused and stays on disk, because
// the removal unlinks and an unlink of a directory fails (om c029468a799e).
func TestApplyRefusesADirectorySwappedForTheFile(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	target := applyTarget(cfgDir, binDir, applyLedger(t))
	// The handle check runs between the Lstat and the removal, so it is where
	// the swap is modelled: the regular file Lstat read is gone and an empty
	// directory holds its name when the removal runs.
	target.cfg.Run = func(context.Context, []string, string, ...string) ([]byte, error) {
		if err := os.Remove(bak); err != nil {
			return nil, err
		}
		if err := os.Mkdir(bak, 0o755); err != nil {
			return nil, err
		}
		return nil, exitErr(1)
	}
	res, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Done) != 0 || len(res.Failed) != 1 {
		t.Fatalf("Result = %+v, want the swapped directory refused", res)
	}
	info, err := os.Lstat(bak)
	if err != nil || !info.IsDir() {
		t.Errorf("%s = %v (Lstat err %v), want the directory still there", bak, info, err)
	}
}

// TestApplyRefusesAFileAProcessHeldOpen: a file a process holds open when Apply
// asks again is a Failure and stays, even though the plan saw it closed
// (om 0275b7e840cc).
func TestApplyRefusesAFileAProcessHeldOpen(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	lsof := newFakeLsof(t)
	lsof.open(bak, "4242")

	target := applyTarget(cfgDir, binDir, l)
	target.cfg.Run = lsof.run
	res, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertKept(t, bak)
	if len(res.Done) != 0 || len(res.Failed) != 1 {
		t.Fatalf("Result = %+v, want the open file refused", res)
	}
	if !strings.Contains(res.Failed[0].Err, "a process holds the file open") {
		t.Errorf("failure = %q, want it to name the handle", res.Failed[0].Err)
	}
	entries := lastEntries(t, l, 2)
	if len(entries) != 1 || len(entries[0].Errors) != 1 {
		t.Errorf("ledger = %+v, want one line recording the refusal", entries)
	}
}

// TestApplyKeepsAFileTheHandleCheckCannotAnswer: an lsof that cannot answer at
// apply time keeps the file, because an unknown handle is not a closed one
// (om 0275b7e840cc).
func TestApplyKeepsAFileTheHandleCheckCannotAnswer(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	lsof := newFakeLsof(t)
	lsof.failed(bak, 2)

	target := applyTarget(cfgDir, binDir, applyLedger(t))
	target.cfg.Run = lsof.run
	res, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertKept(t, bak)
	if len(res.Done) != 0 || len(res.Failed) != 1 {
		t.Fatalf("Result = %+v, want the file kept", res)
	}
	if !strings.Contains(res.Failed[0].Err, "the open check cannot answer") {
		t.Errorf("failure = %q, want it to name the check that could not answer", res.Failed[0].Err)
	}
}

// TestApplyNeedsAHandleCheck: a target with no lsof runner cannot re-check the
// handles the plan judged, so it removes nothing and reports the missing check.
func TestApplyNeedsAHandleCheck(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	target := applyTarget(cfgDir, binDir, applyLedger(t))
	target.cfg.Run = nil
	_, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if !errors.Is(err, errNoRunner) {
		t.Fatalf("Apply error = %v, want errNoRunner", err)
	}
	assertKept(t, bak)
}

// TestApplyStopsWhenTheHandleCheckIsCutShort: a context canceled while lsof is
// asked stops the run and is its error, rather than reading as one more failed
// file.
func TestApplyStopsWhenTheHandleCheckIsCutShort(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lsof := newFakeLsof(t)
	lsof.closed(bak)
	lsof.cancel = cancel

	target := applyTarget(cfgDir, binDir, applyLedger(t))
	target.cfg.Run = lsof.run
	res, err := target.Apply(ctx, diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	assertKept(t, bak)
	if len(res.Done) != 0 {
		t.Errorf("Done = %v, want nothing removed", res.Done)
	}
}

// TestApplyGatesOnThePolicyThePlanWasBuiltWith: the dry-run gate follows the
// policy the caller planned with. A plan built to apply is applied, so an apply
// is not the dry run a target built without a policy would make it
// (om d47f0185ddfe).
func TestApplyGatesOnThePolicyThePlanWasBuiltWith(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	// Three copies of one binary: the newest two are kept, so the third is the
	// one the plan removes.
	writeBackup(t, binDir, "gt.bak-01", 1*time.Hour, 10)
	writeBackup(t, binDir, "gt.bak-02", 2*time.Hour, 10)
	bak := writeBackup(t, binDir, "gt.bak-03", 3*time.Hour, 10)
	lsof := newFakeLsof(t)
	for _, path := range []string{bak, filepath.Join(binDir, "gt.bak-01"), filepath.Join(binDir, "gt.bak-02")} {
		lsof.closed(path)
	}
	target := NewTarget(Config{ConfigDirs: []string{cfgDir}, BinaryDirs: []string{binDir}, Ledger: l, Run: lsof.run})

	plan, err := target.Plan(context.Background(), diskmaint.Policy{Apply: true, Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Ref != bak {
		t.Fatalf("plan actions = %+v, want the old backup", plan.Actions)
	}
	if _, err := target.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertGone(t, bak)
}

// TestApplyRefusesAPlanBuiltAsADryRun: a plan the caller built as a dry run is
// still a dry run when applied, so no file is removed (om d47f0185ddfe).
func TestApplyRefusesAPlanBuiltAsADryRun(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	writeBackup(t, binDir, "gt.bak-01", 1*time.Hour, 10)
	writeBackup(t, binDir, "gt.bak-02", 2*time.Hour, 10)
	bak := writeBackup(t, binDir, "gt.bak-03", 3*time.Hour, 10)
	lsof := newFakeLsof(t)
	for _, path := range []string{bak, filepath.Join(binDir, "gt.bak-01"), filepath.Join(binDir, "gt.bak-02")} {
		lsof.closed(path)
	}
	target := NewTarget(Config{ConfigDirs: []string{cfgDir}, BinaryDirs: []string{binDir}, Ledger: l, Run: lsof.run})

	plan, err := target.Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := target.Apply(context.Background(), plan); !errors.Is(err, errDryRun) {
		t.Fatalf("Apply error = %v, want errDryRun", err)
	}
	assertKept(t, bak)
}

// TestApplyGatesOnThePlanNotTheTargetsLastPlan: a dry-run plan stays a dry run
// after a later Plan on the same target ran with Apply true, because the policy
// travels with the plan and not on the target (om d3a492b3cdfb).
func TestApplyGatesOnThePlanNotTheTargetsLastPlan(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	writeBackup(t, binDir, "gt.bak-01", 1*time.Hour, 10)
	writeBackup(t, binDir, "gt.bak-02", 2*time.Hour, 10)
	bak := writeBackup(t, binDir, "gt.bak-03", 3*time.Hour, 10)
	lsof := newFakeLsof(t)
	for _, path := range []string{bak, filepath.Join(binDir, "gt.bak-01"), filepath.Join(binDir, "gt.bak-02")} {
		lsof.closed(path)
	}
	target := NewTarget(Config{ConfigDirs: []string{cfgDir}, BinaryDirs: []string{binDir}, Ledger: l, Run: lsof.run})

	dry, err := target.Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("dry Plan: %v", err)
	}
	apply, err := target.Plan(context.Background(), diskmaint.Policy{Apply: true, Now: testNow})
	if err != nil {
		t.Fatalf("apply Plan: %v", err)
	}
	if _, err := target.Apply(context.Background(), dry); !errors.Is(err, errDryRun) {
		t.Fatalf("Apply(dry plan) error = %v, want errDryRun", err)
	}
	assertKept(t, bak)
	if _, err := target.Apply(context.Background(), apply); err != nil {
		t.Fatalf("Apply(apply plan): %v", err)
	}
	assertGone(t, bak)
}

// TestApplyStampsTheRunWithTheInjectedClock: the ledger line carries the
// target's clock, not the wall clock (om b495be3394fe).
func TestApplyStampsTheRunWithTheInjectedClock(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("test", -3600))
	target := applyTarget(cfgDir, binDir, l)
	target.cfg.Now = func() time.Time { return when }

	if _, err := target.Apply(context.Background(), diskmaint.Plan{Target: planTarget, Policy: applyPolicy}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries := lastEntries(t, l, 2)
	if len(entries) != 1 || !entries[0].TS.Equal(when.UTC()) {
		t.Fatalf("ledger = %+v, want one line stamped %v", entries, when.UTC())
	}
}

// TestApplyDoesNotQuoteTheRef: a failed removal is recorded with its cause and
// the action's position, never the ref, which is plan text no check has passed
// (om 1ea26114ef06).
func TestApplyDoesNotQuoteTheRef(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	gone := filepath.Join(cfgDir, "gone.toml.bak-01")

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(gone, 0)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Failed) != 1 || strings.Contains(res.Failed[0].Err, gone) {
		t.Fatalf("failure = %+v, want it to name the cause and not the ref", res.Failed)
	}
	entries := lastEntries(t, l, 2)
	if len(entries) != 1 || len(entries[0].Errors) != 1 || strings.Contains(entries[0].Errors[0], gone) {
		t.Fatalf("ledger = %+v, want the error to leave the ref out", entries)
	}
}

// TestApplyReportsALedgerThatCannotBeWritten: a ledger under a regular file
// cannot be written, so the apply returns an error naming the record. The file
// it removed before the record is still removed.
func TestApplyReportsALedgerThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", notADir, err)
	}
	l := diskmaint.NewLedger(filepath.Join(notADir, "maint.jsonl"), 0)
	bak := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)

	res, err := applyTarget(cfgDir, binDir, l).Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(bak, 10)},
	})
	if err == nil || !strings.Contains(err.Error(), "record the apply") {
		t.Fatalf("Apply error = %v, want one naming the record that failed", err)
	}
	assertGone(t, bak)
	if len(res.Done) != 1 {
		t.Errorf("Done = %v, want the removal reported even though the record failed", res.Done)
	}
}
