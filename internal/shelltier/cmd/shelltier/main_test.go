package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// The pool starts every Parallel entry before the Serial ones, so a run's
	// order is not the manifest order; each entry must still run exactly once,
	// and the Serial entries keep their relative manifest order.
	parallel := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Mode == shelltier.Parallel {
			parallel[e.Path] = true
		}
	}
	ran := make(map[string]int, len(f.cmds))
	var gotSerial []string
	for _, c := range f.cmds {
		path := c.Argv[len(c.Argv)-1]
		ran[path]++
		if !parallel[path] {
			gotSerial = append(gotSerial, path)
		}
	}
	var wantSerial []string
	for _, e := range entries {
		if got := ran[e.Path]; got != 1 {
			t.Errorf("%s ran %d times, want once", e.Path, got)
		}
		if !parallel[e.Path] {
			wantSerial = append(wantSerial, e.Path)
		}
	}
	if !slices.Equal(gotSerial, wantSerial) {
		t.Fatalf("Serial scripts ran in the order %q, want the manifest order %q", gotSerial, wantSerial)
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
	if got := errorLines(f.err.String()); !slices.Contains(got, want) {
		t.Fatalf("error stream lines = %q, want the summary %q among them", got, want)
	}
}

// TestMainPrintsTheFailedScriptsLogTailAfterTheSummary drives a red run whose
// failing script wrote a known line to its log and requires the job output to
// carry it, since the log itself is gone with the runner's temp directory.
func TestMainPrintsTheFailedScriptsLogTailAfterTheSummary(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	f := newFixture(map[string]int{"scripts/b_test.sh": 1}, twoScripts()...)
	exec := f.env.Exec
	f.env.Exec = func(ctx context.Context, c shelltier.Command) (int, error) {
		if len(c.Argv) > 0 && c.Argv[len(c.Argv)-1] == "scripts/b_test.sh" {
			if err := os.WriteFile(c.Log, []byte("ok\nboom: the known failure\n"), 0o600); err != nil {
				t.Errorf("write %s: %v", c.Log, err)
			}
		}
		return exec(ctx, c)
	}
	if code := Main([]string{"--all", "--logdir", logDir}, &f.out, f.env); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	want := "tier-sweep: shell scripts/a_test.sh ...\n" +
		"tier-sweep: shell scripts/b_test.sh ...\n" +
		"tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: scripts/b_test.sh (logs " + logDir + ") in 2m14s\n" +
		"tier-sweep: shell log tail scripts/b_test.sh\n" +
		"ok\nboom: the known failure\n"
	if got := f.err.String(); got != want {
		t.Fatalf("error stream = %q, want %q", got, want)
	}
}

// TestMainNamesAnUnreadableFailedScriptLog requires a red run's log tail to say
// when the log cannot be read, and to leave the summary and exit status alone.
func TestMainNamesAnUnreadableFailedScriptLog(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	f := newFixture(map[string]int{"scripts/b_test.sh": 1}, twoScripts()...)
	if code := Main([]string{"--all", "--logdir", logDir}, &f.out, f.env); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	lines := errorLines(f.err.String())
	want := "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: scripts/b_test.sh (logs " + logDir + ") in 2m14s"
	if len(lines) != 5 {
		t.Fatalf("error stream lines = %q, want the two announces, the summary, the header and one unreadable line", lines)
	}
	if lines[2] != want {
		t.Fatalf("summary = %q, want %q", lines[2], want)
	}
	if line := "tier-sweep: shell log tail scripts/b_test.sh"; lines[3] != line {
		t.Fatalf("header = %q, want %q", lines[3], line)
	}
	if !strings.Contains(lines[4], "cannot read") {
		t.Fatalf("last line = %q, want it to say the log cannot be read", lines[4])
	}
}

// errorLines is s's lines, without the trailing newline.
func errorLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
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

// hangGuard bounds every wait this file makes on a run that ends at once: a
// command that wired --jobs starts both entries together and returns as soon
// as they are released, so a regression fails the test here rather than
// hanging the suite.
const hangGuard = 10 * time.Second

// endRun waits for the command's run to end within hangGuard, failing the test
// if it does not: a released run returns at once, so a stuck one is a
// regression, not a slow one.
func endRun(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(hangGuard): //testpolicy:allow no-sleep — a hang guard only: every held script is released, so a correct run returns at once and a stuck one fails here rather than hanging the suite
		t.Fatalf("waited %s for the run to finish", hangGuard)
	}
}

