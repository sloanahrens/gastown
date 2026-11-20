package git

import (
	"fmt"
	"time"
)

// ErrTimedOut is matched (errors.Is) by the error of a git call that ran out
// its deadline and was killed.
var ErrTimedOut = errTimedOut

// timeoutError is a call killed at its deadline. Its text is the one these
// errors have always carried; Unwrap lets a caller match ErrTimedOut.
type timeoutError struct {
	command string
	after   time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("git %s timed out after %v (remote may be unreachable)", e.command, e.after)
}

func (e *timeoutError) Unwrap() error { return ErrTimedOut }

// WithTimeout returns a copy of g whose calls are killed after d, the
// receiver left as it was. A method that sets its own deadline (a push's, a
// fetch's) keeps it; d bounds every call that sets none. Zero is today's
// behavior: no deadline. A killed call's error matches ErrTimedOut.
func (g *Git) WithTimeout(d time.Duration) *Git {
	c := *g
	c.timeout = d
	return &c
}

// deadline is the timeout a call runs under: the method's own when it sets
// one, else the WithTimeout default (0 means none).
func (g *Git) deadline(own time.Duration) time.Duration {
	if own > 0 {
		return own
	}
	return g.timeout
}

// WithEnv returns a copy of g whose every call runs with env added to the
// process environment, a later entry for a key replacing an earlier one.
func (g *Git) WithEnv(env []string) *Git {
	c := *g
	c.env = append(append([]string(nil), g.env...), env...)
	return &c
}

// WithoutEnv returns a copy of g whose every call runs with keys taken out of
// the process environment, the receiver left as it was. It is for a call that
// must not inherit a variable pointing it at another repository: an inherited
// GIT_DIR makes every command act on the wrong one (rpkg #769), and exporting
// the key empty does not clear it (see callEnv).
func (g *Git) WithoutEnv(keys ...string) *Git {
	c := *g
	c.unset = append(append([]string(nil), g.unset...), keys...)
	return &c
}

// WithoutLocalHooks returns a copy of g whose calls run with the repository's
// local git hooks off, the receiver left as it was; a push keeps them, so the
// pre-push fence stays on. A tracked hook that runs bd fails every merge and
// commit in a pod where bd refuses a call with no BEADS_DIR (gt-7efzug).
func (g *Git) WithoutLocalHooks() *Git {
	c := *g
	c.noHooks = true
	return &c
}

// WithoutAllHooks returns a copy of g whose every call runs with the
// repository's local git hooks off, a push's pre-push included, the receiver
// left as it was. It is for the one legitimate pusher, the landing worker,
// whose pushes must not run a pre-push hook a pod's bd gateway refuses; a
// polecat or crew handle takes WithoutLocalHooks instead, so the fence stays
// on for its pushes (gt-gh0vkk).
func (g *Git) WithoutAllHooks() *Git {
	c := *g
	c.noHooks = true
	c.noPushHooks = true
	return &c
}
