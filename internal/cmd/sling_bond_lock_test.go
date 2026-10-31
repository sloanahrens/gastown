package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// bondFakeClock is the clock and sleep a bond's lock wait reads, spent in no
// real time: every sleep advances it by its pause and records the pause.
//
// maxSleeps, when set, is the most pauses a wait may take before the test
// fails. A wait with no bound does not sit in the kernel here, it spins, so
// without this cap the test would hang rather than fail. Only a test that
// drives the wait from its own goroutine may set it, since the failure comes
// from t.Fatalf.
//
// onSleep runs after each pause. A contending bond spins on that pause, so a
// test that blocks there holds the waiter still and decides when its time
// moves; without it the spin would spend the whole bound between two lines of
// the test, and the wait would time out before the test could free the lock.
type bondFakeClock struct {
	t         *testing.T
	maxSleeps int
	onSleep   func()

	mu    sync.Mutex
	zero  time.Time
	now   time.Time
	slept []time.Duration
}

func newBondFakeClock(t *testing.T) *bondFakeClock {
	t.Helper()
	now := time.Unix(0, 0)
	return &bondFakeClock{t: t, zero: now, now: now}
}

func (c *bondFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *bondFakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	n := len(c.slept)
	c.mu.Unlock()
	if c.maxSleeps > 0 && n > c.maxSleeps {
		c.t.Fatalf("bond took %d pauses, past the %d a bounded wait may take", n, c.maxSleeps)
	}
	if c.onSleep != nil {
		c.onSleep()
	}
}

// waited is how far the clock has been carried: the bond's whole pause.
func (c *bondFakeClock) waited() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.zero)
}

// pauses is every pause the bond asked for, in order.
func (c *bondFakeClock) pauses() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// bondBD is a formulaBD over fake whose bond waits run on clock.
func bondBD(fake *fakeCook, clock *bondFakeClock) formulaBD {
	return formulaBD{open: fake.open, sleep: clock.Sleep, now: clock.Now}
}

// TestBondSerializesAcrossProcesses: two slings bonding at once take turns on
// the town lock, so bd never runs two bonds against the same tables, which is
// how the slings that started together all lost their commit (gt-40twy).
//
// The two formulaBD values open the lock file separately, the way two sling
// processes do: flock admits one holder per open file description, and that
// is the contention it reports between processes too.
func TestBondSerializesAcrossProcesses(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	var inFlight, overlaps, calls atomic.Int32
	firstInBond := make(chan struct{})
	releaseBond := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBond) }) }
	defer release()

	fake := &fakeCook{bond: func(int) ([]byte, error) {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		defer inFlight.Add(-1)
		if calls.Add(1) == 1 {
			// Hold the first bond where the lock is held, inside bd.
			close(firstInBond)
			<-releaseBond
		}
		return []byte(`{"result_id":"gt-wisp-serial"}`), nil
	}}

	firstClock := newBondFakeClock(t)
	secondClock := newBondFakeClock(t)
	// The second bond's wait parks here once it has contended: onSleep holds it
	// until the first bond has landed and let the lock go, so the fake clock
	// cannot carry the waiter through its bound while the test is still
	// arranging the release.
	contended := make(chan struct{}, 1)
	held := make(chan struct{})
	var heldOnce sync.Once
	letGo := func() { heldOnce.Do(func() { close(held) }) }
	defer letGo()
	secondClock.onSleep = func() {
		select {
		case contended <- struct{}{}:
		default:
		}
		<-held
	}

	var firstRoot, secondRoot string
	var firstErr, secondErr error
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		firstRoot, firstErr = bondBD(fake, firstClock).bond(
			"mol-polecat-work", "mol-polecat-work", "gt-first", townRoot, townRoot, []string{"feature=a"})
	}()
	<-firstInBond

	go func() {
		defer func() { done <- struct{}{} }()
		secondRoot, secondErr = bondBD(fake, secondClock).bond(
			"mol-polecat-work", "mol-polecat-work", "gt-second", townRoot, townRoot, []string{"feature=b"})
	}()

	// Either the second bond waits on the lock — the behaviour — or it reaches
	// bd while the first is still inside, which is the mutation. Both are
	// events, so this needs no timer: the first bond cannot finish before
	// release, so a done here is the second, and it did not wait.
	select {
	case <-contended:
	case <-done:
		t.Fatal("the second bond never waited for the town lock while the first held it")
	}
	release()
	<-done // the first bond landed, and its lock went with it
	letGo()
	<-done

	if firstErr != nil || secondErr != nil {
		t.Fatalf("bonds: first %v, second %v; want both to land", firstErr, secondErr)
	}
	if firstRoot != "gt-wisp-serial" || secondRoot != "gt-wisp-serial" {
		t.Errorf("roots = %q, %q; want gt-wisp-serial both", firstRoot, secondRoot)
	}
	if n := overlaps.Load(); n != 0 {
		t.Errorf("%d bonds ran while another was in bd, want 0", n)
	}
}

