package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// gt-csng: the polecat-side pre-work duplicate check.

const (
	dupesOriginCall = "git:rev-parse --verify --quiet origin/main"
	dupesLogPrefix  = "git:log origin/main --since=1 day --name-only --pretty=format:%h%x09%s -- "
	dupesSharedFile = "cmd/gt/hermetic_main_test.go"
	// dupesSharedLog is what git log prints for one fresh origin/main commit
	// touching dupesSharedFile.
	dupesSharedLog = "abc1234\tTestRunPrimeExternalTools_BoundsSlowMailCheck: fix the slow-mail bound (gt-abc)\n" + dupesSharedFile + "\n"
)

// dupesRunner answers the check's two git calls: origin/main resolves unless
// noOrigin, the log for dupesSharedFile names the fresh commit, and the log
// for any other single path is empty. Any other call fails, as git outside
// a repository does.
func dupesRunner(noOrigin bool, otherPaths ...string) *fakePrimeRunner {
	answers := map[string]string{dupesLogPrefix + dupesSharedFile: dupesSharedLog}
	if !noOrigin {
		answers[dupesOriginCall] = ""
	}
	for _, p := range otherPaths {
		answers[dupesLogPrefix+p] = ""
	}
	return &fakePrimeRunner{answers: answers}
}

// hookedPathDupesOutput runs the check over f and returns what it printed.
func hookedPathDupesOutput(f *fakePrimeRunner, ctx RoleContext, bead *beads.Issue, skip ...bool) string {
	p, _, out := newFakePrimeTools(f)
	p.skipDupes = len(skip) > 0 && skip[0]
	p.hookedPathDupes(ctx, bead)
	return out.String()
}

func TestDupesRecentLog(t *testing.T) {
	t.Parallel()
	f := dupesRunner(false, "internal/cmd/never_touched.go")
	p, _, _ := newFakePrimeTools(f)
	commits, err := p.dupesRecentLog("/work", []string{dupesSharedFile})
	if err != nil {
		t.Fatalf("dupesRecentLog: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d, want 1: %+v", len(commits), commits)
	}
	c := commits[0]
	if c.Hash != "abc1234" {
		t.Errorf("hash = %q, want abc1234", c.Hash)
	}
	if !c.Files[dupesSharedFile] {
		t.Errorf("Files = %v, want %s", c.Files, dupesSharedFile)
	}
	want := "TestRunPrimeExternalTools_BoundsSlowMailCheck: fix the slow-mail bound (gt-abc)"
	if c.Subject != want {
		t.Errorf("Subject = %q, want %q", c.Subject, want)
	}

	// A path the history never touches returns nothing, no error.
	if commits, err := p.dupesRecentLog("/work", []string{"internal/cmd/never_touched.go"}); err != nil || len(commits) != 0 {
		t.Errorf("untouched path: commits=%v err=%v, want empty/nil", commits, err)
	}

	// Without origin/main the log is never read.
	f = dupesRunner(true)
	p, _, _ = newFakePrimeTools(f)
	if _, err := p.dupesRecentLog("/work", []string{dupesSharedFile}); err == nil {
		t.Errorf("no origin/main: want an error")
	}
	if f.called(dupesLogPrefix + dupesSharedFile) {
		t.Errorf("git log ran without origin/main: %v", f.callLines())
	}
}

