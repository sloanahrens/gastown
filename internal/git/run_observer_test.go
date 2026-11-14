package git

import (
	"testing"
	"time"
)

// answerEvery is an exec runner that answers every git process with err.
func answerEvery(err error) runFunc {
	return func(gitCall) (string, string, error) { return "", "", err }
}

func TestSlingCallsCountEachGitProcess(t *testing.T) {
	t.Parallel()
	var count int
	g := (&Git{workDir: t.TempDir(), exec: answerEvery(nil)}).WithRunObserver(func() { count++ })

	if !g.IsRepo() || !g.IsRepo() {
		t.Fatal("IsRepo = false, want true from a runner that succeeds")
	}
	if count != 2 {
		t.Fatalf("observer counted %d git processes for two calls, want 2", count)
	}
}

func TestSlingCallsCountAFailedProcess(t *testing.T) {
	t.Parallel()
	var count int
	g := (&Git{workDir: t.TempDir(), exec: answerEvery(gitExit(128))}).WithRunObserver(func() { count++ })

	if g.IsRepo() {
		t.Fatal("IsRepo = true, want false from a runner that exits 128")
	}
	if count != 1 {
		t.Fatalf("observer counted %d git processes for one failed call, want 1", count)
	}
}

func TestSlingCallsNilObserverCountsNothingAndStillRuns(t *testing.T) {
	t.Parallel()
	var ran int
	g := (&Git{workDir: t.TempDir(), exec: func(gitCall) (string, string, error) {
		ran++
		return "", "", nil
	}}).WithRunObserver(nil)

	if !g.IsRepo() {
		t.Fatal("IsRepo = false with a nil observer, want the call's own result")
	}
	if ran != 1 {
		t.Fatalf("runner ran %d times with a nil observer, want 1", ran)
	}
}

func TestSlingCallsCopyKeepsTheObserver(t *testing.T) {
	t.Parallel()
	var count int
	g := (&Git{workDir: t.TempDir(), exec: answerEvery(nil)}).
		WithRunObserver(func() { count++ }).
		WithEnv([]string{"GT_TEST=1"}).
		WithTimeout(time.Second)

	g.IsRepo()
	if count != 1 {
		t.Fatalf("observer counted %d after a copy made by WithEnv and WithTimeout, want 1", count)
	}
}

func TestSlingCallsTwoHandlesCountSeparately(t *testing.T) {
	t.Parallel()
	var first, second int
	base := &Git{workDir: t.TempDir(), exec: answerEvery(nil)}
	a := base.WithRunObserver(func() { first++ })
	b := base.WithRunObserver(func() { second++ })

	a.IsRepo()
	a.IsRepo()
	b.IsRepo()
	if first != 2 || second != 1 {
		t.Fatalf("handles counted %d and %d, want 2 and 1: one sling's calls must not land on another's count", first, second)
	}
}

// TestSlingCallsSparseCheckoutCountsThroughTheHandle drives InitSparseCheckout
// on an observed handle: the town-root guard's rev-parse and the two
// sparse-checkout calls each count (gt-74tzuk).
func TestSlingCallsSparseCheckoutCountsThroughTheHandle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := newScripted(map[string]reply{
		"-C " + dir + " rev-parse --show-toplevel":   ok(dir),
		"-C " + dir + " sparse-checkout init --cone": ok(""),
		"-C " + dir + " sparse-checkout set one two": ok(""),
	})
	var count int
	g := (&Git{exec: script.run}).WithRunObserver(func() { count++ })

	if err := g.InitSparseCheckout(dir, []string{"one", "two"}); err != nil {
		t.Fatalf("InitSparseCheckout: %v", err)
	}
	if count != 3 {
		t.Fatalf("observer counted %d git processes for the guard and the two sparse-checkout calls, want 3", count)
	}
}

func TestSlingCallsCountsARetryAsAnotherProcess(t *testing.T) {
	t.Parallel()
	var count int
	r := &configReadRunner{attempts: []configReadAttempt{
		{stderr: configReadFatal, err: gitExit(128)},
		{},
	}}
	g := (&Git{workDir: t.TempDir(), exec: r.run}).WithRunObserver(func() { count++ }).WithConfigReadRetry()
	g.retryWait = func(time.Duration) {}

	if !g.IsRepo() {
		t.Fatal("IsRepo = false, want the retry's success")
	}
	if count != 2 || r.calls != 2 {
		t.Fatalf("observer counted %d processes and the runner ran %d times, want 2 and 2", count, r.calls)
	}
}
