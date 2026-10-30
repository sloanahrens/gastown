package forgejoresync

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// receiveBound is the most a test waits for a seam to report. A regression fails the
// test at this bound instead of hanging the package.
const receiveBound = 10 * time.Second

// awaitReceive reads one value from ch, failing the test when nothing arrives within
// receiveBound. Every wait on a seam's channel goes through it, so a seam that stops
// reporting fails fast.
func awaitReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), receiveBound)
	defer cancel()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("nothing arrived on the seam's channel within %v", receiveBound)
	}
	var zero T
	return zero
}

// TestParseRefsSortsInByteOrder pins the order the script's LC_ALL=C sort gave:
// uppercase before lowercase, branches before tags, and "v1.10" before "v1.9".
// A case-insensitive sort puts "alpha" before "Zeta" and fails here.
func TestParseRefsSortsInByteOrder(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		out  string
		want []Ref
	}{
		{
			name: "empty listing",
			out:  "",
			want: nil,
		},
		{
			name: "unsorted branches and tags",
			out: sha + " refs/tags/v1.9\n" +
				sha + " refs/heads/main\n" +
				sha + " refs/heads/alpha\n" +
				sha + " refs/tags/v1.10\n",
			want: []Ref{
				{Name: "refs/heads/alpha", SHA: sha},
				{Name: "refs/heads/main", SHA: sha},
				{Name: "refs/tags/v1.10", SHA: sha},
				{Name: "refs/tags/v1.9", SHA: sha},
			},
		},
		{
			name: "uppercase sorts before lowercase",
			out: sha + " refs/heads/alpha\n" +
				sha + " refs/heads/Zeta\n",
			want: []Ref{
				{Name: "refs/heads/Zeta", SHA: sha},
				{Name: "refs/heads/alpha", SHA: sha},
			},
		},
		{
			name: "blank lines are skipped",
			out:  "\n" + sha + " refs/heads/main\n\n",
			want: []Ref{{Name: "refs/heads/main", SHA: sha}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRefs([]byte(tc.out))
			if err != nil {
				t.Fatalf("ParseRefs: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseRefs order = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseRefsRefusesMalformedLine drives the refusal branch: a line that is
// not "<sha> <ref>" is an error, so a dropped ref cannot reach the plan.
func TestParseRefsRefusesMalformedLine(t *testing.T) {
	t.Parallel()
	for _, out := range []string{
		"refs/heads/main\n",
		"0123 refs/heads/main extra\n",
	} {
		if _, err := ParseRefs([]byte(out)); err == nil {
			t.Fatalf("ParseRefs(%q) accepted a malformed line", out)
		}
	}
}

// TestPlan pins what a pruning push does: a GitHub-only ref is pushed, a
// Forgejo-only ref is deleted, a ref at another sha is pushed, and converged
// sets give nothing. Swapping push and delete turns the first two rows red.
func TestPlan(t *testing.T) {
	t.Parallel()
	const a = "aaaa"
	const b = "bbbb"
	cases := []struct {
		name       string
		github     []Ref
		forgejo    []Ref
		wantPush   []string
		wantDelete []string
	}{
		{
			name:     "ref only on GitHub is pushed",
			github:   []Ref{{Name: "refs/heads/new", SHA: a}},
			wantPush: []string{"refs/heads/new"},
		},
		{
			name:       "ref only on Forgejo is deleted",
			forgejo:    []Ref{{Name: "refs/heads/gone", SHA: a}},
			wantDelete: []string{"refs/heads/gone"},
		},
		{
			name:     "ref at a different sha is pushed, not deleted",
			github:   []Ref{{Name: "refs/heads/main", SHA: a}},
			forgejo:  []Ref{{Name: "refs/heads/main", SHA: b}},
			wantPush: []string{"refs/heads/main"},
		},
		{
			name:    "converged sets give nothing",
			github:  []Ref{{Name: "refs/heads/main", SHA: a}, {Name: "refs/tags/v1", SHA: b}},
			forgejo: []Ref{{Name: "refs/heads/main", SHA: a}, {Name: "refs/tags/v1", SHA: b}},
		},
		{
			name:       "push and delete together, each in its own list",
			github:     []Ref{{Name: "refs/heads/kept", SHA: a}, {Name: "refs/heads/added", SHA: a}},
			forgejo:    []Ref{{Name: "refs/heads/kept", SHA: a}, {Name: "refs/heads/removed", SHA: b}},
			wantPush:   []string{"refs/heads/added"},
			wantDelete: []string{"refs/heads/removed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			push, del := Plan(tc.github, tc.forgejo)
			if !slices.Equal(push, tc.wantPush) {
				t.Fatalf("push = %v, want %v", push, tc.wantPush)
			}
			if !slices.Equal(del, tc.wantDelete) {
				t.Fatalf("del = %v, want %v", del, tc.wantDelete)
			}
		})
	}
}

// fakeRunner records each argv it is handed and returns a canned result, so the
// exec implementation is checked without starting a process.
type fakeRunner struct {
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunner) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	return f.out, f.err
}

// TestGitExecArgs pins the exact git argument list each operation builds.
func TestGitExecArgs(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/acme/gastown.git"
	const dir = "scratch/refs.git"
	refspecs := []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	cases := []struct {
		name string
		call func(g ExecGit) error
		want []string
	}{
		{
			name: "LsRemote",
			call: func(g ExecGit) error { _, err := g.LsRemote(url); return err },
			want: []string{"git", "ls-remote", "--heads", "--tags", "--refs", "--", url},
		},
		{
			name: "InitBare",
			call: func(g ExecGit) error { return g.InitBare(dir) },
			want: []string{"git", "init", "--bare", "--quiet", dir},
		},
		{
			name: "Fetch",
			call: func(g ExecGit) error { return g.Fetch(dir, url, refspecs...) },
			want: []string{"git", "-C", dir, "fetch", "--quiet", "--", url, refspecs[0], refspecs[1]},
		},
		{
			name: "PushPrune",
			call: func(g ExecGit) error { return g.PushPrune(dir, url, refspecs...) },
			want: []string{"git", "-C", dir, "push", "--prune", "--quiet", "--", url, refspecs[0], refspecs[1]},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRunner{}
			if err := tc.call(ExecGit{Run: f.run}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(f.calls) != 1 || !slices.Equal(f.calls[0], tc.want) {
				t.Fatalf("%s argv = %q, want %q", tc.name, f.calls, tc.want)
			}
		})
	}
}

// TestGitExecEndsOptionsBeforeURL pins that a url which starts with "-" reaches
// git after a "--", so it is read as a repository and never as an option. Without
// the "--" the url below would be the first argument after the subcommand's
// options, where git takes it as a flag.
func TestGitExecEndsOptionsBeforeURL(t *testing.T) {
	t.Parallel()
	const hostile = "--upload-pack=touch /tmp/owned"
	const dir = "scratch/refs.git"
	cases := []struct {
		name string
		call func(g ExecGit) error
	}{
		{name: "LsRemote", call: func(g ExecGit) error { _, err := g.LsRemote(hostile); return err }},
		{name: "Fetch", call: func(g ExecGit) error { return g.Fetch(dir, hostile, Refspecs...) }},
		{name: "PushPrune", call: func(g ExecGit) error { return g.PushPrune(dir, hostile, Refspecs...) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRunner{}
			if err := tc.call(ExecGit{Run: f.run}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(f.calls) != 1 {
				t.Fatalf("%s made %d calls, want 1", tc.name, len(f.calls))
			}
			argv := f.calls[0]
			at := slices.Index(argv, hostile)
			if at < 1 || argv[at-1] != "--" {
				t.Fatalf("%s argv = %q, want %q directly before the url", tc.name, argv, "--")
			}
		})
	}
}

// TestGitCommandDisablesPrompt pins that a git process is built with terminal
// prompting off and with the argv it was given. The command is built, never
// started, so no process runs.
func TestGitCommandDisablesPrompt(t *testing.T) {
	t.Parallel()
	argv := []string{"git", "ls-remote", "--heads", "--", "https://github.com/acme/gastown.git"}
	cmd := gitCommand(context.Background(), argv[0], argv[1:]...)
	if !slices.Equal(cmd.Args, argv) {
		t.Fatalf("gitCommand args = %q, want %q", cmd.Args, argv)
	}
	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("gitCommand environment lacks GIT_TERMINAL_PROMPT=0, so a credential prompt could block the run")
	}
}

// TestGitExecCancelsAnInFlightCallWithTheRunContext drives the Runner seam: the
// runner a call runs through is handed the run's context, so canceling the run ends
// the call in flight. Mutation: run the call under context.Background() and the
// call's context is never done, so the test fails at its bound.
func TestGitExecCancelsAnInFlightCallWithTheRunContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	run := func(callCtx context.Context, _ []string) ([]byte, error) {
		close(started)
		<-callCtx.Done() // the run is canceled while this call is in flight
		return nil, callCtx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- ExecGit{Context: ctx, Run: run}.Fetch("scratch/refs.git", "https://github.com/acme/gastown.git", Refspecs...)
	}()
	awaitReceive(t, started)
	cancel()
	if err := awaitReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch error = %v, want context.Canceled", err)
	}
}

// TestGitRunnerDerivesEachCallFromTheRunContext drives the production starter path
// without a process: the call's context comes from the run's and carries its own
// bound, so a canceled run ends the call and the call still cannot outlive the
// bound. Mutation: derive the call's context from context.Background() and the
// call never ends on the run's cancellation; drop the bound and the deadline check
// goes red.
func TestGitRunnerDerivesEachCallFromTheRunContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callContexts := make(chan context.Context, 1)
	start := func(callCtx context.Context, _ string, _ ...string) ([]byte, error) {
		callContexts <- callCtx
		<-callCtx.Done() // a call in flight ends with its context
		return nil, callCtx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := gitRunnerWith(time.Hour, start)(ctx, []string{"git", "fetch", "--quiet"})
		done <- err
	}()
	callCtx := awaitReceive(t, callContexts)
	if _, ok := callCtx.Deadline(); !ok {
		t.Fatal("the call's context has no bound, want one derived from the git timeout")
	}
	cancel()
	if err := awaitReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("git fetch error = %v, want context.Canceled", err)
	}
	if err := callCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the call's context error = %v after the run was canceled, want context.Canceled", err)
	}
}

