package civol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// applyTarget is the target with a ledger in a temporary directory, and the
// ledger's path so the tests can read the record back.
func applyTarget(t *testing.T, f *fakeDocker) (*Target, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	return NewTarget(Config{
		Dind:   f.dind,
		Runner: f.runner,
		Image:  testImage,
		Run:    f.run,
		Ledger: diskmaint.NewLedger(path, 1<<20),
	}), path
}

// deletePath is one planned delete of a path inside a volume.
func deletePath(volume, path string, bytes int64) diskmaint.Action {
	return diskmaint.Action{Kind: deleteKind, Ref: volume + ":" + path, Bytes: bytes, Why: "test"}
}

// planOf is the plan a trim is applied from.
func planOf(actions ...diskmaint.Action) diskmaint.Plan {
	return diskmaint.Plan{Target: planTarget, Actions: actions}
}

// apply runs the target's apply against the fake and fails the test if it
// errors. A test that expects an error calls Apply itself.
func apply(t *testing.T, tgt *Target, plan diskmaint.Plan) diskmaint.Result {
	t.Helper()
	result, err := tgt.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return result
}

// ledgerEntries reads the ledger's entries, failing the test if it cannot.
func ledgerEntries(t *testing.T, path string) []diskmaint.LedgerEntry {
	t.Helper()
	entries, err := diskmaint.NewLedger(path, 1<<20).Tail(10)
	if err != nil {
		t.Fatalf("tail ledger: %v", err)
	}
	return entries
}

// commands returns the recorded calls that contain want, in order.
func (f *fakeDocker) commands(want string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, want) {
			out = append(out, c)
		}
	}
	return out
}

// doneRefs is the refs of the actions an apply completed, in order.
func doneRefs(result diskmaint.Result) []string {
	refs := make([]string, 0, len(result.Done))
	for _, a := range result.Done {
		refs = append(refs, a.Ref)
	}
	return refs
}

// TestApplyRemovesPerVolumeThenPrunes drives a quiet trim of two volumes and
// asserts one container per volume, then both prunes, and one ledger line.
func TestApplyRemovesPerVolumeThenPrunes(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, path := applyTarget(t, f)

	result := apply(t, tgt, planOf(
		deletePath(gocacheVolume, "aa/old.bin", 2*gb),
		deletePath(npmVolume, "_cacache/tmp/x", 1*gb),
	))

	if got, want := doneRefs(result), []string{
		gocacheVolume + ":aa/old.bin",
		npmVolume + ":_cacache/tmp/x",
		"builder",
		"image",
	}; !slices.Equal(got, want) {
		t.Fatalf("done = %v, want %v", got, want)
	}
	if result.BytesFreed != 3*gb {
		t.Fatalf("bytes freed = %d, want %d", result.BytesFreed, 3*gb)
	}

	want := []string{
		"docker exec " + testDind + " docker run --rm --user 0 -v " + gocacheVolume + ":/c " +
			"--entrypoint rm " + testImage + " -rf -- /c/aa/old.bin",
		"docker exec " + testDind + " docker run --rm --user 0 -v " + npmVolume + ":/c " +
			"--entrypoint rm " + testImage + " -rf -- /c/_cacache/tmp/x",
		"docker exec " + testDind + " docker builder prune -f",
		"docker exec " + testDind + " docker image prune -f",
	}
	writes := f.writes()
	if !slices.Equal(writes, want) {
		t.Fatalf("write calls =\n  %v\nwant\n  %v", writes, want)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "prune") && strings.Contains(c, "-a") {
			t.Fatalf("prune call %q names -a; only dangling images may go", c)
		}
	}

	entries := ledgerEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Mode != "apply" || entry.Target != planTarget {
		t.Fatalf("ledger mode/target = %q/%q, want apply/%s", entry.Mode, entry.Target, planTarget)
	}
	if entry.Planned != 2 || entry.Done != 4 || entry.BytesFreed != 3*gb {
		t.Fatalf("ledger planned/done/freed = %d/%d/%d, want 2/4/%d", entry.Planned, entry.Done, entry.BytesFreed, 3*gb)
	}
	if len(entry.Errors) != 0 {
		t.Fatalf("ledger errors = %v, want none", entry.Errors)
	}
}

