package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// gcConfig wires a target whose collect runs through run.
func gcConfig(t *testing.T, reg *recordingRegistry, path string, run diskmaint.Runner) Config {
	t.Helper()
	cfg := applyConfig(t, reg, path)
	cfg.Run = run
	return cfg
}

// wantCollect is the command the plan fixes: docker exec with the validated
// container name and the config path, on the configured daemon.
func wantCollect(container, dockerHost string) (argv []string, env []string) {
	return []string{"docker", "exec", container, "timeout", CollectTimeoutSeconds, "registry", "garbage-collect", "--delete-untagged", "/etc/docker/registry/config.yml"},
		[]string{"DOCKER_HOST=" + dockerHost}
}

// refInRepo is a fixture digest in the main repository.
func refInRepo(label string) string { return RepoMain + "@" + dg(label) }

// deleteFailure is the ledger line one failed delete writes, in the client's
// words plus the ref the target names.
func deleteFailure(ref, label string, code int) string {
	return fmt.Sprintf("delete %s: DELETE /v2/%s/manifests/%s: HTTP %d", ref, RepoMain, dg(label), code)
}

func TestGarbageCollectRunsOnlyAfterASuccessfulDelete(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	steps := &stepLog{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, func(context.Context, []string, string, ...string) ([]byte, error) {
		steps.add("collect")
		return nil, nil
	})
	cfg.HTTP = &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
		steps.add("delete")
		reg.handler(w, r)
	})}

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := steps.all(), []string{"delete", "collect"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want the collect after the delete %v", got, want)
	}
	if !result.Collected {
		t.Fatal("result.Collected = false; want the collect recorded as run")
	}
	if entry := readLedger(t, path)[0]; len(entry.Errors) != 0 {
		t.Fatalf("ledger errors = %v, want none from a collect that ran", entry.Errors)
	}
}

func TestGarbageCollectSkippedWhenEveryDeleteFailed(t *testing.T) {
	t.Parallel()
	ref := refInRepo("a1")
	reg := newRecordingRegistry(map[string]int{ref: http.StatusInternalServerError})
	run := &fakeRunner{}
	path := ledgerPath(t)

	result, err := NewTarget(gcConfig(t, reg, path, run.run)).Apply(context.Background(), deletePlan(ref))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect after a run that deleted nothing")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	entry := readLedger(t, path)[0]
	want := []string{deleteFailure(ref, "a1", http.StatusInternalServerError)}
	if !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v with no collect line", entry.Errors, want)
	}
}

func TestGarbageCollectUsesTheConfiguredDockerHost(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	cfg := gcConfig(t, reg, ledgerPath(t), run.run)
	// A host unlike the helper's, so the assertion names the one the config
	// supplied rather than a constant this package also hard-codes.
	cfg.DockerHost = "unix:///run/registry-test/docker.sock"
	cfg.Container = "registry-test"

	if _, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1"))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("commands = %v, want the one collect", run.calls)
	}
	argv, env := wantCollect("registry-test", "unix:///run/registry-test/docker.sock")
	if !slices.Equal(run.calls[0], argv) {
		t.Fatalf("collect argv = %v, want %v", run.calls[0], argv)
	}
	if !slices.Equal(run.envs[0], env) {
		t.Fatalf("collect env = %v, want %v", run.envs[0], env)
	}
}

func TestGarbageCollectSkippedWhenPushInFlight(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)
	cfg.PushInFlight = func() bool { return true }

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete kept", result.Done)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect while a push is in flight")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	entry := readLedger(t, path)[0]
	if entry.Done != 1 {
		t.Fatalf("ledger done = %d, want 1", entry.Done)
	}
	want := []string{"collect skipped: a push is in flight"}
	if !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestGarbageCollectRefusesWhenThePushGuardIsUnwired(t *testing.T) {
	t.Parallel()
	// No PushInFlight means the guard cannot answer. The collect must not run:
	// a registry storage walk with a push possibly writing it is the failure
	// the guard exists to prevent.
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)
	cfg.PushInFlight = nil

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete kept", result.Done)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect from a guard nobody wired")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	want := []string{"collect skipped: no push-in-flight reader configured"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestGarbageCollectRefusesWithoutARunner(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, nil)

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect without a runner")
	}
	want := []string{"collect skipped: no runner configured"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestGarbageCollectRejectsBadContainerName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "gt registry", "../gt-registry", "gt/registry"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := newRecordingRegistry(nil)
			run := &fakeRunner{}
			path := ledgerPath(t)
			cfg := gcConfig(t, reg, path, run.run)
			cfg.Container = name

			result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if result.Collected {
				t.Fatal("result.Collected = true; want no command from a name that failed validation")
			}
			if len(run.calls) != 0 {
				t.Fatalf("commands = %v, want none", run.calls)
			}
			entry := readLedger(t, path)[0]
			want := []string{"collect skipped: container name failed validation"}
			if !slices.Equal(entry.Errors, want) {
				t.Fatalf("ledger errors = %v, want %v with the refused name not quoted", entry.Errors, want)
			}
		})
	}
}

