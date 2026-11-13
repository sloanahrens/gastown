package bdgateway

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestExecutorRealProcess is the one test in this package that starts a
// process. It builds testdata/bd into t.TempDir and points the executor's
// injected binary at it, so the real runner's exit status, its two streams and
// the timeout kill are shown on a process rather than on a fake. Every other
// test in the package drives fakeRunner.
func TestExecutorRealProcess(t *testing.T) {
	t.Parallel()
	helper := buildBDStandIn(t)
	e, err := New(Options{
		Binary:        helper,
		WorkspaceRoot: testWorkspace(t),
		Home:          t.TempDir(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("exit status and streams", func(t *testing.T) {
		var out bytes.Buffer
		if err := e.Exec(context.Background(), Request{Argv: []string{"3"}, Rig: "gastown"}, nil, &out); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) == 0 {
			t.Fatal("no frames")
		}
		if got, err := frames[len(frames)-1].ExitStatus(); err != nil || got != 3 {
			t.Errorf("exit status = %d (%v), want 3", got, err)
		}
		if got := streamOf(frames, FrameStdout); got != "out\n" {
			t.Errorf("stdout frames = %q, want %q", got, "out\n")
		}
		if got := streamOf(frames, FrameStderr); got != "err\n" {
			t.Errorf("stderr frames = %q, want %q", got, "err\n")
		}
	})

	t.Run("timeout kills the process", func(t *testing.T) {
		var out bytes.Buffer
		err := e.Exec(context.Background(), Request{Argv: []string{"hold"}, Rig: "gastown", TimeoutMS: 100}, nil, &out)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Exec = %v, want ErrTimeout", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
		}
		refusal, err := frames[0].GatewayError()
		if err != nil || refusal.Code != CodeTimeout {
			t.Errorf("error frame = %+v (%v), want code %q", refusal, err, CodeTimeout)
		}
		if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
			t.Errorf("exit status = %d (%v), want 124", status, err)
		}
	})
}

// buildBDStandIn builds testdata/bd with the go tool found under GOROOT. The
// tool is run by absolute path, so the unit tier's refused-tool tripwire on
// PATH is never consulted; -buildvcs=false keeps the build from running git.
func buildBDStandIn(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no source path for the bd stand-in")
	}
	bin := filepath.Join(t.TempDir(), "bd-stand-in")
	//testpolicy:allow no-subprocess — the compiled stand-in is what the bead names: the go tool is run by absolute path to build it, and no other tool is started
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-buildvcs=false", "-o", bin, "./testdata/bd")
	cmd.Dir = filepath.Dir(file)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the bd stand-in: %v\n%s", err, out)
	}
	return bin
}
