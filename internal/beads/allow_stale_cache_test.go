package beads

import (
	"context"
	"errors"
	"testing"
)

// TestAllowStaleCacheKeepsOnlyDefinitiveAnswers is gt-22hdp.27: a probe that
// timed out under load was cached as "unsupported" for the life of the
// process, so a long-running daemon dropped --allow-stale from every later
// bd call. A timeout now answers "unsupported" for that call only; the next
// call probes again. A definitive "unknown flag" is still cached.
func TestAllowStaleCacheKeepsOnlyDefinitiveAnswers(t *testing.T) {
	t.Parallel()
	var c allowStaleCache
	probes := 0
	timedOut := func() (bool, bool) { probes++; return false, false }
	ok := func() (bool, bool) { probes++; return true, true }

	if c.supported("/bin/bd", timedOut) {
		t.Fatal("a timed-out probe reported supported")
	}
	if !c.supported("/bin/bd", ok) {
		t.Fatal("after a timeout, the next call did not probe again and use the flag")
	}
	if !c.supported("/bin/bd", timedOut) || probes != 2 {
		t.Errorf("a definitive answer was not cached: %d probes", probes)
	}

	var d allowStaleCache
	unknownFlag := func() (bool, bool) { probes++; return false, true }
	probes = 0
	d.supported("/bin/bd", unknownFlag)
	if d.supported("/bin/bd", ok) || probes != 1 {
		t.Errorf("a definitive unknown flag was not cached: %d probes", probes)
	}
	if !d.supported("/other/bd", ok) {
		t.Error("a different bd path reused the cached answer")
	}
}

func TestAllowStaleAnswer(t *testing.T) {
	t.Parallel()
	live := context.Background()
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name                string
		ctx                 context.Context
		err                 error
		out                 string
		supported, definite bool
	}{
		{"accepted", live, nil, "bd version 1.2.2", true, true},
		{"unknown flag exit 0", live, nil, "Error: unknown flag: --allow-stale", false, true},
		{"unknown flag nonzero exit", live, exitError{1}, "Error: unknown flag: --allow-stale", false, true},
		// A bd that exits nonzero for any other reason (a lock held, a
		// database it cannot reach, a crash) said nothing about the flag:
		// caching that as "unsupported" drops --allow-stale from every later
		// call for the life of the process (gt-22hdp.27).
		{"nonzero exit, other error", live, exitError{1}, "Error: failed to open database: lock held", false, false},
		{"nonzero exit, no output", live, exitError{2}, "", false, false},
		{"exit 0, no output", live, nil, "", false, true},
		{"timed out", expired, errors.New("signal: killed"), "", false, false},
		{"did not start", live, errors.New("fork/exec: resource temporarily unavailable"), "", false, false},
	} {
		s, d := allowStaleAnswer(tc.ctx, tc.err, tc.out)
		if s != tc.supported || d != tc.definite {
			t.Errorf("%s: = %v, %v; want %v, %v", tc.name, s, d, tc.supported, tc.definite)
		}
	}
}