func TestGarbageCollectFailureIsRecordedNotFatal(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{err: errors.New("registry container is not running")}
	path := ledgerPath(t)

	result, err := NewTarget(gcConfig(t, reg, path, run.run)).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v; a failed collect is recorded and does not fail the apply", err)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; the collect command failed")
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete kept", result.Done)
	}
	want := []string{"collect: registry container is not running"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestGarbageCollectRunsAfterAPartialDeleteFailure(t *testing.T) {
	t.Parallel()
	failing, deleted := refInRepo("a1"), refInRepo("a2")
	reg := newRecordingRegistry(map[string]int{failing: http.StatusInternalServerError})
	run := &fakeRunner{}
	path := ledgerPath(t)

	result, err := NewTarget(gcConfig(t, reg, path, run.run)).Apply(context.Background(), deletePlan(failing, deleted))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Done) != 1 || len(result.Failed) != 1 {
		t.Fatalf("done = %d, failed = %d; want one each", len(result.Done), len(result.Failed))
	}
	if !result.Collected {
		t.Fatal("result.Collected = false; one delete succeeded, so the blobs are collectable")
	}
	if len(run.calls) != 1 {
		t.Fatalf("commands = %v, want the one collect", run.calls)
	}
	entry := readLedger(t, path)[0]
	want := []string{deleteFailure(failing, "a1", http.StatusInternalServerError)}
	if !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want only the failed delete %v", entry.Errors, want)
	}
}

// TestGarbageCollectTimeoutIsARealBound: the collect is bounded inside the
// container, so a hung collect ends there and docker exec returns. The bound
// must be a real number of seconds, neither zero (timeout 0 is no timeout at
// all) nor so large it is no bound (gt-dl4nv.13).
func TestGarbageCollectTimeoutIsARealBound(t *testing.T) {
	t.Parallel()
	secs, err := strconv.Atoi(CollectTimeoutSeconds)
	if err != nil {
		t.Fatalf("CollectTimeoutSeconds %q is not a number of seconds: %v", CollectTimeoutSeconds, err)
	}
	if secs < 60 || secs > 7200 {
		t.Errorf("CollectTimeoutSeconds = %d, want between one minute and two hours so the collect is bounded but not cut short", secs)
	}
	// The command carries the ceiling and collectTimeout clamps to it, so the
	// two must name the same number.
	if CollectTimeoutSeconds != strconv.Itoa(collectCeilingSeconds) {
		t.Fatalf("CollectTimeoutSeconds = %q, want the ceiling %d", CollectTimeoutSeconds, collectCeilingSeconds)
	}
	argv, _ := wantCollect("gt-registry", "unix:///x")
	if len(argv) < 5 || argv[3] != "timeout" || argv[4] != CollectTimeoutSeconds {
		t.Errorf("the collect command %v does not run under the container's timeout", argv)
	}
}

