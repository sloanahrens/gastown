package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// verifyProbeCall is one command a fake verify RunFunc received.
type verifyProbeCall struct {
	name string
	args []string
}

// verifyProbeRun records each command a verify probe runs and answers with one
// fixed result.
type verifyProbeRun struct {
	calls  []verifyProbeCall
	output string
	exit   int
	err    error
}

func (f *verifyProbeRun) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, verifyProbeCall{name: name, args: append([]string(nil), args...)})
	return f.output, f.exit, f.err
}

// verifyProbeExe is the executable path the probes read the installed build
// from.
func verifyProbeExe() (string, error) { return "/opt/gt/bin/gt", nil }

// TestUpdateVerifyGTVersionParsesTheVersionLine: the installed build is read
// from the file at the executable path, and only a version line answers a
// commit.
func TestUpdateVerifyGTVersionParsesTheVersionLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("parses the commit from the version line", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: "gt version d43dd6953 (dev: main@d43dd6953)\n"}
		probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

		version, err := probes.GTVersion(ctx)
		if err != nil {
			t.Fatalf("gt version: %v", err)
		}
		if version != "d43dd6953" {
			t.Errorf("version = %q, want the field after gt version", version)
		}
		want := verifyProbeCall{name: "/opt/gt/bin/gt", args: []string{"version"}}
		if len(run.calls) != 1 || run.calls[0].name != want.name || !slices.Equal(run.calls[0].args, want.args) {
			t.Errorf("ran %+v, want [%+v]", run.calls, want)
		}
	})

	t.Run("output that is not a version line is an error carrying it", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: "boom: not a version line\n"}
		probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

		_, err := probes.GTVersion(ctx)
		if err == nil {
			t.Fatal("non-version output reported no error")
		}
		if !strings.Contains(err.Error(), "boom: not a version line") {
			t.Errorf("error %q does not carry the output", err)
		}
	})

	t.Run("a nonzero exit is an error carrying the output", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: "gt: not built", exit: 2}
		probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

		_, err := probes.GTVersion(ctx)
		if err == nil {
			t.Fatal("a nonzero exit reported no error")
		}
		if !strings.Contains(err.Error(), "gt: not built") {
			t.Errorf("error %q does not carry the output", err)
		}
	})

	t.Run("a command that cannot start is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("exec: no such file")
		run := &verifyProbeRun{err: boom}
		probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

		if _, err := probes.GTVersion(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the run error", err)
		}
	})

	t.Run("an unresolved executable is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("no executable")
		probes := newVerifyProbes(verifyProbeEnv{Exe: func() (string, error) { return "", boom }})

		if _, err := probes.GTVersion(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the exe error", err)
		}
	})
}

// TestUpdateVerifyDaemonCommit: the daemon's commit is answered as recorded,
// and a state that names none never passes as a build.
func TestUpdateVerifyDaemonCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns the commit as is", func(t *testing.T) {
		t.Parallel()
		probes := newVerifyProbes(verifyProbeEnv{
			DaemonCommit: func() (string, error) { return "d43dd6953", nil },
		})

		commit, err := probes.DaemonVersion(ctx)
		if err != nil {
			t.Fatalf("daemon version: %v", err)
		}
		if commit != "d43dd6953" {
			t.Errorf("commit = %q, want the recorded commit", commit)
		}
	})

	t.Run("an empty commit is an error", func(t *testing.T) {
		t.Parallel()
		probes := newVerifyProbes(verifyProbeEnv{
			DaemonCommit: func() (string, error) { return "", nil },
		})

		if _, err := probes.DaemonVersion(ctx); err == nil {
			t.Error("an empty commit reported no error")
		}
	})

	t.Run("a loader error is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("state unreadable")
		probes := newVerifyProbes(verifyProbeEnv{
			DaemonCommit: func() (string, error) { return "", boom },
		})

		if _, err := probes.DaemonVersion(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the loader error", err)
		}
	})
}

// TestUpdateVerifyDaemonStateCommit: the daemon state is read only through a
// resolved town root, and a failure at either step is an error.
func TestUpdateVerifyDaemonStateCommit(t *testing.T) {
	t.Parallel()

	t.Run("reads the commit from the state", func(t *testing.T) {
		t.Parallel()
		commit, err := daemonCommitFrom(
			func() (string, error) { return "/town", nil },
			func(root string) (*daemon.State, error) {
				if root != "/town" {
					t.Errorf("loaded the state for %q, want the resolved town root", root)
				}
				return &daemon.State{Commit: "d43dd6953"}, nil
			},
		)
		if err != nil {
			t.Fatalf("daemon commit: %v", err)
		}
		if commit != "d43dd6953" {
			t.Errorf("commit = %q, want the state's commit", commit)
		}
	})

	t.Run("an unresolved town root is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("no town")
		_, err := daemonCommitFrom(
			func() (string, error) { return "", boom },
			func(string) (*daemon.State, error) {
				t.Fatal("loaded the state without a town root")
				return nil, nil
			},
		)
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want the town root error", err)
		}
	})

	t.Run("an unreadable state is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("state unreadable")
		_, err := daemonCommitFrom(
			func() (string, error) { return "/town", nil },
			func(string) (*daemon.State, error) { return nil, boom },
		)
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want the loader error", err)
		}
	})
}

