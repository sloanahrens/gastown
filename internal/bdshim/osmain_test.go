package bdshim

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

func TestRunBdRealReportsAChildThatNeverStarted(t *testing.T) {
	t.Parallel()
	// A path inside the test's own directory that nothing created: the run
	// fails before any process exists, which is the branch a missing bd.real
	// takes.
	missing := filepath.Join(t.TempDir(), "bd.real")

	err := runBdReal(missing, []string{"show", "gt-abc"})
	if err == nil {
		t.Fatal("runBdReal of a missing bd.real = nil, want the start failure")
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		t.Errorf("runBdReal of a missing bd.real = %v, want the start failure itself rather than a child status", err)
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

// processRun drives Main with the wiring osEnv uses, each process read a fake:
// the working directory and BEADS_DIR the test names, the shim's own path in a
// bin directory of its own, and bd.real as proc records it. It returns the exit
// code, the shim's stderr and the shim's path.
func processRun(t *testing.T, cwd, beadsDir string, proc *fakeProcess, args ...string) (int, *bytes.Buffer, string) {
	t.Helper()
	stderr := &bytes.Buffer{}
	shimPath := filepath.Join(t.TempDir(), "bin", "bdshim")
	e := processEnv(args, beadsDirOnly(beadsDir),
		func() (string, error) { return cwd, nil },
		func() (string, error) { return shimPath, nil },
		proc.run, stderr)
	return Main(e), stderr, shimPath
}

func TestProcessWiringRoutesLikeTheShim(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	beta := filepath.Join(town, "beta", "mayor", "rig")
	const refusalBeta = "bdshim: verb list is not implemented for tracker rig bt-\n"

	t.Run("a tracker cwd refuses a verb without running bd.real", func(t *testing.T) {
		t.Parallel()
		proc := &fakeProcess{}
		got, stderr, _ := processRun(t, beta, "", proc, "list")

		if got != 1 {
			t.Errorf("Main = %d, want 1", got)
		}
		if stderr.String() != refusalBeta {
			t.Errorf("stderr = %q, want %q", stderr.String(), refusalBeta)
		}
		if proc.ran != nil {
			t.Errorf("Main ran %v, want no fallback to bd.real", proc.ran)
		}
	})

	t.Run("a tracker id after a boolean flag is refused from a dolt cwd", func(t *testing.T) {
		t.Parallel()
		proc := &fakeProcess{}
		got, stderr, _ := processRun(t, alpha, "", proc, "show", "--json", "bt-abc")

		if got != 1 {
			t.Errorf("Main = %d, want 1", got)
		}
		if want := "bdshim: verb show is not implemented for tracker rig bt-\n"; stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
		if proc.ran != nil {
			t.Errorf("Main ran %v, want the tracker id kept away from bd.real", proc.ran)
		}
	})

	t.Run("a dolt cwd runs bd.real beside the shim and returns its status", func(t *testing.T) {
		t.Parallel()
		args := []string{"show", "al-abc", "--json"}
		proc := &fakeProcess{err: fakeChild(13)}
		got, _, shimPath := processRun(t, alpha, "", proc, args...)

		if got != 13 {
			t.Errorf("Main = %d, want bd.real's 13", got)
		}
		if want := filepath.Join(filepath.Dir(shimPath), realName); proc.path != want {
			t.Errorf("Main ran %q, want bd.real beside the shim at %q", proc.path, want)
		}
		if strings.Join(proc.ran, " ") != strings.Join(args, " ") {
			t.Errorf("Main ran %v, want bd's argv %v unchanged", proc.ran, args)
		}
	})

	t.Run("a relative BEADS_DIR routes from the cwd to the tracker rig", func(t *testing.T) {
		t.Parallel()
		proc := &fakeProcess{}
		got, stderr, _ := processRun(t, alpha, "../../../beta/mayor/rig/.beads", proc, "list")

		if got != 1 {
			t.Errorf("Main = %d, want 1", got)
		}
		if stderr.String() != refusalBeta {
			t.Errorf("stderr = %q, want %q", stderr.String(), refusalBeta)
		}
		if proc.ran != nil {
			t.Errorf("Main ran %v, want no fallback to bd.real", proc.ran)
		}
	})
}