// TestGarbageCollectTimeoutFollowsTheCallersDeadline: the collect's in-container
// timeout is the time left on the caller's deadline less the grace, so the
// collect is over before the deadline the retention set, and a deadline that
// leaves more than the ceiling does not raise the collect's bound above it
// (gt-dl4nv.13).
func TestGarbageCollectTimeoutFollowsTheCallersDeadline(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	var gotCtx context.Context
	var gotArgs []string
	run := func(ctx context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		gotCtx, gotArgs = ctx, args
		return nil, nil
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(60*time.Second))
	defer cancel()

	result, err := NewTarget(gcConfig(t, reg, path, run)).Apply(ctx, deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.Collected {
		t.Fatal("result.Collected = false; want the collect recorded as run")
	}
	if entry := readLedger(t, path)[0]; len(entry.Errors) != 0 {
		t.Fatalf("ledger errors = %v, want none from a collect that answered", entry.Errors)
	}
	// Nothing but the container's timeout may end the collect: a context the
	// caller's deadline cancels would kill the docker client at the deadline
	// and leave the collect running in the container.
	if _, bounded := gotCtx.Deadline(); bounded {
		t.Fatal("the collect ran under a context the caller's deadline ends; only the container's timeout may end it")
	}
	if len(gotArgs) < 4 || gotArgs[2] != "timeout" {
		t.Fatalf("collect argv = %v, want it run under the container's timeout", gotArgs)
	}
	secs, err := strconv.Atoi(gotArgs[3])
	if err != nil {
		t.Fatalf("collect timeout %q is not a number of seconds: %v", gotArgs[3], err)
	}
	// A 60s deadline less the grace leaves 59s. The range admits the time the
	// delete step spends before the collect without admitting a bound the
	// deadline cannot cover.
	if secs > 59 || secs < 55 {
		t.Fatalf("collect timeout = %ds for a deadline 60s away, want the 59s the deadline less the grace leaves it", secs)
	}
}

// TestGarbageCollectTimeoutCappedAtTheCeiling: a deadline that leaves the
// collect more time than the ceiling does not raise its bound, so a collect
// still ends inside the container in the time a caller with no deadline gets.
func TestGarbageCollectTimeoutCappedAtTheCeiling(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()

	timeout, ok := collectTimeout(ctx)
	if !ok {
		t.Fatal("collectTimeout refused a deadline an hour away")
	}
	if timeout != CollectTimeoutSeconds {
		t.Fatalf("collect timeout = %q for a deadline an hour away, want the ceiling %q", timeout, CollectTimeoutSeconds)
	}
}

// TestGarbageCollectRefusedWhenTheDeadlineLeavesNoTime: a deadline closer than
// the grace plus a whole second refuses the collect instead of starting one the
// container would end at once. A refused collect is recorded, not run.
func TestGarbageCollectRefusedWhenTheDeadlineLeavesNoTime(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	target := NewTarget(gcConfig(t, reg, ledgerPath(t), run.run))
	ctx, cancel := context.WithTimeout(context.Background(), collectGrace)
	defer cancel()
	result := diskmaint.Result{Done: deletePlan(refInRepo("a1")).Actions}

	got := target.gc(ctx, &result)

	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none from a deadline that leaves no time to run one", run.calls)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want a refused collect")
	}
	want := []string{"collect: the caller's deadline leaves no time to run it"}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger lines = %v, want %v", got, want)
	}
}

// TestGarbageCollectRefusedWhenTheCallersContextIsDone: a collect does not
// start on a context that is already done, whether the caller's budget was
// spent or it gave up. The command is refused rather than left to the runner
// to notice.
func TestGarbageCollectRefusedWhenTheCallersContextIsDone(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	target := NewTarget(gcConfig(t, reg, ledgerPath(t), run.run))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := diskmaint.Result{Done: deletePlan(refInRepo("a1")).Actions}

	got := target.gc(ctx, &result)

	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none on a context that is done", run.calls)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want a refused collect")
	}
	want := []string{"collect: the caller's context is done: context canceled"}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger lines = %v, want %v", got, want)
	}
}

// TestGarbageCollectAbandonedAtTheCallersDeadline: a docker client that never
// answers does not hold the plugin lock past the caller's deadline. The
// deadline arrives while the collect runs, and the wait for the client is
// abandoned without the client's context being ended, so the client keeps
// running to see the container's timeout end the collect (gt-dl4nv.13).
func TestGarbageCollectAbandonedAtTheCallersDeadline(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// The deadline is an hour away and the collect ends it by cancelling, so
	// the test spends no time waiting: the deadline arrives where the test says
	// it does.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()
	var commandCtx context.Context
	// The collect answers only when the test ends, so only the bound can end
	// the wait for it.
	run := func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
		commandCtx = ctx
		cancel()
		<-release
		return nil, nil
	}
	target := NewTarget(gcConfig(t, reg, ledgerPath(t), run))
	result := diskmaint.Result{Done: deletePlan(refInRepo("a1")).Actions}

	got := target.gc(ctx, &result)

	if result.Collected {
		t.Fatal("result.Collected = true; a collect that never answered did not run")
	}
	want := []string{"collect: no answer by the caller's deadline: context canceled"}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger lines = %v, want %v", got, want)
	}
	// The abandon is the wait's, not the command's: the docker client keeps
	// running to collect the container's own timeout ending the collect.
	if err := commandCtx.Err(); err != nil {
		t.Fatalf("the command's context was ended at the deadline: %v", err)
	}
}

// TestGarbageCollectRecordedWhenTheCommandNeverAnswers is the retention path: an
// apply whose collect never answers at the caller's deadline records the
// failure and returns without error, so the retention continues and releases
// the plugin lock.
func TestGarbageCollectRecordedWhenTheCommandNeverAnswers(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()
	run := func(context.Context, []string, string, ...string) ([]byte, error) {
		cancel()
		<-release
		return nil, nil
	}

	result, err := NewTarget(gcConfig(t, reg, path, run)).Apply(ctx, deletePlan(refInRepo("a1")))

	if err != nil {
		t.Fatalf("apply: %v; a collect that never answers is recorded and does not fail the apply", err)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; a collect that never answered did not run")
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete kept", result.Done)
	}
	want := []string{"collect: no answer by the caller's deadline: context canceled"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}