// TestUpdateVerifyImageLabel: the label is read from gastown-polecat:dev with
// the org.gastown.gt-commit key, trimmed, and a failed inspect is an error.
func TestUpdateVerifyImageLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns the trimmed label", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: "  d43dd6953\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run})

		label, err := probes.ImageLabel(ctx)
		if err != nil {
			t.Fatalf("image label: %v", err)
		}
		if label != "d43dd6953" {
			t.Errorf("label = %q, want the trimmed label", label)
		}
		want := verifyProbeCall{
			name: "docker",
			args: []string{"image", "inspect", "gastown-polecat:dev", "--format", `{{index .Config.Labels "org.gastown.gt-commit"}}`},
		}
		if len(run.calls) != 1 || run.calls[0].name != want.name || !slices.Equal(run.calls[0].args, want.args) {
			t.Errorf("ran %+v, want [%+v]", run.calls, want)
		}
	})

	t.Run("a nonzero exit is an error carrying the output", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: "Error: No such image", exit: 1}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run})

		_, err := probes.ImageLabel(ctx)
		if err == nil {
			t.Fatal("a nonzero exit reported no error")
		}
		if !strings.Contains(err.Error(), "Error: No such image") {
			t.Errorf("error %q does not carry the output", err)
		}
	})

	t.Run("a command that cannot start is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("exec: docker not found")
		run := &verifyProbeRun{err: boom}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run})

		if _, err := probes.ImageLabel(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the run error", err)
		}
	})
}

// TestUpdateVerifyHealthTakesTheFirstLineIgnoringTheExitCode: the line still
// describes a degraded town, so the exit status does not hide it.
func TestUpdateVerifyHealthTakesTheFirstLineIgnoringTheExitCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name   string
		output string
		exit   int
		want   string
	}{
		{
			name:   "green",
			output: "GREEN tick 1m ago\nmore",
			exit:   0,
			want:   "GREEN tick 1m ago",
		},
		{
			name:   "degraded exits nonzero",
			output: "DEGRADED tick 2m ago: promote=27_behind",
			exit:   3,
			want:   "DEGRADED tick 2m ago: promote=27_behind",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &verifyProbeRun{output: tt.output, exit: tt.exit}
			probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

			line, err := probes.Health(ctx)
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if line != tt.want {
				t.Errorf("line = %q, want %q", line, tt.want)
			}
			want := verifyProbeCall{name: "/opt/gt/bin/gt", args: []string{"status", "--line"}}
			if len(run.calls) != 1 || run.calls[0].name != want.name || !slices.Equal(run.calls[0].args, want.args) {
				t.Errorf("ran %+v, want [%+v]", run.calls, want)
			}
		})
	}

	t.Run("a command that cannot start is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("exec: no such file")
		run := &verifyProbeRun{err: boom}
		probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

		if _, err := probes.Health(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the run error", err)
		}
	})

	t.Run("an unresolved executable is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("no executable")
		probes := newVerifyProbes(verifyProbeEnv{Exe: func() (string, error) { return "", boom }})

		if _, err := probes.Health(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the exe error", err)
		}
	})

	t.Run("output with no line is an error", func(t *testing.T) {
		t.Parallel()
		for _, output := range []string{"", "  \n", "\n"} {
			run := &verifyProbeRun{output: output}
			probes := newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: run.run})

			if line, err := probes.Health(ctx); err == nil {
				t.Errorf("output %q: health = %q with no error, want an error", output, line)
			}
		}
	})
}

