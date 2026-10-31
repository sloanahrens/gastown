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
// limit, when set, is the most the clock may advance before the test fails —
// a bound is what the wait is supposed to have, and a wait without one never
// returns, which a test would see as a hang rather than a failure.
type bondFakeClock struct {
	t       *testing.T
	limit   time.Duration
	onSleep func()

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
	waited := c.now.Sub(c.zero)
	c.mu.Unlock()
	if c.limit > 0 && waited > c.limit {
		c.t.Fatalf("bond waited %s, past its bound of %s", waited, c.limit)
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
	contended := make(chan struct{}, 1)
	secondClock.onSleep = func() {
		select {
		case contended <- struct{}{}:
		default:
		}
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

	select {
	case <-contended:
	//testpolicy:allow no-sleep — bound the wait for a bond contending on the lock, which fails rather than hangs
	case <-time.After(5 * time.Second):
		t.Fatal("the second bond never waited for the town lock while the first held it")
	}
	release()
	<-done
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
	clock.limit = bondLockWait + bondLockRetryInterval
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

// TestBondRefusesWhenTheLockCannotBeOpened: a town whose lock directory cannot
// be made fails the bond rather than bonding without the lock, which would be
// the race the lock is there to stop.
func TestBondRefusesWhenTheLockCannotBeOpened(t *testing.T) {
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
