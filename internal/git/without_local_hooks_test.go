package git

import (
	"slices"
	"strings"
	"testing"
)

// TestHooksOffArgs pins where `-c core.hooksPath=/dev/null` lands: before the
// subcommand but after every global option, and nowhere on a push.
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
