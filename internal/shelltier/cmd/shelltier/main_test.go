package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/shelltier"
	"github.com/steveyegge/gastown/internal/shelltier/shelltiertest"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

type memFiles = shelltiertest.MemFiles

// noChanges is a repository with no change since any record and no uncommitted path.
type noChanges struct{}

func (noChanges) DiffNameOnly(_, _ string) ([]string, error) { return nil, nil }

func (noChanges) Worktree() ([]string, error) { return nil, nil }

const headCommit = shelltiertest.HeadCommit

// fixture is one case's Env: a fake Exec that records each command, answers
// the given codes and takes 67 seconds per script on a fake clock, so two
// scripts run for 134 seconds however often the runner reads the clock.
type fixture struct {
	env      Env
	out, err bytes.Buffer
	cmds     []shelltier.Command
	budgets  []time.Duration
	made     []string
	now      time.Time
	files    memFiles
}

func newFixture(codes map[string]int, entries ...shelltier.Entry) *fixture {
	f := &fixture{now: time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC), files: memFiles{}}
	f.env = Env{
		Err:     &f.err,
		Entries: entries,
		Exec: func(_ context.Context, c shelltier.Command) (int, error) {
			f.cmds = append(f.cmds, c)
			f.now = f.now.Add(67 * time.Second)
			return codes[c.Argv[len(c.Argv)-1]], nil
		},
		Now: func() time.Time { return f.now },
		WithTimeout: func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
			f.budgets = append(f.budgets, d)
			return context.WithCancel(ctx)
		},
		TempDir: "/tmp",
		MkdirTemp: func(dir, pattern string) (string, error) {
			f.made = append(f.made, dir+"/"+pattern)
			return "/made", nil
		},
		Files: f.files,
		Checkout: func(string) shelltier.Checkout {
			return shelltier.Checkout{
				Head: func() (string, error) { return headCommit, nil },
				Git:  noChanges{},
			}
		},
		Zone: func() (*time.Location, error) { return time.UTC, nil },
	}
	return f
}

// seedGreen records entry path as green at oldCommit on 29 October, in the
// state file a --since-green run under /town reads.
func (f *fixture) seedGreen(t *testing.T, path string) []byte {
	t.Helper()
	st := tiertrigger.State{}
	st.Ran(path, "1111111111111111111111111111111111111111", time.Date(2026, time.October, 29, 9, 0, 0, 0, time.UTC))
	if err := (tiertrigger.Store{Path: shelltier.StatePath("/town"), Files: f.files}).Save(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return f.files[shelltier.StatePath("/town")]
}

// lastLine is the final line of s, without its newline.
func lastLine(s string) string {
	return strings.TrimSpace(s[strings.LastIndex(strings.TrimSpace(s), "\n")+1:])
}

func twoScripts() []shelltier.Entry {
	return []shelltier.Entry{{Path: "scripts/a_test.sh"}, {Path: "scripts/b_test.sh"}}
}

func TestMainRunsEveryEntryWithAll(t *testing.T) {
	t.Parallel()
	entries := shelltier.Manifest()
	f := newFixture(nil, entries...)
	if code := Main([]string{"--all", "--logdir", "/logs"}, &f.out, f.env); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.cmds) != len(entries) {
		t.Fatalf("ran %d scripts, want %d", len(f.cmds), len(entries))
	}
	for i, e := range entries {
		if got := f.cmds[i].Argv[len(f.cmds[i].Argv)-1]; got != e.Path {
			t.Fatalf("script %d = %s, want %s", i, got, e.Path)
		}
	}
}

func TestMainPrintsTheScriptLinesAndTheSummary(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	Main([]string{"--all", "--logdir", "/logs"}, &f.out, f.env)
	want := "tier-sweep: shell scripts/a_test.sh ...\n" +
		"tier-sweep: shell scripts/b_test.sh ...\n" +
		"tier-sweep: shell GREEN passed=2 failed=0 skipped=0 (logs /logs) in 2m14s\n"
	if f.err.String() != want {
		t.Fatalf("error stream = %q, want %q", f.err.String(), want)
	}
	if f.out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on a run", f.out.String())
	}
}