// TestUpdateVerifyDefaultSeams: a probe built with no seams holds the
// production executable lookup, RunFunc and daemon state read. Each nil seam
// must take its production function, so deleting a default in
// newVerifyProbes turns this red. Nothing is called: the defaults would run
// the installed gt or read the live town.
func TestUpdateVerifyDefaultSeams(t *testing.T) {
	t.Parallel()
	p, ok := newVerifyProbes(verifyProbeEnv{}).(*verifyProbes)
	if !ok {
		t.Fatal("newVerifyProbes did not return the verify probes")
	}

	if got, want := reflect.ValueOf(p.env.Exe).Pointer(), reflect.ValueOf(os.Executable).Pointer(); got != want {
		t.Errorf("Exe seam = %#x, want os.Executable (%#x)", got, want)
	}
	if got, want := reflect.ValueOf(p.env.Run).Pointer(), reflect.ValueOf(update.ExecRun).Pointer(); got != want {
		t.Errorf("Run seam = %#x, want update.ExecRun (%#x)", got, want)
	}
	if got, want := reflect.ValueOf(p.env.DaemonCommit).Pointer(), reflect.ValueOf(daemonStateCommit).Pointer(); got != want {
		t.Errorf("DaemonCommit seam = %#x, want daemonStateCommit (%#x)", got, want)
	}
}

// TestVerifyImageLabelReadsTheValuesFile holds the image the probe inspects: the
// sandbox image by name when there is no values file, and the image the file
// names when there is one, whose label must be the file's commit.
func TestVerifyImageLabelReadsTheValuesFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		fileCommit  = "0123456789abcdef0123456789abcdef01234567"
		otherCommit = "fedcba9876543210fedcba9876543210fedcba98"
		polecatRef  = "registry.internal:5000/gastown/gastown-polecat@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	)
	label := `{{index .Config.Labels "org.gastown.gt-commit"}}`
	writeValues := func(t *testing.T, dir string, v imagevalues.Values) {
		t.Helper()
		if err := imagevalues.Write(filepath.Join(dir, imagevalues.DefaultRelPath), v); err != nil {
			t.Fatalf("writing the values file: %v", err)
		}
	}
	values := imagevalues.Values{
		Daemon:  "registry.internal:5000/gastown/gastown-daemon@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222",
		Polecat: polecatRef,
		Commit:  fileCommit,
	}

	t.Run("no values file inspects the sandbox image by name", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: fileCommit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run, ValuesDir: func() string { return t.TempDir() }})

		if _, err := probes.ImageLabel(ctx); err != nil {
			t.Fatalf("image label: %v", err)
		}
		if len(run.calls) != 1 || !slices.Contains(run.calls[0].args, "gastown-polecat:dev") {
			t.Errorf("ran %+v, want an inspect of gastown-polecat:dev", run.calls)
		}
	})

	t.Run("a nil ValuesDir inspects the sandbox image by name", func(t *testing.T) {
		t.Parallel()
		run := &verifyProbeRun{output: fileCommit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run})

		if _, err := probes.ImageLabel(ctx); err != nil {
			t.Fatalf("image label: %v", err)
		}
		if len(run.calls) != 1 || !slices.Contains(run.calls[0].args, "gastown-polecat:dev") {
			t.Errorf("ran %+v, want an inspect of gastown-polecat:dev", run.calls)
		}
	})

	t.Run("a values file inspects the image it names", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeValues(t, dir, values)
		run := &verifyProbeRun{output: fileCommit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run, ValuesDir: func() string { return dir }})

		got, err := probes.ImageLabel(ctx)
		if err != nil {
			t.Fatalf("image label: %v", err)
		}
		if got != fileCommit {
			t.Errorf("label = %q, want the file's commit %q", got, fileCommit)
		}
		want := []string{"image", "inspect", polecatRef, "--format", label}
		if len(run.calls) != 1 || !slices.Equal(run.calls[0].args, want) {
			t.Errorf("ran %+v, want an inspect of %s", run.calls, polecatRef)
		}
	})

	t.Run("a label that differs from the file's commit is an error naming both", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeValues(t, dir, values)
		run := &verifyProbeRun{output: otherCommit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run, ValuesDir: func() string { return dir }})

		_, err := probes.ImageLabel(ctx)
		if err == nil {
			t.Fatal("a label that differs from the file's commit reported no error")
		}
		for _, commit := range []string{otherCommit, fileCommit} {
			if !strings.Contains(err.Error(), commit) {
				t.Errorf("error %q does not name commit %s", err, commit)
			}
		}
	})

	t.Run("a values file that cannot be read is an error, not the sandbox image", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// A directory where the file should be: present, but not a values file.
		if err := os.MkdirAll(filepath.Join(dir, imagevalues.DefaultRelPath), 0o755); err != nil {
			t.Fatal(err)
		}
		run := &verifyProbeRun{output: fileCommit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{Run: run.run, ValuesDir: func() string { return dir }})

		if _, err := probes.ImageLabel(ctx); err == nil {
			t.Fatal("an unreadable values file reported no error")
		}
		if len(run.calls) != 0 {
			t.Errorf("ran %+v after an unreadable values file, want no inspect", run.calls)
		}
	})
}
