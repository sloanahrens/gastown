package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/slot"
)

// gateContainerFor builds the container the watch sees, from the display name
// it reports, so a test reads as the log line it produces.
func gateContainerFor(display string) slot.GateContainer {
	image, name, _ := strings.Cut(display, " ")
	return slot.GateContainer{Image: image, Name: name}
}

func gateContainersFor(displays ...string) []slot.GateContainer {
	out := make([]slot.GateContainer, 0, len(displays))
	for _, d := range displays {
		out = append(out, gateContainerFor(d))
	}
	return out
}

// stubContainerWatchInterval shortens the watch's poll interval so a test can
// drive it in milliseconds. 0 disables the watch entirely.
func stubContainerWatchInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := containerWatchInterval
	containerWatchInterval = interval
	t.Cleanup(func() { containerWatchInterval = prev })
}

// stubGateContainerListing replaces the watch's docker listing. The stub goes
// through the package var rather than slot.SetContainerListerForTest so a test
// does not have to race stubNoContainers' process-wide, once-per-binary
// install (mq_batch_slot_test.go).
func stubGateContainerListing(t *testing.T, fn func() ([]slot.GateContainer, error)) {
	t.Helper()
	prev := listGateContainers
	listGateContainers = fn
	t.Cleanup(func() { listGateContainers = prev })
}

// stubGateSlotHeld replaces the watch's read of the container-gate slot.
func stubGateSlotHeld(t *testing.T, held bool) {
	t.Helper()
	prev := gateSlotHeld
	gateSlotHeld = func(string) (bool, error) { return held, nil }
	t.Cleanup(func() { gateSlotHeld = prev })
}

// watchLog is a standalone verify log a test can read back. The gate's own log
// lives in the worktree, which the poll-logic tests have no reason to build.
func watchLog(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "watch.log"))
	if err != nil {
		t.Fatalf("creating watch log: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func watchLogText(t *testing.T, f *os.File) string {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading watch log: %v", err)
	}
	return string(b)
}

// pollWatch builds a watch a test drives by calling poll() itself: the ticker
// never fires, so every container the test sees is one the test put there.
func pollWatch(t *testing.T, containers func() ([]slot.GateContainer, error), held func(string) (bool, error)) (*containerWatch, *atomic.Int32) {
	t.Helper()
	var cancels atomic.Int32
	return &containerWatch{
		interval:   time.Hour,
		containers: containers,
		slotHeld:   held,
		townRoot:   "/town",
		logFile:    watchLog(t),
		cancelRun:  func() { cancels.Add(1) },
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		started:    time.Now(),
	}, &cancels
}

func neverHeld(string) (bool, error) { return false, nil }

