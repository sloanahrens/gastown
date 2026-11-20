package git

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestHooksOffArgs pins where `-c core.hooksPath=/dev/null` lands: before a
// subcommand the package sends, but after every global option, and nowhere on a
// push or a subcommand it does not recognise.
func TestHooksOffArgs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "subcommand first",
			args: []string{"merge", "--no-ff", "-m", "m", "side"},
			want: []string{"-c", "core.hooksPath=/dev/null", "merge", "--no-ff", "-m", "m", "side"},
		},
		{
			name: "after a --git-dir flag",
			args: []string{"--git-dir=/srv/repo.git", "commit", "-m", "m"},
			want: []string{"--git-dir=/srv/repo.git", "-c", "core.hooksPath=/dev/null", "commit", "-m", "m"},
		},
		{
			name: "after a -c option and its value",
			args: []string{"-c", "user.name=x", "commit", "-m", "m"},
			want: []string{"-c", "user.name=x", "-c", "core.hooksPath=/dev/null", "commit", "-m", "m"},
		},
		{
			name: "push keeps the pre-push hook",
			args: []string{"push", "origin", "side"},
			want: []string{"push", "origin", "side"},
		},
		{
			name: "push behind a --git-dir flag",
			args: []string{"--git-dir=/srv/repo.git", "push", "origin", "main"},
			want: []string{"--git-dir=/srv/repo.git", "push", "origin", "main"},
		},
		{
			name: "an alias push is left alone",
			args: []string{"-c", "alias.p=push", "p"},
			want: []string{"-c", "alias.p=push", "p"},
		},
		{
			name: "a global option git does not know takes a value",
			args: []string{"--super-prefix", "x", "push", "origin", "main"},
			want: []string{"--super-prefix", "x", "push", "origin", "main"},
		},
		{
			name: "no subcommand",
			args: []string{"--version"},
			want: []string{"--version"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hooksOffArgs(append([]string(nil), tt.args...))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("hooksOffArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestWithoutLocalHooksRunsTheSubcommandWithHooksOff: a commit and a no-ff
// merge through the handle each send `-c core.hooksPath=/dev/null` and then the
// subcommand, and a push through the same handle sends no core.hooksPath at
// all: the pre-push fence stays on.
func TestWithoutLocalHooksRunsTheSubcommandWithHooksOff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newScripted(map[string]reply{
		"-C " + dir + " rev-parse --show-toplevel":            ok(dir),
		"-c core.hooksPath=/dev/null commit -m m":             ok(""),
		"-c core.hooksPath=/dev/null merge --no-ff -m m side": ok(""),
		"push origin side": ok(""),
		"-c core.hooksPath=/dev/null symbolic-ref refs/remotes/origin/HEAD": ok("refs/remotes/origin/main\n"),
	})
	g := (&Git{workDir: dir, exec: s.run}).WithoutLocalHooks()
	if err := g.Commit("m"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.MergeNoFF("side", "m"); err != nil {
		t.Fatalf("MergeNoFF: %v", err)
	}
	if err := g.Push("origin", "side", false); err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, want := range []string{
		"-c core.hooksPath=/dev/null commit -m m",
		"-c core.hooksPath=/dev/null merge --no-ff -m m side",
	} {
		if !s.hasSent(want) {
			t.Errorf("no call recorded as %q; sent %q", want, s.sent())
		}
	}
	c, found := s.sentCall("push origin side")
	if !found {
		t.Fatalf("push recorded no plain call; sent %q", s.sent())
	}
	for _, arg := range c.args {
		if strings.HasPrefix(arg, "core.hooksPath") {
			t.Errorf("push ran with %q; a push must keep the pre-push hook: %q", arg, c.args)
		}
	}
	s.noUnscripted(t)
}

// TestWithoutLocalHooksCoversWorktreeAndEnvPaths: a worktree add and the
// commit-tree of a message rewrite, which build their argv in
// runWithEnvAndTimeout, also run with the repository's local hooks off; a plain
// handle sends neither with the option (gt-4sm3lp).
func TestWithoutLocalHooksCoversWorktreeAndEnvPaths(t *testing.T) {
	t.Parallel()
	const (
		hooksOff       = "-c core.hooksPath=/dev/null "
		oldTip, newTip = "1111", "2222"
		identity       = "a\x00a@x\x002020-01-01T00:00:00Z\x00c\x00c@x\x002020-01-01T00:00:00Z\n"
	)
	for _, tt := range []struct{ name, prefix string }{{"hooks off", hooksOff}, {"plain handle", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			wt := filepath.Join(dir, "wt")
			s := newScripted(map[string]reply{
				// The safety guard asks the runner for the top level directly.
				"-C " + dir + " rev-parse --show-toplevel":                                        ok(dir),
				tt.prefix + "worktree add --detach " + wt + " main":                               ok(""),
				tt.prefix + "rev-parse HEAD":                                                      ok(oldTip),
				tt.prefix + "rev-list --reverse --topo-order --parents base..HEAD":                ok(oldTip),
				tt.prefix + "log -1 --format=%B " + oldTip:                                        ok("old"),
				tt.prefix + "log -1 --format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI " + oldTip:    ok(identity),
				tt.prefix + "commit-tree " + oldTip + "^{tree} -m new":                            ok(newTip),
				tt.prefix + "update-ref -m rewrite commit messages HEAD " + newTip + " " + oldTip: ok(""),
			})
			g := &Git{workDir: dir, exec: s.run}
			if tt.prefix != "" {
				g = g.WithoutLocalHooks()
			}
			if err := g.WorktreeAddDetached(wt, "main"); err != nil {
				t.Fatalf("WorktreeAddDetached: %v", err)
			}
			if _, err := g.RewriteCommitMessages("base", func(string, string) (string, error) { return "new", nil }); err != nil {
				t.Fatalf("RewriteCommitMessages: %v", err)
			}
			s.noUnscripted(t)
			for _, want := range []string{
				tt.prefix + "worktree add --detach " + wt + " main",
				tt.prefix + "commit-tree " + oldTip + "^{tree} -m new",
			} {
				if !s.hasSent(want) {
					t.Errorf("no call recorded as %q; sent %q", want, s.sent())
				}
			}
		})
	}
}

// TestWithoutLocalHooksLeavesTheReceiverAndPlainHandlesAlone: the receiver of
// WithoutLocalHooks, and a handle that never called it, run exactly as before.
func TestWithoutLocalHooksLeavesTheReceiverAndPlainHandlesAlone(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"commit -m m": ok("")})
	receiver := newTestGit(t, s)
	_ = receiver.WithoutLocalHooks()
	plain := newTestGit(t, s)
	for _, g := range []*Git{receiver, plain} {
		if err := g.Commit("m"); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if got := s.sent(); !slices.Equal(got, []string{"commit -m m", "commit -m m"}) {
		t.Fatalf("calls = %q, want both plain `commit -m m`", got)
	}
}

// TestWithoutLocalHooksCoversTimedStdinMergeCheck: the timed, stdin and
// merge-check paths run with the local hooks off too; a push through them
// keeps its pre-push hook, and a plain handle carries the option nowhere
// (gt-eeinkv).
func TestWithoutLocalHooksCoversTimedStdinMergeCheck(t *testing.T) {
	t.Parallel()
	const off = "-c core.hooksPath=/dev/null "
	for _, tt := range []struct{ name, prefix string }{{"hooks off", off}, {"plain handle", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newScripted(map[string]reply{
				"-C " + dir + " rev-parse --show-toplevel": ok(dir),
				tt.prefix + "fetch *":                      ok(""),
				tt.prefix + "diff *":                       ok("d\n"),
				tt.prefix + "patch-id *":                   ok("abc 1\n"),
				tt.prefix + "merge *":                      ok(""),
				tt.prefix + "reset *":                      ok(""),
				tt.prefix + "symbolic-ref *":               ok("refs/remotes/origin/main\n"),
				"push *":                                   ok(""),
			})
			g := &Git{workDir: dir, exec: s.run}
			if tt.prefix != "" {
				g = g.WithoutLocalHooks()
			}
			if err := g.FetchRefspecWithTimeout("origin", "main", time.Second); err != nil {
				t.Fatalf("FetchRefspecWithTimeout: %v", err)
			}
			if _, err := g.PatchID("base", "HEAD"); err != nil {
				t.Fatalf("PatchID: %v", err)
			}
			if _, err := g.CheckConflictsAtHead("side"); err != nil {
				t.Fatalf("CheckConflictsAtHead: %v", err)
			}
			if err := g.PushWithTimeout("origin", "feature", false, time.Second); err != nil {
				t.Fatalf("PushWithTimeout: %v", err)
			}
			if err := g.PushNotes("origin", "verdicts"); err != nil {
				t.Fatalf("PushNotes: %v", err)
			}
			s.noUnscripted(t)
			want := []string{
				tt.prefix + "fetch origin main",
				tt.prefix + "patch-id --stable",
				tt.prefix + "merge --no-commit --no-ff side",
			}
			sent := s.sent()
			for _, w := range want {
				if !slices.Contains(sent, w) {
					t.Errorf("no call recorded as %q; sent %q", w, sent)
				}
			}
			for _, call := range sent {
				if !strings.Contains(call, "core.hooksPath") {
					continue
				}
				if strings.Contains(call, "push ") || tt.prefix == "" {
					t.Errorf("ran with hooks off although it must not: %q", call)
				}
			}
		})
	}
}
