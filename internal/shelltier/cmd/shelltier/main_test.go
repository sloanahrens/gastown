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

	"github.com/steveyegge/gastown/internal/shelltier"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// fixture is one case's Env: two scripts, a fake Exec that records each
// command and answers the given codes, and a clock 134 seconds apart.
type fixture struct {
	env      Env
	out, err bytes.Buffer
	cmds     []shelltier.Command
	budgets  []time.Duration
	made     []string
}

func newFixture(codes map[string]int, entries ...shelltier.Entry) *fixture {
	f := &fixture{}
	start := time.Unix(0, 0)
	calls := 0
	f.env = Env{
		Err:     &f.err,
		Entries: entries,
		Exec: func(_ context.Context, c shelltier.Command) (int, error) {
			f.cmds = append(f.cmds, c)
			return codes[c.Argv[len(c.Argv)-1]], nil
		},
		Now: func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return start.Add(134 * time.Second)
		},
		WithTimeout: func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
			f.budgets = append(f.budgets, d)
			return context.WithCancel(ctx)
		},
		TempDir: "/tmp",
		MkdirTemp: func(dir, pattern string) (string, error) {
			f.made = append(f.made, dir+"/"+pattern)
			return "/made", nil
		},
	}
	return f
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