func TestDupesRecentLogParsesSeveralCommits(t *testing.T) {
	t.Parallel()
	f := &fakePrimeRunner{answers: map[string]string{
		dupesOriginCall:              "",
		dupesLogPrefix + "a.go b.go": "1111111\tfirst\na.go\nb.go\n\n2222222\tsecond\n./b.go\n",
	}}
	p, _, _ := newFakePrimeTools(f)
	commits, err := p.dupesRecentLog("/work", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("dupesRecentLog: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("commits = %+v, want 2", commits)
	}
	if !commits[0].Files["a.go"] || !commits[0].Files["b.go"] || commits[0].Subject != "first" {
		t.Errorf("first commit = %+v", commits[0])
	}
	if !commits[1].Files["b.go"] || commits[1].Hash != "2222222" {
		t.Errorf("second commit = %+v, want b.go normalized from ./b.go", commits[1])
	}
}

func TestCheckHookedPathDupes(t *testing.T) {
	t.Parallel()
	shared := &beads.Issue{
		ID:    "gt-3vr",
		Title: "Fix the slow-mail bound",
		Description: dupesSharedFile + " fails in " +
			"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
	}
	t.Run("warns when a shared file was recently changed", func(t *testing.T) {
		t.Parallel()
		out := hookedPathDupesOutput(dupesRunner(false), RoleContext{Role: RolePolecat, WorkDir: "/work"}, shared)
		if !strings.Contains(out, dupesSharedFile) {
			t.Fatalf("output missing the shared file:\n%s", out)
		}
		if !strings.Contains(out, "test name your bead names") {
			t.Fatalf("output missing the stronger warning line:\n%s", out)
		}
		if !strings.Contains(out, "check before duplicating this work") {
			t.Fatalf("output missing the header:\n%s", out)
		}
	})

	t.Run("no stronger line without a shared test name", func(t *testing.T) {
		t.Parallel()
		out := hookedPathDupesOutput(dupesRunner(false), RoleContext{Role: RolePolecat, WorkDir: "/work"},
			&beads.Issue{ID: "gt-x", Title: "x", Description: dupesSharedFile + "."})
		if !strings.Contains(out, "check before duplicating this work") {
			t.Fatalf("output missing the header:\n%s", out)
		}
		if strings.Contains(out, "test name your bead names") {
			t.Fatalf("stronger line without a shared test name:\n%s", out)
		}
	})

	t.Run("silent when no overlap", func(t *testing.T) {
		t.Parallel()
		out := hookedPathDupesOutput(dupesRunner(false, "internal/cmd/never_touched.go"), RoleContext{Role: RolePolecat, WorkDir: "/work"},
			&beads.Issue{
				ID:          "gt-rl0",
				Title:       "Some unrelated bead",
				Description: "Touches internal/cmd/never_touched.go only.",
			})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("expected silence, got:\n%s", out)
		}
	})

	t.Run("silent when the bead names no paths", func(t *testing.T) {
		t.Parallel()
		f := dupesRunner(false)
		out := hookedPathDupesOutput(f, RoleContext{Role: RolePolecat},
			&beads.Issue{ID: "gt-x", Title: "No paths here", Description: "Just prose."})
		if strings.TrimSpace(out) != "" || len(f.callLines()) != 0 {
			t.Fatalf("expected silence and no git, got %v:\n%s", f.callLines(), out)
		}
	})

	// Each gate below is checked with a bead and history the check WOULD
	// warn on, so silence is the gate and not an absent overlap.
	for _, tc := range []struct {
		name string
		role Role
		skip bool
	}{
		{name: "skipped in continuation mode or dry-run", role: RolePolecat, skip: true},
		{name: "non-polecat is skipped", role: RoleCrew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := dupesRunner(false)
			out := hookedPathDupesOutput(f, RoleContext{Role: tc.role, WorkDir: "/work"}, shared, tc.skip)
			if strings.TrimSpace(out) != "" || len(f.callLines()) != 0 {
				t.Fatalf("the check must not run, got calls %v:\n%s", f.callLines(), out)
			}
		})
	}

	t.Run("silent when the repo has no origin/main", func(t *testing.T) {
		t.Parallel()
		// Fail-closed: a worktree whose remote branch is not fetched yet (or
		// a workdir outside any repository, where the same rev-parse fails)
		// is exactly the case a warning would be least trustworthy in, so
		// the check must say nothing rather than guess.
		out := hookedPathDupesOutput(dupesRunner(true), RoleContext{Role: RolePolecat, WorkDir: "/work"}, shared)
		if strings.TrimSpace(out) != "" {
			t.Fatalf("no origin/main must fail closed, got:\n%s", out)
		}
	})
}

// TestCheckHookedPathDupes_BoundsSlowGitLog proves the check's git calls run on
// prime's external-tool deadline rather than waiting a wedged git out, and
// that an abandoned call is not reported on. The deadline is asserted on the
// fake clock (see waitOutWedgedTool) rather than timed.
func TestCheckHookedPathDupes_BoundsSlowGitLog(t *testing.T) {
	t.Parallel()
	const logCall = "git:log origin/main --since=1 day --name-only --pretty=format:%h%x09%s -- cmd/gt/hermetic_main_test.go"
	f := &fakePrimeRunner{
		answers: map[string]string{"git:rev-parse --verify --quiet origin/main": ""},
		block:   map[string]bool{logCall: true},
	}
	p, clk, out := newFakePrimeTools(f)

	call := waitOutWedgedTool(t, f, clk, func() {
		p.hookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
			&beads.Issue{
				ID:    "gt-x",
				Title: "Fix the slow-mail bound",
				Description: "cmd/gt/hermetic_main_test.go fails in " +
					"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
			})
	})

	if call.line != logCall {
		t.Fatalf("wedged call = %q, want %q", call.line, logCall)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a git call the check abandoned must not be reported on:\n%s", out.String())
	}
}