// TestApplyMakesTheModuleCacheWritableFirst asserts the chmod runs before the
// rm for the module cache, whose trees are read-only.
func TestApplyMakesTheModuleCacheWritableFirst(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, _ := applyTarget(t, f)

	const mod = "pkg/mod/github.com/x@v1.2.3"
	result := apply(t, tgt, planOf(deletePath(gomodVolume, mod, gb)))
	if got, want := doneRefs(result)[:1], []string{gomodVolume + ":" + mod}; !slices.Equal(got, want) {
		t.Fatalf("done = %v, want %v", got, want)
	}

	want := []string{
		"docker exec " + testDind + " docker run --rm --user 0 -v " + gomodVolume + ":/c " +
			"--entrypoint chmod " + testImage + " -R u+w -- /c/" + mod,
		"docker exec " + testDind + " docker run --rm --user 0 -v " + gomodVolume + ":/c " +
			"--entrypoint rm " + testImage + " -rf -- /c/" + mod,
	}
	if got := f.writes(); !slices.Equal(got[:2], want) {
		t.Fatalf("first write calls =\n  %v\nwant\n  %v", got, want)
	}
}

// TestApplyDefersWhenAJobStarted asserts a job that appeared since the plan
// holds the whole trim back, including the build cache's own paths.
func TestApplyDefersWhenAJobStarted(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.ps = answer{out: "job1\n"}
	tgt, path := applyTarget(t, f)

	result := apply(t, tgt, planOf(
		deletePath(gocacheVolume, "aa/old.bin", gb),
		deletePath(npmVolume, "_cacache/tmp/x", gb),
	))

	if result.Deferred != deferredJobStarted {
		t.Fatalf("deferred = %q, want %q", result.Deferred, deferredJobStarted)
	}
	if len(result.Done) != 0 || len(result.Failed) != 0 {
		t.Fatalf("done = %v, failed = %v, want neither", doneRefs(result), result.Failed)
	}
	if w := f.writes(); len(w) > 0 {
		t.Fatalf("a deferred apply issued write commands: %v", w)
	}
	if entry := ledgerEntries(t, path)[0]; entry.Deferred != deferredJobStarted || entry.Done != 0 {
		t.Fatalf("ledger deferred/done = %q/%d, want %q/0", entry.Deferred, entry.Done, deferredJobStarted)
	}
}

// TestApplySkipsThePruneWhenAJobStartsDuringTheTrim asserts a job that starts
// after the first job read, while the volumes are trimmed, holds the prune
// back. The deletes the plan named still run.
func TestApplySkipsThePruneWhenAJobStartsDuringTheTrim(t *testing.T) {
	t.Parallel()
	f := newFake()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	// The job starts when the first rm runs: the job read before the deletes
	// saw none, and the re-read before the prune sees one.
	run := func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "rm") {
			f.ps = answer{out: "job1\n"}
		}
		return f.run(ctx, env, name, args...)
	}
	tgt := NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: run,
		Ledger: diskmaint.NewLedger(path, 1<<20)})

	result := apply(t, tgt, planOf(deletePath(gocacheVolume, "aa/old.bin", gb)))

	if got, want := doneRefs(result), []string{gocacheVolume + ":aa/old.bin"}; !slices.Equal(got, want) {
		t.Fatalf("done = %v, want %v", got, want)
	}
	if result.Deferred != deferredPruneJob {
		t.Fatalf("deferred = %q, want %q", result.Deferred, deferredPruneJob)
	}
	if p := f.commands("prune"); len(p) > 0 {
		t.Fatalf("prunes ran after a job started: %v", p)
	}
	if entry := ledgerEntries(t, path)[0]; entry.Deferred != deferredPruneJob || entry.Done != 1 {
		t.Fatalf("ledger deferred/done = %q/%d, want %q/1", entry.Deferred, entry.Done, deferredPruneJob)
	}
}

// TestApplyTrimsTheBuildCacheWhileAJobRuns asserts a build-cache-only plan is
// the one a running job allows, and that it prunes nothing.
func TestApplyTrimsTheBuildCacheWhileAJobRuns(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.ps = answer{out: "job1\n"}
	tgt, _ := applyTarget(t, f)

	result := apply(t, tgt, planOf(deletePath(gocacheVolume, "aa/old.bin", gb)))
	if result.Deferred != "" {
		t.Fatalf("deferred = %q, want the build cache's paths removed", result.Deferred)
	}
	if got, want := doneRefs(result), []string{gocacheVolume + ":aa/old.bin"}; !slices.Equal(got, want) {
		t.Fatalf("done = %v, want %v", got, want)
	}
	if p := f.commands("prune"); len(p) > 0 {
		t.Fatalf("prunes ran while a job was running: %v", p)
	}
}