func TestMainExitsRedWhenAScriptFails(t *testing.T) {
	t.Parallel()
	f := newFixture(map[string]int{"scripts/b_test.sh": 1}, twoScripts()...)
	code := Main([]string{"--all", "--logdir", "/logs"}, &f.out, f.env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	want := "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: scripts/b_test.sh (logs /logs) in 2m14s"
	if got := lastLine(f.err.String()); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestMainWritesHelpToTheOutStream(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	if code := Main([]string{"-h"}, &f.out, f.env); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(f.out.String(), "usage: shelltier [--all]") {
		t.Fatalf("stdout = %q, want the usage line", f.out.String())
	}
	if f.err.Len() != 0 {
		t.Fatalf("error stream = %q, want nothing for -h", f.err.String())
	}
}

func TestMainRejectsAnUnknownFlag(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	code := Main([]string{"--nope"}, &f.out, f.env)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(f.err.String(), "usage: shelltier [--all]") {
		t.Fatalf("stderr = %q, want the usage line", f.err.String())
	}
	if len(f.cmds) != 0 {
		t.Fatalf("ran %d scripts after a usage error, want 0", len(f.cmds))
	}
}

func TestMainPassesTheBudgetToEachScript(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	Main([]string{"--all", "--logdir", "/logs", "--budget", "3m"}, &f.out, f.env)
	want := []time.Duration{3 * time.Minute, 3 * time.Minute}
	if !slices.Equal(f.budgets, want) {
		t.Fatalf("budgets = %v, want %v", f.budgets, want)
	}
}

func TestMainMakesADefaultLogDirUnderTempDir(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	Main([]string{"--all"}, &f.out, f.env)
	if want := []string{"/tmp/tier-sweep."}; !slices.Equal(f.made, want) {
		t.Fatalf("made dirs = %v, want %v", f.made, want)
	}
	if !strings.HasSuffix(lastLine(f.err.String()), "(logs /made) in 2m14s") {
		t.Fatalf("summary %q does not name the made log dir", f.err.String())
	}
}

func TestMainFailsWhenTheLogDirCannotBeMade(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	f.env.MkdirTemp = func(string, string) (string, error) { return "", errors.New("denied") }
	if code := Main([]string{"--all"}, &f.out, f.env); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("ran %d scripts with no log dir, want 0", len(f.cmds))
	}
}

func TestAnnounceRunsACommandWithNoArgvWithoutPanicking(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	ran := false
	exec := announce(&w, func(context.Context, shelltier.Command) (int, error) {
		ran = true
		return 0, nil
	})
	if _, err := exec(t.Context(), shelltier.Command{}); err != nil {
		t.Fatalf("error = %v, want nil from the fake exec", err)
	}
	if !ran {
		t.Fatal("announce did not hand the command to exec")
	}
}

func TestSinceGreenRunsOnlyTheSelectedEntries(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	f.seedGreen(t, "scripts/a_test.sh")
	code := Main([]string{"--since-green", "--town", "/town", "--logdir", "/logs"}, &f.out, f.env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.cmds) != 1 || f.cmds[0].Argv[len(f.cmds[0].Argv)-1] != "scripts/b_test.sh" {
		t.Fatalf("ran %v, want only scripts/b_test.sh", f.cmds)
	}
	want := "tier-sweep: shell GREEN passed=1 failed=0 skipped=1 (logs /logs) in 1m7s"
	if got := lastLine(f.err.String()); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestSinceGreenRecordsTheEntriesThatPassed(t *testing.T) {
	t.Parallel()
	f := newFixture(map[string]int{"scripts/b_test.sh": 1}, twoScripts()...)
	Main([]string{"--since-green", "--town", "/town", "--logdir", "/logs"}, &f.out, f.env)
	st, err := tiertrigger.Store{Path: shelltier.StatePath("/town"), Files: f.files}.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if rec, ok := st["scripts/a_test.sh"]; !ok || rec.Commit != headCommit {
		t.Fatalf("record for the passing script = %+v, want commit %s", rec, headCommit)
	}
	if _, ok := st["scripts/b_test.sh"]; ok {
		t.Fatal("the failing script was recorded as green")
	}
}

func TestAllIgnoresAndLeavesTheState(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	before := f.seedGreen(t, "scripts/a_test.sh")
	code := Main([]string{"--all", "--since-green", "--town", "/town", "--logdir", "/logs"}, &f.out, f.env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.cmds) != 2 {
		t.Fatalf("ran %d scripts with --all, want 2", len(f.cmds))
	}
	if got := f.files[shelltier.StatePath("/town")]; !bytes.Equal(got, before) {
		t.Fatalf("state file changed to %q under --all, want it untouched", got)
	}
}

func TestSinceGreenNeedsATown(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	if code := Main([]string{"--since-green"}, &f.out, f.env); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(f.err.String(), "usage: shelltier [--all]") {
		t.Fatalf("stderr = %q, want the usage line", f.err.String())
	}
	if len(f.cmds) != 0 {
		t.Fatalf("ran %d scripts without a town, want 0", len(f.cmds))
	}
}

func TestWorktreePathsCountsUntrackedFiles(t *testing.T) {
	t.Parallel()
	status := &git.GitStatus{
		Modified:  []string{"a.go"},
		Added:     []string{"b.go"},
		Deleted:   []string{"c.go"},
		Unmerged:  []string{"d.go"},
		Untracked: []string{"e_test.go"},
	}
	want := []string{"a.go", "b.go", "c.go", "d.go", "e_test.go"}
	if got := worktreePaths(status); !slices.Equal(got, want) {
		t.Fatalf("worktreePaths = %v, want %v", got, want)
	}
}

func TestWorktreeGitReportsAFailedStatus(t *testing.T) {
	t.Parallel()
	statusErr := errors.New("index is locked")
	g := worktreeGit{
		diff:   func(string, string) ([]string, error) { return nil, nil },
		status: func() (*git.GitStatus, error) { return nil, statusErr },
	}
	paths, err := g.Worktree()
	if !errors.Is(err, statusErr) {
		t.Fatalf("error = %v, want the status failure", err)
	}
	if paths != nil {
		t.Fatalf("paths = %v with a failed status, want none", paths)
	}
}

func TestSinceGreenRunsEveryEntryWhenTheWorktreeCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newFixture(nil, twoScripts()...)
	// Both entries hold a record from earlier today, so neither is due by its
	// window: only the unreadable worktree can make them run.
	st := tiertrigger.State{}
	for _, e := range twoScripts() {
		st.Ran(e.Path, "1111111111111111111111111111111111111111", time.Date(2026, time.October, 29, 9, 0, 0, 0, time.UTC))
	}
	if err := (tiertrigger.Store{Path: shelltier.StatePath("/town"), Files: f.files}).Save(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	statusErr := errors.New("index is locked")
	f.env.Checkout = func(string) shelltier.Checkout {
		return shelltier.Checkout{
			Head: func() (string, error) { return headCommit, nil },
			Git: worktreeGit{
				diff:   func(string, string) ([]string, error) { return nil, nil },
				status: func() (*git.GitStatus, error) { return nil, statusErr },
			},
		}
	}
	code := Main([]string{"--since-green", "--town", "/town", "--logdir", "/logs"}, &f.out, f.env)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.cmds) != 2 {
		t.Fatalf("ran %d scripts with an unreadable worktree, want every entry (2)", len(f.cmds))
	}
	for _, path := range []string{"scripts/a_test.sh", "scripts/b_test.sh"} {
		if !strings.Contains(f.err.String(), path+": its record cannot be read") {
			t.Fatalf("error stream = %q, want %s named as unreadable", f.err.String(), path)
		}
	}
	if !strings.Contains(f.err.String(), "read the working tree: index is locked") {
		t.Fatalf("error stream = %q, want the status failure surfaced", f.err.String())
	}
	if want := "tier-sweep: shell GREEN passed=2 failed=0 skipped=0 (logs /logs) in 2m14s"; lastLine(f.err.String()) != want {
		t.Fatalf("summary = %q, want %q", lastLine(f.err.String()), want)
	}
}

func TestSinceGreenRefusesAMissingTownValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"--town given with no value", []string{"--since-green", "--town"}},
		{"--town given as an empty value", []string{"--since-green", "--town", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(nil, twoScripts()...)
			if code := Main(tc.args, &f.out, f.env); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(f.err.String(), "usage: shelltier [--all]") {
				t.Fatalf("stderr = %q, want the usage line", f.err.String())
			}
			if len(f.cmds) != 0 {
				t.Fatalf("ran %d scripts without a town, want 0", len(f.cmds))
			}
		})
	}
}