// TestGitExecPropagatesRunnerError drives the refusal branch: a failed git
// operation returns its error rather than passing as done.
func TestGitExecPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	f := &fakeRunner{err: boom}
	g := ExecGit{Run: f.run}
	if _, err := g.LsRemote("https://github.com/acme/gastown.git"); !errors.Is(err, boom) {
		t.Fatalf("LsRemote error = %v, want %v", err, boom)
	}
	if err := g.PushPrune("scratch/refs.git", "https://forge.test/acme/gastown.git", Refspecs...); !errors.Is(err, boom) {
		t.Fatalf("PushPrune error = %v, want %v", err, boom)
	}
}

// TestEqualRefSets pins the convergence check the run compares after the push:
// the same refs at the same shas are equal; a differing sha, a missing ref or an
// extra ref is not.
func TestEqualRefSets(t *testing.T) {
	t.Parallel()
	a := []Ref{{Name: "refs/heads/main", SHA: "aaaa"}, {Name: "refs/tags/v1", SHA: "bbbb"}}
	cases := []struct {
		name string
		b    []Ref
		want bool
	}{
		{name: "same refs at same shas", b: []Ref{{Name: "refs/heads/main", SHA: "aaaa"}, {Name: "refs/tags/v1", SHA: "bbbb"}}, want: true},
		{name: "an empty set differs", b: nil, want: false},
		{name: "a sha differs", b: []Ref{{Name: "refs/heads/main", SHA: "cccc"}, {Name: "refs/tags/v1", SHA: "bbbb"}}, want: false},
		{name: "a ref is missing", b: []Ref{{Name: "refs/heads/main", SHA: "aaaa"}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Equal(a, tc.b); got != tc.want {
				t.Fatalf("Equal = %v, want %v", got, tc.want)
			}
		})
	}
	if !Equal(nil, nil) {
		t.Fatal("Equal(nil, nil) = false, want true")
	}
}

// TestExecRunnerErrorNamesCommandNotOutput drives the failure branch with a fake
// process starter, so no process starts. The error wraps the starter's error and
// names the program and subcommand, and the output the failed process wrote
// stays out of it, since that output can carry a secret.
func TestExecRunnerErrorNamesCommandNotOutput(t *testing.T) {
	t.Parallel()
	const secret = "token-value-abc"
	boom := errors.New("exit status 128")
	start := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(secret), boom
	}
	_, err := execRunner(start)(context.Background(), []string{"git", "ls-remote", "--heads", "https://github.com/acme/gastown.git"})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	if !strings.HasPrefix(err.Error(), "git ls-remote: ") {
		t.Fatalf("error = %q, want it to name git ls-remote", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q carries the process output", err)
	}
}