// TestApplyRefusesUnsafeActions drives every action re-validation rule and
// asserts each refusal issues no command at all.
func TestApplyRefusesUnsafeActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		action diskmaint.Action
	}{
		{"absolute path", deletePath(gocacheVolume, "/etc/passwd", gb)},
		{"path escaping its volume", deletePath(gocacheVolume, "a/../../etc/passwd", gb)},
		{"path spanning lines", deletePath(gocacheVolume, "a\nb", gb)},
		{"empty path", deletePath(gocacheVolume, "", gb)},
		{"volume root", deletePath(gocacheVolume, ".", gb)},
		{"volume root with a slash", deletePath(npmVolume, "./", gb)},
		{"dot segment ending the path", deletePath(gocacheVolume, "aa/.", gb)},
		{"empty segment", deletePath(gocacheVolume, "aa//bb", gb)},
		{"trailing slash", deletePath(gocacheVolume, "aa/", gb)},
		{"dot segment in a module path", deletePath(gomodVolume, "pkg/mod/./github.com/x@v1.2.3", gb)},
		{"volume not in the limits", deletePath("ci-other", "x", gb)},
		{"module under the download cache", deletePath(gomodVolume, "pkg/mod/cache/download/x/@v/v1.zip", gb)},
		{"bare download cache path", deletePath(gomodVolume, "cache/download/x/@v/v1.zip", gb)},
		{"path inside a module, not a whole module", deletePath(gomodVolume, "pkg/mod/github.com/x@v1.2.3/a.go", gb)},
		{"ref with no volume", diskmaint.Action{Kind: deleteKind, Ref: "pkg/mod/x", Bytes: gb}},
		{"wrong kind", diskmaint.Action{Kind: "delete-manifest", Ref: gocacheVolume + ":x", Bytes: gb}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			tgt, path := applyTarget(t, f)

			result := apply(t, tgt, planOf(c.action))
			if len(f.calls) != 0 {
				t.Fatalf("a refused action issued commands: %v", f.calls)
			}
			if len(result.Failed) != 1 || len(result.Done) != 0 {
				t.Fatalf("failed = %v, done = %v, want one failure and no done", result.Failed, doneRefs(result))
			}
			if entry := ledgerEntries(t, path)[0]; len(entry.Errors) == 0 || entry.Done != 0 {
				t.Fatalf("ledger errors/done = %v/%d, want the refusal recorded and nothing done", entry.Errors, entry.Done)
			}
		})
	}
}

