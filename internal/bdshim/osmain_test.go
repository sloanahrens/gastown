package bdshim

import (
	"os"
	"strings"
	"testing"
)

func TestChildStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		end  childStatus
		want int
	}{
		{"a clean exit", childStatus{code: 0}, 0},
		{"bd's own failing status", childStatus{code: 13}, 13},
		{"a signal death", childStatus{code: -1, signal: 9}, 137},
		{"an end with no status", childStatus{code: -1}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.end.ExitCode(); got != tc.want {
				t.Errorf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOSEnvIsTheProductionSurface(t *testing.T) {
	t.Parallel()
	e := osEnv([]string{"show", "gt-abc"})

	if strings.Join(e.Args, " ") != "show gt-abc" {
		t.Errorf("Args = %v, want bd's argv", e.Args)
	}
	if e.Handlers == nil {
		t.Error("Handlers is nil, so no tracker verb could ever be answered")
	}
	if e.Getenv == nil || e.Getwd == nil || e.Executable == nil || e.Run == nil {
		t.Error("a surface function is nil, so a real run would panic")
	}
	if e.Stderr != os.Stderr {
		t.Errorf("Stderr = %v, want the process's own", e.Stderr)
	}
}

func TestRealCommandInheritsTheProcess(t *testing.T) {
	t.Parallel()
	argv := []string{"show", "gt-abc", "--json"}
	cmd := realCommand("/opt/bin/bd.real", argv)

	if cmd.Path != "/opt/bin/bd.real" {
		t.Errorf("Path = %q, want bd.real", cmd.Path)
	}
	if got := strings.Join(cmd.Args, " "); got != "/opt/bin/bd.real "+strings.Join(argv, " ") {
		t.Errorf("Args = %q, want bd.real with bd's argv unchanged", got)
	}
	if cmd.Env != nil {
		t.Errorf("Env = %v, want nil so the environment is inherited", cmd.Env)
	}
	if cmd.Stdin != os.Stdin || cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Errorf("streams = %v/%v/%v, want the inherited os.Stdin/Stdout/Stderr", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}