// TestMainRunsParallelEntriesUpToJobs: --jobs reaches the pool. Two Parallel
// entries with --jobs 2 start together; a command that did not wire the flag
// runs them one at a time and never starts the second while the first is held.
func TestMainRunsParallelEntriesUpToJobs(t *testing.T) {
	t.Parallel()
	entries := []shelltier.Entry{
		{Path: "scripts/a_test.sh", Mode: shelltier.Parallel},
		{Path: "scripts/b_test.sh", Mode: shelltier.Parallel},
	}
	started := make(chan string, len(entries))
	release := make(chan struct{})
	var errs bytes.Buffer
	env := Env{
		Err:     &errs,
		Entries: entries,
		Exec: func(ctx context.Context, c shelltier.Command) (int, error) {
			started <- c.Argv[len(c.Argv)-1]
			select {
			case <-release:
			case <-ctx.Done():
			}
			return 0, nil
		},
		Now: func() time.Time { return time.Unix(0, 0) },
		WithTimeout: func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		},
	}
	var code int
	done := make(chan struct{})
	go func() {
		code = Main([]string{"--all", "--logdir", "/logs", "--jobs", "2"}, &bytes.Buffer{}, env)
		close(done)
	}()
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseAll()
		endRun(t, done)
	})

	var began []string
	for range entries {
		select {
		case path := <-started:
			began = append(began, path)
		case <-time.After(hangGuard): //testpolicy:allow no-sleep — a hang guard only: both entries start at once on a wired --jobs, and a command that dropped the flag fails here rather than hanging the suite
			t.Fatalf("waited %s for both Parallel entries to start: %q started", hangGuard, began)
		}
	}
	releaseAll()
	endRun(t, done)
	if code != 0 {
		t.Errorf("exit = %d, want 0: %s", code, errs.String())
	}
}

// TestMainRefusesJobsBelowOne: a pool of no workers starts no script, so the
// command refuses the value instead of silently running one.
func TestMainRefusesJobsBelowOne(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			f := newFixture(nil, twoScripts()...)
			if code := Main([]string{"--all", "--jobs", value}, &f.out, f.env); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(f.err.String(), "usage: shelltier [--all]") {
				t.Fatalf("stderr = %q, want the usage line", f.err.String())
			}
			if len(f.cmds) != 0 {
				t.Fatalf("ran %d scripts with --jobs %s, want 0", len(f.cmds), value)
			}
		})
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

func TestMainRecordsEachScriptsTimeUnderTheTown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		// found is the town root the lookup answers.
		found string
		// looked is the root the lookup must be given, or "" when --town must
		// keep the lookup from being consulted at all.
		looked string
		want   string
	}{
		{
			name:   "the town flag names the town",
			args:   []string{"--all", "--town", "/flag-town", "--logdir", "/logs"},
			found:  "/looked-up-town",
			looked: "",
			want:   shelltier.TimingPath("/flag-town"),
		},
		{
			name:   "the town is looked up from the run's root",
			args:   []string{"--all", "--logdir", "/logs"},
			found:  "/found-town",
			looked: ".",
			want:   shelltier.TimingPath("/found-town"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(nil, twoScripts()...)
			timing := &shelltiertest.Timing{}
			looked := ""
			f.env.Timing = timing
			f.env.Town = func(root string) (string, error) {
				looked = root
				return tc.found, nil
			}
			if code := Main(tc.args, &f.out, f.env); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if looked != tc.looked {
				t.Errorf("town lookup root = %q, want %q", looked, tc.looked)
			}
			if len(timing.Lines) != 2 {
				t.Fatalf("appended %d lines, want one per script", len(timing.Lines))
			}
			for i, line := range timing.Lines {
				if timing.Paths[i] != tc.want {
					t.Errorf("line %d appended to %q, want %q", i, timing.Paths[i], tc.want)
				}
				if path := twoScripts()[i].Path; !strings.Contains(line, path) {
					t.Errorf("line %d = %q, want it to name %s", i, line, path)
				}
			}
		})
	}
}

func TestMainRecordsNoTimingWithoutATown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		town func(root string) (string, error)
	}{
		{"the lookup fails", func(string) (string, error) { return "", errors.New("not in a workspace") }},
		{"the lookup finds no town", func(string) (string, error) { return "", nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(nil, twoScripts()...)
			timing := &shelltiertest.Timing{}
			f.env.Timing, f.env.Town = timing, tc.town
			if code := Main([]string{"--all", "--logdir", "/logs"}, &f.out, f.env); code != 0 {
				t.Fatalf("exit = %d, want 0: a missing town must not change the verdict", code)
			}
			if len(timing.Lines) != 0 {
				t.Errorf("appended %q, want nothing without a town", timing.Lines)
			}
			if !strings.Contains(f.err.String(), "no town root for the timing log") {
				t.Errorf("error stream = %q, want the missing town said", f.err.String())
			}
		})
	}
}

// TestTownOfFindsTheTownContainingTheRoot drives the production town lookup:
// the runner starts in a worktree inside a town and records its timing under
// the town root, whatever the depth.
func TestTownOfFindsTheTownContainingTheRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(town, "gastown", "polecats", "ace")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := townOf(worktree)
	if err != nil {
		t.Fatalf("townOf(%q): %v", worktree, err)
	}
	if got != town {
		t.Errorf("townOf(%q) = %q, want the town root %q", worktree, got, town)
	}
}
