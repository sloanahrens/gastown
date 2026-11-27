package rehearsal

import (
	"context"
	"slices"
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