// TestContainerWatchBlamesAContainerTheSlotFreeRunStarted is the case the
// command-text decision cannot see (gt-0ss4): the rig's command carries no
// GT_TEST_DOCKER=1, the gate ran slot-free, and the suite started a container
// anyway. The container is this run's, so the watch kills the run.
func TestContainerWatchBlamesAContainerTheSlotFreeRunStarted(t *testing.T) {
	const stray = "dolt/dolt-sql-server:2.2.0 reaper_abc123"
	containers := []slot.GateContainer{}
	w, cancels := pollWatch(t,
		func() ([]slot.GateContainer, error) { return containers, nil },
		neverHeld)

	containers = gateContainersFor(stray)
	w.poll() // first sight: a candidate, not yet a finding
	if got := w.strayContainers(); len(got) != 0 {
		t.Fatalf("blamed %v on one poll, want none: a container on its way out of a releasing holder's suite must not be blamed", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Fatalf("cancelled the run (%d) on one poll, want 0", n)
	}

	w.poll() // still there with no holder: this run started it
	if got := w.strayContainers(); len(got) != 1 || got[0] != stray {
		t.Fatalf("stray = %v, want [%s]", got, stray)
	}
	if n := cancels.Load(); n != 1 {
		t.Fatalf("cancel calls = %d, want 1", n)
	}

	// A second container later does not re-blame or re-cancel: the gate has
	// already failed, and the first finding is the one reported.
	containers = gateContainersFor(stray, "testcontainers/ryuk:0.13.0 reaper_def456")
	w.poll()
	if got := w.strayContainers(); len(got) != 1 || got[0] != stray {
		t.Errorf("stray = %v after a second container, want the first finding [%s]", got, stray)
	}
	if n := cancels.Load(); n != 1 {
		t.Errorf("cancel calls = %d after a second container, want 1", n)
	}
}

// TestContainerWatchIgnoresAContainerThatWasAlreadyRunning pins the baseline's
// job: a container that predates the run is not this run's, which is what
// keeps pre-existing debris (gt-ul1k) from failing an innocent gate.
func TestContainerWatchIgnoresAContainerThatWasAlreadyRunning(t *testing.T) {
	const stale = "dolt/dolt-sql-server:2.2.0 reaper_old"
	containers := gateContainersFor(stale)
	w, cancels := pollWatch(t,
		func() ([]slot.GateContainer, error) { return containers, nil },
		neverHeld)
	w.baseline = containerNameSet(containerDisplays(containers))

	for i := 0; i < 4; i++ {
		w.poll()
	}
	if got := w.strayContainers(); len(got) != 0 {
		t.Errorf("blamed %v, want none: the container was running before the gate's run", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Errorf("cancel calls = %d, want 0", n)
	}
}

// TestContainerWatchIgnoresAContainerALiveHolderOwns is the false positive
// that would fail the whole town's gates: the refinery's batch gate holds the
// token and starts containers while this slot-free run is in flight. Those
// containers are its holder's, not this run's.
func TestContainerWatchIgnoresAContainerALiveHolderOwns(t *testing.T) {
	const theirs = "dolt/dolt-sql-server:2.2.0 reaper_refinery"
	containers := gateContainersFor(theirs)
	log := watchLog(t)
	w, cancels := pollWatch(t,
		func() ([]slot.GateContainer, error) { return containers, nil },
		func(string) (bool, error) { return true, nil })
	w.logFile = log

	for i := 0; i < 4; i++ {
		w.poll()
	}
	if got := w.strayContainers(); len(got) != 0 {
		t.Errorf("blamed %v, want none: another owner holds the container-gate slot", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Errorf("cancel calls = %d, want 0", n)
	}
	if text := watchLogText(t, log); !strings.Contains(text, "another owner holds the container-gate slot") {
		t.Errorf("the log does not account for the container it saw:\n%s", text)
	}
}

// TestContainerWatchGoesQuietWhenTheSlotStateIsUnreadable: a lock state the
// watch cannot read is one it cannot attribute a container through. Staying
// quiet is the conservative direction, but the reason has to reach the log —
// a watch that could not check must not read as one that checked and found
// nothing.
func TestContainerWatchGoesQuietWhenTheSlotStateIsUnreadable(t *testing.T) {
	containers := gateContainersFor("dolt/dolt-sql-server:2.2.0 reaper_abc")
	log := watchLog(t)
	lockErr := errors.New("permission denied")
	w, cancels := pollWatch(t,
		func() ([]slot.GateContainer, error) { return containers, nil },
		func(string) (bool, error) { return true, lockErr })
	w.logFile = log

	for i := 0; i < 4; i++ {
		w.poll()
	}
	if got := w.strayContainers(); len(got) != 0 {
		t.Errorf("blamed %v, want none: the slot state was unreadable", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Errorf("cancel calls = %d, want 0", n)
	}
	text := watchLogText(t, log)
	if !strings.Contains(text, "permission denied") {
		t.Errorf("the log does not carry the unreadable-slot reason:\n%s", text)
	}
	if summary := w.summary(); !strings.Contains(summary, "incomplete") || !strings.Contains(summary, "permission denied") {
		t.Errorf("summary = %q, want it to report the watch was incomplete and why", summary)
	}
}

// TestContainerWatchCannotSeeAtAll: a listing that fails is not an empty
// listing. The watch reports it and does not fail the run on it.
func TestContainerWatchCannotSeeAtAll(t *testing.T) {
	log := watchLog(t)
	w, cancels := pollWatch(t,
		func() ([]slot.GateContainer, error) { return nil, errors.New("docker ps did not respond within 5s") },
		neverHeld)
	w.logFile = log

	for i := 0; i < 3; i++ {
		w.poll()
	}
	if got := w.strayContainers(); len(got) != 0 {
		t.Errorf("blamed %v on an unreadable listing, want none", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Errorf("cancel calls = %d, want 0", n)
	}
	if summary := w.summary(); !strings.Contains(summary, "did not respond within 5s") {
		t.Errorf("summary = %q, want it to name the failed listing", summary)
	}
}

// TestStartGateContainerWatchWithoutABaselineDoesNotWatch: with no baseline
// the watch cannot tell this run's container from one that was already there.
// It refuses to guess, says so in the log, and returns no watch — the town's
// `gt slot status` is the detector for that state.
func TestStartGateContainerWatchWithoutABaselineDoesNotWatch(t *testing.T) {
	stubContainerWatchInterval(t, time.Millisecond)
	stubGateContainerListing(t, func() ([]slot.GateContainer, error) {
		return nil, errors.New("docker ps failed: permission denied while trying to connect to the Docker daemon socket")
	})
	log := watchLog(t)

	got := startGateContainerWatch(context.Background(), func() {}, t.TempDir(), log)
	if got != nil {
		t.Fatalf("startGateContainerWatch = %+v, want nil when the baseline cannot be taken", got)
	}
	if text := watchLogText(t, log); !strings.Contains(text, "will not fail this run") {
		t.Errorf("the log does not say the watch stood down:\n%s", text)
	}
}

// TestStartGateContainerWatchDisabledByInterval keeps the escape hatch honest:
// a zero interval is how the gate's own tests keep the watch from shelling out
// to docker.
func TestStartGateContainerWatchDisabledByInterval(t *testing.T) {
	stubContainerWatchInterval(t, 0)
	if got := startGateContainerWatch(context.Background(), func() {}, t.TempDir(), watchLog(t)); got != nil {
		t.Fatalf("startGateContainerWatch = %+v, want nil when the interval is 0", got)
	}
}

// TestSlotFreeGateRunFailsWhenTheSuiteStartsAContainer is the end-to-end
// wiring: the gate decides slot-free from the rig's command text, its suite
// starts a container anyway, and the gate fails naming the container and the
// fix — rather than reporting a test failure or a slot problem.
func TestSlotFreeGateRunFailsWhenTheSuiteStartsAContainer(t *testing.T) {
	townRoot := t.TempDir()
	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	const stray = "dolt/dolt-sql-server:2.2.0 reaper_abc123"
	runStarted := make(chan struct{})
	slotTaken := false
	stubVerifyGate(t,
		func(townRoot, role string, timeout time.Duration) (func(), error) {
			slotTaken = true
			return func() {}, nil
		},
		func(ctx context.Context, _ string, script string, _ []string, _ *os.File) error {
			close(runStarted)
			<-ctx.Done()
			return ctx.Err()
		})
	stubContainerWatchInterval(t, 5*time.Millisecond)
	stubGateSlotHeld(t, false)
	// Empty until the suite is under way, then a container: the baseline is
	// the empty listing, so the container is this run's.
	stubGateContainerListing(t, func() ([]slot.GateContainer, error) {
		select {
		case <-runStarted:
			return gateContainersFor(stray), nil
		default:
			return nil, nil
		}
	})

	mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=8 make test"}
	g := git.NewGit(dir)
	_, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/watch-role")
	if err == nil {
		t.Fatal("the gate passed a slot-free run whose suite started a container (gt-0ss4)")
	}
	if slotTaken {
		t.Error("the gate took the slot on the command-text decision, so there was nothing for the watch to catch")
	}
	for _, want := range []string{stray, "only a slot holder may use it", dockerTestsEnv + "=1 make test", "not a test result"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
	logBytes, readErr := os.ReadFile(testVerifyLogPath(dir))
	if readErr != nil {
		t.Fatalf("reading verify log: %v", readErr)
	}
	if logText := string(logBytes); !strings.Contains(logText, stray) {
		t.Errorf("the verify log does not name the container that failed the run:\n%s", logText)
	}
}

// TestSlotFreeGateRunRecordsTheContainerWatchEvidence pins the other half of
// the observable: a slot-free run that starts no container says so in the
// artifact, which is what makes the decision to skip the slot a checked fact
// rather than an assumption (gt-0ss4).
func TestSlotFreeGateRunRecordsTheContainerWatchEvidence(t *testing.T) {
	townRoot := t.TempDir()
	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	stubVerifyGate(t, nil, func(_ context.Context, _ string, _ string, _ []string, _ *os.File) error {
		return nil
	})
	stubContainerWatchInterval(t, 5*time.Millisecond)
	stubGateSlotHeld(t, false)
	stubGateContainerListing(t, func() ([]slot.GateContainer, error) { return nil, nil })

	mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=8 make test"}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/watch-clean-role")
	if err != nil {
		t.Fatalf("runDefaultTestVerification: %v", err)
	}
	logBytes, readErr := os.ReadFile(result.logPath)
	if readErr != nil {
		t.Fatalf("reading verify log: %v", readErr)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "container watch: no container appeared in") {
		t.Errorf("the verify log does not record the container watch's evidence:\n%s", logText)
	}
	if !strings.Contains(logText, "a container that starts anyway fails this run") {
		t.Errorf("the header does not say the slot-free run is watched:\n%s", logText)
	}
}
