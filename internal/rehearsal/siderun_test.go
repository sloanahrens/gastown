package rehearsal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRehearsalSideRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env, argv := []string{"GOCACHE=" + dir}, []string{dir + "/make", "presubmit"}
	if cmd, err := sideCmd(context.Background(), dir, env, argv); err != nil {
		t.Fatalf("sideCmd(%v): %v", argv, err)
	} else if !slices.Equal(cmd.Args, argv) || cmd.Dir != dir || !slices.Equal(cmd.Env, env) || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatalf("sideCmd(%v) = args %v, dir %q, %d env entries, output %v/%v; want args %v, dir %q, %d env entries, nil output", argv, cmd.Args, cmd.Dir, len(cmd.Env), cmd.Stdout, cmd.Stderr, argv, dir, len(env))
	}
	for name, argv := range map[string][]string{"nil argv": nil, "empty argv": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if cmd, err := sideCmd(context.Background(), dir, env, argv); err == nil || cmd != nil {
				t.Errorf("sideCmd(%v) = %v, %v, want nil, an error", argv, cmd, err)
			}
		})
	}
}

// TestRehearsalSideRunRefusesWithoutProcess drives the failures that start no process.
func TestRehearsalSideRunRefusesWithoutProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		argv   []string
		want   error
		wantIn string
	}{
		{name: "empty argv", ctx: context.Background(), wantIn: "empty argv"},
		{name: "program not on PATH", ctx: context.Background(), argv: []string{"no-such-side-program", "presubmit", "--token=sekret"}, wantIn: "side run no-such-side-program presubmit"},
		{name: "canceled context", ctx: ctx, argv: []string{"no-such-side-program", "presubmit"}, want: context.Canceled, wantIn: "side run no-such-side-program presubmit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, err := ExecSideRun(tc.ctx, t.TempDir(), nil, tc.argv)
			if code != -1 || err == nil || !strings.Contains(err.Error(), tc.wantIn) || strings.Contains(err.Error(), "sekret") || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("ExecSideRun(%v) = %d, %v; want -1 and an error wrapping %v and naming %q", tc.argv, code, err, tc.want, tc.wantIn)
			}
		})
	}
}