// TestBondLockWaitIsBounded: a lock another sling never releases ends the
// bond in an error within the bound instead of a hung sling (gt-40twy).
func TestBondLockWaitIsBounded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	lockDir := filepath.Dir(bondLockPath(townRoot))
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatalf("mkdir the lock dir: %v", err)
	}
	held, err := lock.FlockTryAcquireStable(bondLockPath(townRoot))
	if err != nil {
		t.Fatalf("take the bond lock: %v", err)
	}
	defer held()

	clock := newBondFakeClock(t)
	// One pause per retry interval is the most a full wait may take; the cap
	// is that count, so a wait that never ends fails here instead of spinning.
	clock.maxSleeps = int(bondLockWait/bondLockRetryInterval) + 1
	fake := formulaBDFake(`{"result_id":"gt-wisp-x"}`)

	_, err = bondBD(fake, clock).bond(
		"mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if !errors.Is(err, ErrBondLockTimeout) {
		t.Fatalf("bond on a held lock: err = %v, want ErrBondLockTimeout", err)
	}
	if n := len(fake.called("mol bond ")); n != 0 {
		t.Errorf("bond ran %d attempts while the lock was held, want 0", n)
	}
	if waited := clock.waited(); waited > bondLockWait || waited < bondLockWait-bondLockRetryInterval {
		t.Errorf("waited %s, want a full wait of %s", waited, bondLockWait)
	}
}

// TestBondRefusesWhenTheLockDirCannotBeMade: a town whose lock directory
// cannot be made fails the bond rather than bonding without the lock, which
// would be the race the lock is there to stop.
func TestBondRefusesWhenTheLockDirCannotBeMade(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), []byte("a file where the lock dir belongs"), 0644); err != nil {
		t.Fatalf("plant the file: %v", err)
	}

	clock := newBondFakeClock(t)
	fake := formulaBDFake(`{"result_id":"gt-wisp-x"}`)

	_, err := bondBD(fake, clock).bond(
		"mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err == nil {
		t.Fatal("bond ran without the town lock")
	}
	if errors.Is(err, ErrBondLockTimeout) {
		t.Errorf("err = %v, want the directory failure, not a timeout", err)
	}
	if n := len(fake.called("mol bond ")); n != 0 {
		t.Errorf("bond ran %d attempts without the lock, want 0", n)
	}
}

// TestBondRefusesWhenTheLockFileWillNotOpen: a lock file the kernel refuses to
// open fails the bond too, rather than reading as contention and waiting out
// the whole bound for a lock nobody holds.
func TestBondRefusesWhenTheLockFileWillNotOpen(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A directory where the lock file belongs is the one open failure the test
	// can stage without changing permissions of the tree it runs in.
	if err := os.MkdirAll(bondLockPath(townRoot), 0755); err != nil {
		t.Fatalf("plant the lock path: %v", err)
	}

	clock := newBondFakeClock(t)
	fake := formulaBDFake(`{"result_id":"gt-wisp-x"}`)

	_, err := bondBD(fake, clock).bond(
		"mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err == nil {
		t.Fatal("bond ran without the town lock")
	}
	if errors.Is(err, ErrBondLockTimeout) {
		t.Errorf("err = %v, want the open failure, not a timeout", err)
	}
	if waited := clock.waited(); waited != 0 {
		t.Errorf("waited %s on an open failure, want no wait", waited)
	}
	if n := len(fake.called("mol bond ")); n != 0 {
		t.Errorf("bond ran %d attempts without the lock, want 0", n)
	}
}

// TestBondBackoffHasJitter: the pause before a retried bond is drawn from the
// injected source, so slings that lost the same commit do not come back in
// step (gt-40twy). Jitter 0 and 0.5 are the ends the ±25% band makes exact:
// 100ms*0.75 and 200ms*1.0.
func TestBondBackoffHasJitter(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	fake := &fakeCook{bond: func(n int) ([]byte, error) {
		if n < 3 {
			return nil, errors.New("creating wisp: sql commit (regular): Error 1213 (40001): serialization failure")
		}
		return []byte(`{"result_id":"gt-wisp-jitter"}`), nil
	}}

	jitters := []float64{0, 0.5}
	var drawn atomic.Int32
	clock := newBondFakeClock(t)
	bd := bondBD(fake, clock)
	bd.jitter = func() float64 {
		return jitters[int(drawn.Add(1)-1)%len(jitters)]
	}

	root, err := bd.bond("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err != nil {
		t.Fatalf("bond: %v", err)
	}
	if root != "gt-wisp-jitter" {
		t.Errorf("root = %q, want gt-wisp-jitter", root)
	}

	pauses := clock.pauses()
	want := []time.Duration{75 * time.Millisecond, 200 * time.Millisecond}
	if len(pauses) != len(want) {
		t.Fatalf("pauses = %v, want one per lost attempt (%v)", pauses, want)
	}
	for i := range want {
		if pauses[i] != want[i] {
			t.Errorf("pause %d = %s, want %s for jitter %.2f", i+1, pauses[i], want[i], jitters[i])
		}
	}
}
