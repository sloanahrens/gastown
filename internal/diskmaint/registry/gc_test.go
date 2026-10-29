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

func TestGCRunsOnlyAfterASuccessfulDelete(t *testing.T) {
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

func TestGCSkippedWhenEveryDeleteFailed(t *testing.T) {
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

func TestGCUsesTheConfiguredDockerHost(t *testing.T) {
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

func TestGCSkippedWhenPushInFlight(t *testing.T) {
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

func TestGCRefusesWhenThePushGuardIsUnwired(t *testing.T) {
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

func TestGCRefusesWithoutARunner(t *testing.T) {
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

func TestGCRejectsBadContainerName(t *testing.T) {
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

func TestGCFailureIsRecordedNotFatal(t *testing.T) {
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

func TestGCRunsAfterAPartialDeleteFailure(t *testing.T) {
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

// The collect is bounded inside the container: a hung collect ends there, so
// docker exec returns and the plugin lock is never held past the bound. The
// bound must be a real number of seconds, neither zero (timeout 0 is no
// timeout at all) nor so large it is no bound (gt-dl4nv.13).
func TestCollectTimeoutIsARealBound(t *testing.T) {
	t.Parallel()
	secs, err := strconv.Atoi(CollectTimeoutSeconds)
	if err != nil {
		t.Fatalf("CollectTimeoutSeconds %q is not a number of seconds: %v", CollectTimeoutSeconds, err)
	}
	if secs < 60 || secs > 7200 {
		t.Errorf("CollectTimeoutSeconds = %d, want between one minute and two hours so the collect is bounded but not cut short", secs)
	}
	argv, _ := wantCollect("gt-registry", "unix:///x")
	if len(argv) < 5 || argv[3] != "timeout" || argv[4] != CollectTimeoutSeconds {
		t.Errorf("the collect command %v does not run under the container's timeout", argv)
	}
}

// TestCollectAbandonedAtTheCallersDeadline: a collect that never answers is
// reported unanswered when the caller's deadline arrives, so the apply returns
// and the plugin lock is released instead of being held for a docker exec
// nobody hears from (gt-dl4nv.13).
func TestCollectAbandonedAtTheCallersDeadline(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	run := func(context.Context, []string, string, ...string) ([]byte, error) {
		close(started)
		// The command answers only when the test ends: nothing but the bound
		// can end the apply's wait for it.
		<-release
		return nil, nil
	}
	// Both deadlines are wall-clock waits with a bookend: the apply's is longer
	// than the delete it also has to make, and the test's is longer than the
	// apply's, so a collect that is not abandoned fails the test as a timeout
	// rather than hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	wait, expire := context.WithTimeout(context.Background(), 10*time.Second)
	defer expire()

	type outcome struct {
		result diskmaint.Result
		err    error
	}
	target := NewTarget(gcConfig(t, reg, path, run))
	plan := deletePlan(refInRepo("a1"))
	applied := make(chan outcome, 1)
	go func() {
		result, err := target.Apply(ctx, plan)
		applied <- outcome{result, err}
	}()

	var out outcome
	select {
	case out = <-applied:
	case <-wait.Done():
		t.Fatal("the apply did not return after the collect's bound; a docker exec that never answers is holding the plugin lock")
	}
	if out.err != nil {
		t.Fatalf("apply: %v", out.err)
	}
	select {
	case <-started:
	default:
		t.Fatal("the collect never started; one delete succeeded, so it should have run")
	}
	if out.result.Collected {
		t.Fatal("result.Collected = true; a collect that never answered did not run")
	}
	entry := readLedger(t, path)[0]
	unanswered := "collect: no answer by the bound: context deadline exceeded"
	if !slices.Contains(entry.Errors, unanswered) {
		t.Fatalf("ledger errors = %v, want %q", entry.Errors, unanswered)
	}
}

// TestCollectAnsweredInsideTheDeadlineCounts: a collect that answers before the
// caller's deadline is the collect that ran, however soon the deadline follows.
func TestCollectAnsweredInsideTheDeadlineCounts(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	run := &fakeRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := NewTarget(gcConfig(t, reg, path, run.run)).Apply(ctx, deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("commands = %v, want the one collect", run.calls)
	}
	if !result.Collected {
		t.Fatal("result.Collected = false; the collect answered")
	}
	if entry := readLedger(t, path)[0]; len(entry.Errors) != 0 {
		t.Fatalf("ledger errors = %v, want none from a collect that answered", entry.Errors)
	}
}