// TestApplyRefusesOnlyTheBadAction asserts one unsafe action does not stop a
// volume the plan named correctly.
func TestApplyRefusesOnlyTheBadAction(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, _ := applyTarget(t, f)

	result := apply(t, tgt, planOf(
		deletePath(npmVolume, "_cacache/tmp/x", gb),
		deletePath(gocacheVolume, "/etc/passwd", gb),
	))
	if got, want := doneRefs(result)[0], npmVolume+":_cacache/tmp/x"; got != want {
		t.Fatalf("first done ref = %q, want %q", got, want)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != gocacheVolume+":/etc/passwd" {
		t.Fatalf("failed = %v, want only the absolute path", result.Failed)
	}
	if c := f.commands("/etc/passwd"); len(c) > 0 {
		t.Fatalf("a command named the refused path: %v", c)
	}
}

// TestApplyFailedRemoveLeavesOtherVolumes asserts a failed rm fails that
// volume's actions and the other volumes still run.
func TestApplyFailedRemoveLeavesOtherVolumes(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.removes[npmVolume] = answer{err: errors.New("exit status 1")}
	tgt, path := applyTarget(t, f)

	result := apply(t, tgt, planOf(
		deletePath(gocacheVolume, "aa/old.bin", gb),
		deletePath(npmVolume, "_cacache/tmp/x", gb),
	))

	if got, want := doneRefs(result)[0], gocacheVolume+":aa/old.bin"; got != want {
		t.Fatalf("first done ref = %q, want %q", got, want)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != npmVolume+":_cacache/tmp/x" {
		t.Fatalf("failed = %v, want the npm action only", result.Failed)
	}
	if result.BytesFreed != gb {
		t.Fatalf("bytes freed = %d, want only the volume that worked (%d)", result.BytesFreed, gb)
	}
	if c := f.commands(" -v " + npmVolume + ":"); len(c) == 0 {
		t.Fatalf("the npm volume was not attempted: %v", f.calls)
	}
	if entry := ledgerEntries(t, path)[0]; len(entry.Errors) == 0 {
		t.Fatalf("ledger errors = %v, want the failed rm recorded", entry.Errors)
	}
}

// TestApplyFailedPruneIsRecorded asserts a prune that fails is a Failed entry
// of kind prune and the other prune still runs.
func TestApplyFailedPruneIsRecorded(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.prunes["builder"] = answer{err: errors.New("exit status 1")}
	tgt, path := applyTarget(t, f)

	result := apply(t, tgt, planOf(deletePath(gocacheVolume, "aa/old.bin", gb)))

	if len(result.Failed) != 1 {
		t.Fatalf("failed = %v, want the builder prune only", result.Failed)
	}
	failure := result.Failed[0]
	if failure.Action.Kind != pruneKind || failure.Action.Ref != "builder" {
		t.Fatalf("failure = %+v, want kind %s ref builder", failure.Action, pruneKind)
	}
	if !slices.Contains(doneRefs(result), "image") {
		t.Fatalf("done = %v, want the image prune still done", doneRefs(result))
	}
	if entry := ledgerEntries(t, path)[0]; len(entry.Errors) == 0 {
		t.Fatalf("ledger errors = %v, want the failed prune recorded", entry.Errors)
	}
}

// TestApplyRefusedPlanDeletesNothing asserts a refused plan makes no command
// and is recorded as refused.
func TestApplyRefusedPlanDeletesNothing(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, path := applyTarget(t, f)

	result := apply(t, tgt, diskmaint.Plan{Target: planTarget, Refused: "unreadable volumes"})
	if len(f.calls) != 0 || len(result.Done) != 0 {
		t.Fatalf("calls = %v, done = %v, want neither", f.calls, doneRefs(result))
	}
	entry := ledgerEntries(t, path)[0]
	if entry.Refused == "" || entry.Planned != 0 || entry.Done != 0 {
		t.Fatalf("ledger = %+v, want the refusal recorded with nothing done", entry)
	}
}

// TestApplyRefusesWithoutALedger asserts an apply that cannot record does not
// start.
func TestApplyRefusesWithoutALedger(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt := NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: f.run})

	_, err := tgt.Apply(context.Background(), planOf(deletePath(gocacheVolume, "x", gb)))
	if !errors.Is(err, errNoLedger) {
		t.Fatalf("error = %v, want errNoLedger", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an apply without a ledger issued commands: %v", f.calls)
	}
}

// TestApplyWithoutARunnerRecordsTheFailure asserts an apply with nothing to run
// docker with deletes nothing and says so in the ledger.
func TestApplyWithoutARunnerRecordsTheFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	tgt := NewTarget(Config{Dind: testDind, Runner: testRunner, Image: testImage,
		Ledger: diskmaint.NewLedger(path, 1<<20)})

	_, err := tgt.Apply(context.Background(), planOf(deletePath(gocacheVolume, "x", gb)))
	if !errors.Is(err, errNoRunner) {
		t.Fatalf("error = %v, want errNoRunner", err)
	}
	if entry := ledgerEntries(t, path)[0]; len(entry.Errors) == 0 || entry.Done != 0 {
		t.Fatalf("ledger = %+v, want the missing runner recorded and nothing done", entry)
	}
}

// TestApplyUnreadableJobListDefers asserts a job list that cannot be read holds
// the trim back rather than trimming blind.
func TestApplyUnreadableJobListDefers(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.ps = answer{err: errors.New("exit status 1")}
	tgt, path := applyTarget(t, f)

	result, err := tgt.Apply(context.Background(), planOf(deletePath(gocacheVolume, "x", gb)))
	if err == nil {
		t.Fatal("apply error = nil, want the unreadable job list reported")
	}
	if result.Deferred == "" || len(result.Done) != 0 {
		t.Fatalf("deferred = %q, done = %v, want a deferral and nothing done", result.Deferred, doneRefs(result))
	}
	if w := f.writes(); len(w) > 0 {
		t.Fatalf("an apply with an unreadable job list issued write commands: %v", w)
	}
	if entry := ledgerEntries(t, path)[0]; entry.Deferred == "" {
		t.Fatalf("ledger deferred = %q, want the deferral recorded", entry.Deferred)
	}
}

