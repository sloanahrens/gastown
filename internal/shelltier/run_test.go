package shelltier

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// answer is what the fake Exec reports for one script.
type answer struct {
	code int
	err  error
}

// fakeExec returns an Exec that records each command it is given and answers
// with the outcome set for that script's path; a path with no answer exits 0.
func fakeExec(answers map[string]answer, cmds *[]Command) Exec {
	return func(_ context.Context, c Command) (int, error) {
		*cmds = append(*cmds, c)
		a := answers[c.Argv[1]]
		return a.code, a.err
	}
}

// liveTimeout stands in for WithTimeout with a context that never expires on its own.
func liveTimeout(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// twoInstants returns a clock that reads start on its first call and end after.
func twoInstants(start, end time.Time) func() time.Time {
	calls := 0
	return func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return end
	}
}

func entriesAt(paths ...string) []Entry {
	var out []Entry
	for _, p := range paths {
		out = append(out, Entry{Path: p})
	}
	return out
}

func TestRunCountsPassedAndFailed(t *testing.T) {
	t.Parallel()
	var cmds []Command
	cfg := Config{
		Entries:     entriesAt("scripts/a_test.sh", "scripts/b_test.sh", "scripts/c_test.sh"),
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        fakeExec(map[string]answer{"scripts/b_test.sh": {code: 1}}, &cmds),
		Now:         twoInstants(time.Unix(0, 0), time.Unix(0, 0)),
		WithTimeout: liveTimeout,
	}
	r := Run(t.Context(), cfg)
	if r.Passed != 2 || r.Failed != 1 || r.Skipped != 0 {
		t.Fatalf("counts passed=%d failed=%d skipped=%d, want 2 1 0", r.Passed, r.Failed, r.Skipped)
	}
	if want := []string{"scripts/b_test.sh"}; !slices.Equal(r.FailedNames, want) {
		t.Fatalf("FailedNames = %v, want %v", r.FailedNames, want)
	}
	if len(cmds) != 3 {
		t.Fatalf("ran %d commands, want 3", len(cmds))
	}
}

func TestRunFailsAScriptWhoseExecErrors(t *testing.T) {
	t.Parallel()
	var cmds []Command
	cfg := Config{
		Entries:     entriesAt("scripts/a_test.sh"),
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        fakeExec(map[string]answer{"scripts/a_test.sh": {code: 0, err: errors.New("cannot start")}}, &cmds),
		Now:         twoInstants(time.Unix(0, 0), time.Unix(0, 0)),
		WithTimeout: liveTimeout,
	}
	r := Run(t.Context(), cfg)
	if r.Passed != 0 || r.Failed != 1 {
		t.Fatalf("counts passed=%d failed=%d, want 0 1", r.Passed, r.Failed)
	}
}

func TestRunBoundsAScriptThatNeverReturns(t *testing.T) {
	t.Parallel()
	const hangs = "scripts/hangs_test.sh"
	entries := entriesAt(hangs, "scripts/a_test.sh", "scripts/b_test.sh")
	calls := 0
	// The first script's budget has already run out; the others get live contexts.
	withTimeout := func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		calls++
		ctx, cancel := context.WithCancel(ctx)
		if calls == 1 {
			cancel()
		}
		return ctx, cancel
	}
	hungEnded := false
	exec := func(ctx context.Context, c Command) (int, error) {
		if c.Argv[1] != hangs {
			return 0, nil
		}
		// The script never returns on its own: only its context ending releases it.
		hungEnded = ctx.Err() != nil
		return -1, ctx.Err()
	}
	r := Run(t.Context(), Config{
		Entries:     entries,
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        exec,
		Now:         twoInstants(time.Unix(0, 0), time.Unix(0, 0)),
		WithTimeout: withTimeout,
	})
	if !hungEnded {
		t.Fatal("the hanging script was not released by its context ending")
	}
	if r.Passed != 2 || r.Failed != 1 {
		t.Fatalf("counts passed=%d failed=%d, want 2 1", r.Passed, r.Failed)
	}
	if want := []string{hangs}; !slices.Equal(r.FailedNames, want) {
		t.Fatalf("FailedNames = %v, want %v", r.FailedNames, want)
	}
}

func TestRunFailsAScriptThatFinishesAfterItsBudget(t *testing.T) {
	t.Parallel()
	// The budget ran out, but Exec reports a clean exit anyway: the script still fails.
	exec := func(context.Context, Command) (int, error) { return 0, nil }
	withTimeout := func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx, cancel
	}
	r := Run(t.Context(), Config{
		Entries:     entriesAt("scripts/a_test.sh"),
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        exec,
		Now:         twoInstants(time.Unix(0, 0), time.Unix(0, 0)),
		WithTimeout: withTimeout,
	})
	if r.Passed != 0 || r.Failed != 1 {
		t.Fatalf("counts passed=%d failed=%d, want 0 1", r.Passed, r.Failed)
	}
}

func TestRunGivesEachScriptItsOwnEnvAndLog(t *testing.T) {
	t.Parallel()
	const logDir = "/logs"
	tests := []struct {
		path    string
		wantLog string
		wantTmp string
	}{
		{"scripts/a_test.sh", "shell-scripts_a_test.sh.log", "shell-scripts_a_test.sh"},
		{"deploy/k8s/kind/kind_test.sh", "shell-deploy_k8s_kind_kind_test.sh.log", "shell-deploy_k8s_kind_kind_test.sh"},
	}
	var entries []Entry
	for _, tc := range tests {
		entries = append(entries, Entry{Path: tc.path})
	}
	var cmds []Command
	Run(t.Context(), Config{
		Entries:     entries,
		Root:        "/repo",
		LogDir:      logDir,
		Budget:      time.Minute,
		Exec:        fakeExec(nil, &cmds),
		Now:         twoInstants(time.Unix(0, 0), time.Unix(0, 0)),
		WithTimeout: liveTimeout,
	})
	if len(cmds) != len(tests) {
		t.Fatalf("ran %d commands, want %d", len(cmds), len(tests))
	}
	for i, tc := range tests {
		c := cmds[i]
		if c.Dir != "/repo" || !slices.Equal(c.Argv, []string{"bash", tc.path}) {
			t.Errorf("%s: ran %v in %q, want bash %s in /repo", tc.path, c.Argv, c.Dir, tc.path)
		}
		wantTmp := "TMPDIR=" + filepath.Join(logDir, "tmp", tc.wantTmp)
		if want := []string{"GT_TEST_DOCKER=0", wantTmp}; !slices.Equal(c.Env, want) {
			t.Errorf("%s: env = %v, want %v", tc.path, c.Env, want)
		}
		if want := filepath.Join(logDir, tc.wantLog); c.Log != want {
			t.Errorf("%s: log = %q, want %q", tc.path, c.Log, want)
		}
	}
}

func TestRunElapsedIsTheClockSpan(t *testing.T) {
	t.Parallel()
	start := time.Unix(100, 0)
	var cmds []Command
	r := Run(t.Context(), Config{
		Entries:     entriesAt("scripts/a_test.sh"),
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        fakeExec(nil, &cmds),
		Now:         twoInstants(start, start.Add(5*time.Second)),
		WithTimeout: liveTimeout,
	})
	if r.Elapsed != 5*time.Second {
		t.Fatalf("Elapsed = %v, want 5s", r.Elapsed)
	}
}
