package git

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// WithTimeout bounds every call that sets no deadline of its own, and leaves
// the original Git alone.
func TestWithTimeoutAppliesTheDefault(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"add -A":                      ok(""),
		"hash-object -w --stdin":      ok("abc\n"),
		"status --porcelain -uall":    ok(""),
		"rev-parse --abbrev-ref HEAD": ok("main\n"),
	})
	base := newTestGit(t, s)
	g := base.WithTimeout(7 * time.Second)
	if err := g.Add("-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.runWithStdin("x", "hash-object", "-w", "--stdin"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Status(); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{"add -A", "hash-object -w --stdin", "status --porcelain -uall"} {
		if c, found := s.sentCall(args); !found || c.timeout != 7*time.Second {
			t.Errorf("%s ran with timeout %v (found %v), want 7s", args, c.timeout, found)
		}
	}
	if _, err := base.CurrentBranch(); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.sentCall("rev-parse --abbrev-ref HEAD"); c.timeout != 0 {
		t.Errorf("the original Git ran with timeout %v; WithTimeout must return a copy", c.timeout)
	}
}

// A method's own deadline wins over the WithTimeout default.
func TestWithTimeoutExplicitDeadlineWins(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"push origin main": ok(""), "fetch origin main": ok(""), "push origin backup": ok("")})
	g := newTestGit(t, s).WithTimeout(7 * time.Second)
	if err := g.Push("origin", "main", false); err != nil {
		t.Fatal(err)
	}
	if err := g.FetchRefspecWithTimeout("origin", "main", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.PushWithTimeout("origin", "backup", false, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]time.Duration{"push origin main": pushTimeout, "fetch origin main": 3 * time.Second, "push origin backup": 2 * time.Minute} {
		if c, found := s.sentCall(args); !found || c.timeout != want {
			t.Errorf("%s ran with timeout %v (found %v), want its own %v", args, c.timeout, found, want)
		}
	}
}

// Without WithTimeout, calls run exactly as before: no deadline where a method
// sets none.
func TestZeroTimeoutIsUnchanged(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"add -A": ok("")})
	if err := newTestGit(t, s).WithTimeout(0).Add("-A"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.sentCall("add -A"); c.timeout != 0 {
		t.Errorf("add ran with timeout %v, want none", c.timeout)
	}
}

// A call killed at a WithTimeout deadline fails with an error matching
// ErrTimedOut, whichever run path it took.
func TestWithTimeoutKilledCallMatchesErrTimedOut(t *testing.T) {
	t.Parallel()
	g := (&Git{workDir: t.TempDir(), exec: func(gitCall) (string, string, error) {
		return "", "", errTimedOut
	}}).WithTimeout(time.Second)
	for name, call := range map[string]func() error{
		"Add":    func() error { return g.Add("-A") },
		"Commit": func() error { return g.Commit("m") },
		"Push":   func() error { return g.Push("origin", "main", false) },
		"Fetch":  func() error { return g.FetchRefspecWithTimeout("origin", "main", time.Second) },
		"stdin":  func() error { _, err := g.runWithStdin("x", "hash-object", "--stdin"); return err },
	} {
		err := call()
		if !errors.Is(err, ErrTimedOut) {
			t.Errorf("%s error = %v, want one matching ErrTimedOut", name, err)
		}
		var ge *GitError
		if errors.As(err, &ge) {
			t.Errorf("%s: a timeout must not look like git's own failure: %+v", name, ge)
		}
	}
}

// WithEnv adds its environment to every call, beneath a call's own.
func TestWithEnvAddsToEveryCall(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"add -A": ok(""), "push origin main": ok("")})
	g := newTestGit(t, s).WithEnv([]string{"USER=daemon"})
	if err := g.Add("-A"); err != nil {
		t.Fatal(err)
	}
	if err := g.PushWithEnv("origin", "main", false, []string{"GT_X=1"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.sentCall("add -A"); !slices.Equal(c.env, []string{"USER=daemon"}) {
		t.Errorf("add env = %q, want USER=daemon", c.env)
	}
	if c, _ := s.sentCall("push origin main"); !slices.Equal(c.env, []string{"USER=daemon", "GT_X=1"}) {
		t.Errorf("push env = %q, want USER=daemon then the call's GT_X=1", c.env)
	}
}

// ErrTimedOut is the runner's own sentinel, so a runner error and a Git
// error both match it.
func TestErrTimedOutIsTheRunnerSentinel(t *testing.T) {
	t.Parallel()
	if !errors.Is(errTimedOut, ErrTimedOut) || !errors.Is(&timeoutError{command: "add"}, errTimedOut) {
		t.Fatal("ErrTimedOut and the runner's timeout sentinel must match each other")
	}
}