// TestApplyRecordsOneLinePerRun asserts each apply appends exactly one entry.
func TestApplyRecordsOneLinePerRun(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, path := applyTarget(t, f)

	for range 2 {
		apply(t, tgt, planOf(deletePath(gocacheVolume, "aa/old.bin", gb)))
	}
	if entries := ledgerEntries(t, path); len(entries) != 2 {
		t.Fatalf("ledger entries = %d, want one per apply", len(entries))
	}
}

// TestApplyCancelledRunReportsTheCancellation asserts a cancelled apply stops
// with the caller's cancellation instead of a trim that failed on its own.
func TestApplyCancelledRunReportsTheCancellation(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt, _ := applyTarget(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tgt.Apply(ctx, planOf(deletePath(gocacheVolume, "x", gb)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// cancelledApply runs an apply whose runner cancels the run at the first call
// containing want, and returns the result, the error and that runner.
func cancelledApply(t *testing.T, f *fakeDocker, want string) (diskmaint.Result, *cancelRunner, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &cancelRunner{f: f, cancel: cancel, want: want}
	tgt := NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: r.run,
		Ledger: diskmaint.NewLedger(path, 1<<20)})
	result, err := tgt.Apply(ctx, planOf(
		deletePath(gocacheVolume, "aa/old.bin", gb),
		deletePath(npmVolume, "_cacache/tmp/x", gb),
	))
	return result, r, err
}

// TestApplyCanceledWhileReadingJobs asserts a cancellation during the job
// re-read defers the trim and reports the caller's cancellation.
func TestApplyCanceledWhileReadingJobs(t *testing.T) {
	t.Parallel()
	f := newFake()
	result, r, err := cancelledApply(t, f, "ps -q")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !r.fired {
		t.Fatalf("no call matched the job list: %v", f.calls)
	}
	if result.Deferred == "" || len(result.Done) != 0 {
		t.Fatalf("deferred = %q, done = %v, want a deferral and nothing done", result.Deferred, doneRefs(result))
	}
}

// TestApplyCanceledDuringARemove asserts a cancellation while a volume's paths
// are removed stops the run and reports the caller's cancellation.
func TestApplyCanceledDuringARemove(t *testing.T) {
	t.Parallel()
	f := newFake()
	result, r, err := cancelledApply(t, f, "--entrypoint rm")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !r.fired {
		t.Fatalf("no call matched a remove: %v", f.calls)
	}
	if len(result.Done) != 0 || len(result.Failed) == 0 {
		t.Fatalf("done = %v, failed = %v, want the cut-short volume recorded as failed", doneRefs(result), result.Failed)
	}
}

// TestApplyCanceledDuringAPrune asserts a cancellation while the prunes run
// stops the second one and reports the caller's cancellation.
func TestApplyCanceledDuringAPrune(t *testing.T) {
	t.Parallel()
	f := newFake()
	result, r, err := cancelledApply(t, f, "builder prune")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !r.fired {
		t.Fatalf("no call matched the builder prune: %v", f.calls)
	}
	if slices.Contains(doneRefs(result), "builder") || slices.Contains(doneRefs(result), "image") {
		t.Fatalf("done = %v, want no prune reported as done", doneRefs(result))
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Kind != pruneKind {
		t.Fatalf("failed = %v, want the builder prune only", result.Failed)
	}
}

// TestApplyReportsALedgerThatCannotBeWritten asserts an apply whose record
// cannot be written says so and still reports the trim it made.
func TestApplyReportsALedgerThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	f := newFake()
	// A directory where the ledger file goes fails the append however the
	// ledger opens its file, so the test does not depend on that detail.
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	tgt := NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: f.run,
		Ledger: diskmaint.NewLedger(path, 1<<20)})

	result, err := tgt.Apply(context.Background(), planOf(deletePath(gocacheVolume, "aa/old.bin", gb)))
	if err == nil || !strings.Contains(err.Error(), "record the apply") {
		t.Fatalf("error = %v, want it to name the ledger it could not write", err)
	}
	if !slices.Contains(doneRefs(result), gocacheVolume+":aa/old.bin") {
		t.Fatalf("done = %v, want the trim the run made", doneRefs(result))
	}
}
