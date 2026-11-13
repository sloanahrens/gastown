package bdgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExecutorPathMapping: rig and cwd select the directory bd runs in and the
// BEADS_DIR it reads, and a cwd that leaves the workspace or a rig that is not
// one path element is refused before any process starts.
func TestExecutorPathMapping(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	cases := []struct {
		name      string
		rig       string
		cwd       string
		wantDir   string
		wantBeads string
		wantErr   error
	}{
		{
			name: "rig and a crew directory", rig: "gastown", cwd: "gastown/crew/dipper",
			wantDir:   filepath.Join(root, "gastown", "crew", "dipper"),
			wantBeads: filepath.Join(root, "gastown", ".beads"),
		},
		{
			name: "rig with no cwd", rig: "gastown",
			wantDir:   root,
			wantBeads: filepath.Join(root, "gastown", ".beads"),
		},
		{
			name:      "town call",
			wantDir:   root,
			wantBeads: filepath.Join(root, ".beads"),
		},
		{name: "cwd is the parent", rig: "gastown", cwd: "..", wantErr: ErrCwdOutsideWorkspace},
		{name: "cwd escapes upward", rig: "gastown", cwd: "gastown/../../elsewhere", wantErr: ErrCwdOutsideWorkspace},
		{name: "cwd is absolute", rig: "gastown", cwd: "/gastown", wantErr: ErrCwdOutsideWorkspace},
		{name: "rig names two elements", rig: "gastown/crew", wantErr: ErrBadRig},
		{name: "rig is the parent", rig: "..", wantErr: ErrBadRig},
		{name: "rig is the current directory", rig: ".", wantErr: ErrBadRig},
		{name: "rig has no workspace", rig: "elsewhere", wantErr: ErrNoWorkspace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			e := newTestExecutor(t, root, runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: tc.rig, Cwd: tc.cwd}, nil, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				if out.Len() != 0 {
					t.Errorf("a refusal before the process wrote frames: %q", out.Bytes())
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			call := runner.only(t)
			if call.dir != tc.wantDir {
				t.Errorf("bd's directory = %q, want %q", call.dir, tc.wantDir)
			}
			if got := envMap(call.env)["BEADS_DIR"]; got != tc.wantBeads {
				t.Errorf("BEADS_DIR = %q, want %q", got, tc.wantBeads)
			}
			if fi, err := os.Stat(call.dir); err != nil || !fi.IsDir() {
				t.Errorf("bd's directory %q is not a directory: %v", call.dir, err)
			}
		})
	}

	t.Run("a beads path that is not a directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "gastown"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "gastown", ".beads"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		e := newTestExecutor(t, root, runner)
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard)
		if !errors.Is(err, ErrNoWorkspace) {
			t.Fatalf("Exec = %v, want ErrNoWorkspace", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
	})

	t.Run("a cwd behind a symbolic link", func(t *testing.T) {
		// A local path can still leave the tree through a link, so a link below
		// the root is refused before it reaches bd's directory.
		root := testWorkspace(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		e := newTestExecutor(t, root, runner)
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", Cwd: "escape"}, nil, io.Discard)
		if !errors.Is(err, ErrCwdOutsideWorkspace) {
			t.Fatalf("Exec = %v, want ErrCwdOutsideWorkspace", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
	})

	t.Run("a beads path behind a symbolic link", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "gastown"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, ".beads"), filepath.Join(root, "gastown", ".beads")); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		e := newTestExecutor(t, root, runner)
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard)
		if !errors.Is(err, ErrNoWorkspace) {
			t.Fatalf("Exec = %v, want ErrNoWorkspace", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
	})

	t.Run("a cwd that is not a path", func(t *testing.T) {
		// A NUL byte cannot name anything, so the walk itself refuses it
		// instead of handing it to bd.
		runner := &fakeRunner{}
		e := newTestExecutor(t, testWorkspace(t), runner)
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", Cwd: "a\x00b"}, nil, io.Discard)
		if !errors.Is(err, ErrNoWorkspace) {
			t.Fatalf("Exec = %v, want ErrNoWorkspace", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
	})

	t.Run("a cwd the workspace cannot hold", func(t *testing.T) {
		// A regular file where the caller's directory would be created makes
		// MkdirAll fail the way a read-only workspace volume would.
		root := testWorkspace(t)
		if err := os.WriteFile(filepath.Join(root, "gastown", "crew"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		e := newTestExecutor(t, root, runner)
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", Cwd: "gastown/crew/dipper"}, nil, io.Discard)
		if !errors.Is(err, ErrNoWorkspace) {
			t.Fatalf("Exec = %v, want ErrNoWorkspace", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
	})
}

// TestExecutorExit: bd's stdout and stderr come back as their frames, in order
// within each stream, and the exit status the process ended with is the last
// frame's.
func TestExecutorExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		stdout string
		stderr string
	}{
		{name: "zero", status: 0},
		{name: "status three", status: 3, stdout: "out\n", stderr: "err\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{queue: []Process{newExitedFake(tc.status, tc.stdout, tc.stderr)}}
			e := newTestExecutor(t, testWorkspace(t), runner)
			var out bytes.Buffer
			if err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}, nil, &out); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			frames := readFrames(t, out.Bytes())
			if len(frames) == 0 {
				t.Fatal("no frames")
			}
			last := frames[len(frames)-1]
			if last.Kind != FrameExit {
				t.Fatalf("last frame kind = %d, want the exit frame", last.Kind)
			}
			if got, err := last.ExitStatus(); err != nil || got != tc.status {
				t.Errorf("exit status = %d (%v), want %d", got, err, tc.status)
			}
			if got := streamOf(frames, FrameStdout); got != tc.stdout {
				t.Errorf("stdout frames = %q, want %q", got, tc.stdout)
			}
			if got := streamOf(frames, FrameStderr); got != tc.stderr {
				t.Errorf("stderr frames = %q, want %q", got, tc.stderr)
			}
		})
	}
}

// TestExecutorTimeout: a call that outlives its timeout kills the process
// group, names the timeout in an error frame and ends with the design's 124,
// and the refusal does not repeat the caller's argv, which can hold free text.
func TestExecutorTimeout(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	runner := &fakeRunner{queue: []Process{proc}}
	e := newTestExecutor(t, testWorkspace(t), runner)
	var out bytes.Buffer
	err := e.Exec(context.Background(), Request{
		Argv:      []string{"create", "a private bead title"},
		Rig:       "gastown",
		TimeoutMS: 20,
	}, nil, &out)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Exec = %v, want ErrTimeout", err)
	}
	if !proc.wasKilled() {
		t.Error("the process group was not killed")
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
	}
	refusal, err := frames[0].GatewayError()
	if err != nil {
		t.Fatalf("error frame: %v", err)
	}
	if refusal.Code != CodeTimeout {
		t.Errorf("refusal code = %q, want %q", refusal.Code, CodeTimeout)
	}
	if strings.Contains(refusal.Message, "private bead title") {
		t.Errorf("the refusal repeats the caller's text: %q", refusal.Message)
	}
	if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
		t.Errorf("exit status = %d (%v), want 124", status, err)
	}
}

// TestExecutorTimeoutKillFails: a kill that fails is carried in the returned
// error beside the timeout, so a call that could not stop bd does not read as
// one that did.
func TestExecutorTimeoutKillFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("the group would not die")
	proc := newBlockingFake("")
	proc.killErr = boom
	e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}})
	err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", TimeoutMS: 20}, nil, io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Exec = %v, want ErrTimeout", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Exec = %v, want the failed kill in the error", err)
	}
}

// TestExecutorStdinHeld: a body bd never reads is bounded by the call's own
// end the same way its output is, so a write nobody will take cannot hold the
// call past its timeout.
func TestExecutorStdinHeld(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	proc.heldStdin = newHeldStdin()
	e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}})
	err := e.Exec(context.Background(), Request{
		Argv:      []string{"create", "-"},
		Rig:       "gastown",
		TimeoutMS: 20,
	}, bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Exec = %v, want ErrTimeout", err)
	}
	if !proc.wasKilled() {
		t.Error("the process group was not killed")
	}
}

// TestExecutorHeldOpen: bd has ended with a status of its own, but a process it
// left behind still holds its stdout open. The call ends at that status instead
// of waiting past its deadline for a pipe nobody will close, and a bd that
// finished is not reported as a timeout. A kill that failed while doing so is
// carried in the error, since the frames report success.
func TestExecutorHeldOpen(t *testing.T) {
	t.Parallel()
	proc := func(killErr error) *fakeProcess {
		p := newFakeProcess(0, "", "", nil)
		p.stdout = newHeldReader()
		p.killErr = killErr
		return p
	}
	t.Run("bd ended before the kill", func(t *testing.T) {
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc(os.ErrProcessDone)}})
		var out bytes.Buffer
		if err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown", TimeoutMS: 20}, nil, &out); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 1 || frames[0].Kind != FrameExit {
			t.Fatalf("frames = %+v, want the exit frame alone", frames)
		}
		if got, err := frames[0].ExitStatus(); err != nil || got != 0 {
			t.Errorf("exit status = %d (%v), want 0", got, err)
		}
	})
	t.Run("the kill that failed", func(t *testing.T) {
		boom := errors.New("the group would not die")
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc(boom)}})
		var out bytes.Buffer
		err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown", TimeoutMS: 20}, nil, &out)
		if !errors.Is(err, boom) {
			t.Fatalf("Exec = %v, want the failed kill in the error", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 1 || frames[0].Kind != FrameExit {
			t.Fatalf("frames = %+v, want the exit frame alone", frames)
		}
		if got, err := frames[0].ExitStatus(); err != nil || got != 0 {
			t.Errorf("exit status = %d (%v), want 0", got, err)
		}
	})
}

// TestExecutorDisconnect: a client that cancels the request, or one whose
// stream breaks mid-call, still gets bd's process group killed, and the
// executor names the client as the reason.
func TestExecutorDisconnect(t *testing.T) {
	t.Parallel()
	t.Run("canceled context", func(t *testing.T) {
		proc := newBlockingFake("")
		runner := &fakeRunner{queue: []Process{proc}, started: make(chan struct{})}
		e := newTestExecutor(t, testWorkspace(t), runner)
		ctx, cancel := context.WithCancel(context.Background())
		var out bytes.Buffer
		done := make(chan error, 1)
		go func() { done <- e.Exec(ctx, Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &out) }()
		<-runner.started
		cancel()
		err := <-done
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
		if !proc.wasKilled() {
			t.Error("the process group was not killed")
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
		}
		refusal, err := frames[0].GatewayError()
		if err != nil || refusal.Code != CodeCanceled {
			t.Errorf("error frame = %+v (%v), want code %q", refusal, err, CodeCanceled)
		}
		if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
			t.Errorf("exit status = %d (%v), want 124", status, err)
		}
	})
	t.Run("canceled before the call", func(t *testing.T) {
		// The client ended the request before it arrived: no process starts and
		// no frames are written, since there is no bd for the kill to reach.
		runner := &fakeRunner{}
		e := newTestExecutor(t, testWorkspace(t), runner)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out bytes.Buffer
		err := e.Exec(ctx, Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &out)
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
		if got := len(runner.records()); got != 0 {
			t.Errorf("runner started %d calls, want 0", got)
		}
		if out.Len() != 0 {
			t.Errorf("a refusal before the process wrote frames: %q", out.Bytes())
		}
	})
	t.Run("broken stream", func(t *testing.T) {
		proc := newBlockingFake("out that cannot be written\n")
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}})
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, failingWriter{})
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
		if !proc.wasKilled() {
			t.Error("the process group was not killed")
		}
	})
	t.Run("the exit frame cannot be written", func(t *testing.T) {
		// A process that writes nothing, so the exit frame is the first and
		// only write the client's broken stream refuses.
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{newExitedFake(0, "", "")}})
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, failingWriter{})
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
	})
}

// TestExecutorEndings: a start that fails, a wait that fails and a process
// that ended on a signal each write the gateway's error frame and the design's
// 125, and each names its cause in the returned error.
func TestExecutorEndings(t *testing.T) {
	t.Parallel()
	boom := errors.New("the machine gave up")
	cases := []struct {
		name     string
		runner   *fakeRunner
		wantCode string
		wantErr  error
	}{
		{
			name:     "start fails",
			runner:   &fakeRunner{err: boom},
			wantCode: CodeStartFailed,
			wantErr:  boom,
		},
		{
			name:     "wait fails",
			runner:   &fakeRunner{queue: []Process{newFakeProcess(0, "", "", boom)}},
			wantCode: CodeWaitFailed,
			wantErr:  boom,
		},
		{
			name:     "ended on a signal",
			runner:   &fakeRunner{queue: []Process{newFakeProcess(-1, "", "", nil)}},
			wantCode: CodeSignaled,
			wantErr:  ErrSignaled,
		},
		{
			// bd's output cannot be read back, which is the gateway's failure
			// rather than bd's, so it names the read and kills the group.
			name:     "the output cannot be read",
			runner:   &fakeRunner{queue: []Process{newUnreadableFake(boom)}},
			wantCode: CodeWaitFailed,
			wantErr:  errRead,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestExecutor(t, testWorkspace(t), tc.runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}, nil, &out)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
			}
			frames := readFrames(t, out.Bytes())
			if len(frames) != 2 {
				t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
			}
			refusal, err := frames[0].GatewayError()
			if err != nil || refusal.Code != tc.wantCode {
				t.Errorf("error frame = %+v (%v), want code %q", refusal, err, tc.wantCode)
			}
			if status, err := frames[1].ExitStatus(); err != nil || status != 125 {
				t.Errorf("exit status = %d (%v), want 125", status, err)
			}
		})
	}
}

// TestExecutorCap: with every slot taken the next call is refused by name
// before a process starts, and the slot a call held is released when it ends.
func TestExecutorCap(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	runner := &fakeRunner{queue: []Process{proc}, started: make(chan struct{})}
	e, err := New(Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: testWorkspace(t),
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
		MaxConcurrent: 1,
		runner:        runner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var first bytes.Buffer
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &first)
	}()
	<-runner.started

	var second bytes.Buffer
	err = e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &second)
	if !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("the call over the cap = %v, want ErrTooManyCalls", err)
	}
	if got := len(runner.records()); got != 1 {
		t.Errorf("runner started %d calls, want 1", got)
	}
	if second.Len() != 0 {
		t.Errorf("the refused call wrote frames: %q", second.Bytes())
	}

	proc.finish(0)
	if err := <-firstDone; err != nil {
		t.Fatalf("the call holding the slot: %v", err)
	}
}

// TestExecutorStdinCap: a body over the cap is refused before the process
// starts, a body at the cap reaches bd byte for byte, and a broken body is
// reported rather than started.
func TestExecutorStdinCap(t *testing.T) {
	t.Parallel()
	boom := errors.New("the body broke")
	cases := []struct {
		name     string
		stdin    io.Reader
		wantErr  error
		wantBody string
	}{
		{name: "exactly at the cap", stdin: bytes.NewReader(bytes.Repeat([]byte("x"), MaxStdinBytes)), wantBody: strings.Repeat("x", MaxStdinBytes)},
		{name: "over the cap", stdin: bytes.NewReader(bytes.Repeat([]byte("x"), MaxStdinBytes+1)), wantErr: ErrStdinTooLarge},
		{name: "broken body", stdin: errorReader{err: boom}, wantErr: boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := newExitedFake(0, "", "")
			runner := &fakeRunner{queue: []Process{proc}}
			e := newTestExecutor(t, testWorkspace(t), runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"create", "-"}, Rig: "gastown"}, tc.stdin, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if got, closed := proc.stdin.contents(); got != tc.wantBody || !closed {
				t.Errorf("bd's stdin = %d bytes (closed %v), want %d bytes closed", len(got), closed, len(tc.wantBody))
			}
		})
	}
}

// TestExecutorEnv: bd's environment is the computed half plus the caller's
// allowlisted keys, and every other key is refused rather than dropped, so a
// caller cannot aim bd at another database, home or token.
func TestExecutorEnv(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	cases := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{name: "no caller env"},
		{name: "one actor", env: map[string]string{"BD_ACTOR": "town/crew/dipper"}},
		{
			name: "the whole allowlist",
			env: map[string]string{
				"BD_ACTOR":            "town/crew/dipper",
				"BEADS_ACTOR":         "town/crew/dipper",
				"BD_MACHINE":          "daemon",
				"BD_JSON_ENVELOPE":    "1",
				"BD_READONLY":         "true",
				"BD_DOLT_AUTO_COMMIT": "1",
			},
		},
		{name: "an unknown key", env: map[string]string{"GT_ROOT": "/Users/sloan/gt"}, wantErr: ErrEnvNotAllowed},
		{name: "BEADS_DIR", env: map[string]string{"BEADS_DIR": "/etc"}, wantErr: ErrEnvNotAllowed},
		{name: "HOME", env: map[string]string{"HOME": "/root"}, wantErr: ErrEnvNotAllowed},
		{name: "PATH", env: map[string]string{"PATH": "/bin"}, wantErr: ErrEnvNotAllowed},
		{name: "BD_ALLOW_NEW_DATABASE", env: map[string]string{"BD_ALLOW_NEW_DATABASE": "1"}, wantErr: ErrEnvNotAllowed},
		{name: "a NUL value", env: map[string]string{"BD_ACTOR": "a\x00b"}, wantErr: ErrEnvNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			e := newTestExecutor(t, root, runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown", Env: tc.env}, nil, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			env := envMap(runner.only(t).env)
			for k, want := range tc.env {
				if got := env[k]; got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			for k, want := range map[string]string{
				"HOME":                   "/home/bdgw",
				"PATH":                   "/usr/local/bin:/usr/bin:/bin",
				"BEADS_DIR":              filepath.Join(root, "gastown", ".beads"),
				"BEADS_DOLT_SERVER_HOST": "gt-dolt",
				"BEADS_DOLT_SERVER_PORT": "3307",
				"TRACKER_URL":            "http://127.0.0.1:7433",
				"TRACKER_TOKEN_FILE":     "/run/secrets/tracker.token",
			} {
				if got := env[k]; got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if got, want := len(env), 7+len(tc.env); got != want {
				t.Errorf("environment holds %d entries, want %d: %v", got, want, env)
			}
		})
	}
}

// TestExecutorTimeoutValue: the head's timeout_ms is the call's bound when it
// names one, zero takes the executor's default, and a negative value or one
// that does not fit a Duration is refused before a process starts.
func TestExecutorTimeoutValue(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	// The largest timeout_ms whose milliseconds fit a Duration, plus one. A
	// Duration this long would be a negative (unbounded) deadline.
	largestMS := int64(math.MaxInt64) / int64(time.Millisecond)
	cases := []struct {
		name           string
		headMS         int
		defaultTimeout time.Duration
		wantRefused    error
	}{
		// An hour's default makes the head's 20 ms the only bound that can end
		// this call quickly: an executor that ignored the head would fail here
		// at the package timeout instead of passing.
		{name: "the head's value over the default", headMS: 20, defaultTimeout: time.Hour},
		{name: "zero takes the default", headMS: 0, defaultTimeout: 20 * time.Millisecond},
		{name: "a negative value", headMS: -1, defaultTimeout: time.Hour, wantRefused: ErrBadTimeout},
		{name: "over a Duration", headMS: int(largestMS) + 1, defaultTimeout: time.Hour, wantRefused: ErrBadTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{queue: []Process{newBlockingFake("")}}
			e, err := New(Options{
				Binary:         "/opt/bd/bd",
				WorkspaceRoot:  root,
				Home:           "/home/bdgw",
				Path:           "/usr/local/bin:/usr/bin:/bin",
				DoltHost:       "gt-dolt",
				DoltPort:       "3307",
				DefaultTimeout: tc.defaultTimeout,
				runner:         runner,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", TimeoutMS: tc.headMS}, nil, io.Discard)
			if tc.wantRefused != nil {
				if !errors.Is(err, tc.wantRefused) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantRefused)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				return
			}
			if !errors.Is(err, ErrTimeout) {
				t.Errorf("Exec = %v, want ErrTimeout", err)
			}
		})
	}
}

// TestExecutorUnwired: an executor built without New refuses rather than
// running with a nil semaphore that would block or default.
func TestExecutorUnwired(t *testing.T) {
	t.Parallel()
	for name, e := range map[string]*Executor{"nil": nil, "zero value": {}} {
		t.Run(name, func(t *testing.T) {
			if err := e.Exec(context.Background(), Request{Argv: []string{"list"}}, nil, io.Discard); !errors.Is(err, ErrUnconfigured) {
				t.Errorf("Exec = %v, want ErrUnconfigured", err)
			}
		})
	}
	t.Run("no frame writer", func(t *testing.T) {
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{})
		if err := e.Exec(context.Background(), Request{Argv: []string{"list"}}, nil, nil); !errors.Is(err, ErrUnconfigured) {
			t.Errorf("Exec = %v, want ErrUnconfigured", err)
		}
	})
}

// TestExecutorNew: every field bd cannot run without is refused by name, the
// two limits reject negatives, and zero takes the design's defaults.
func TestExecutorNew(t *testing.T) {
	t.Parallel()
	base := Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: "/ws",
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	}
	mutations := map[string]func(*Options){
		"no binary":            func(o *Options) { o.Binary = "" },
		"no workspace root":    func(o *Options) { o.WorkspaceRoot = "" },
		"relative workspace":   func(o *Options) { o.WorkspaceRoot = "ws" },
		"no HOME":              func(o *Options) { o.Home = "" },
		"no PATH":              func(o *Options) { o.Path = "" },
		"no Dolt host":         func(o *Options) { o.DoltHost = "" },
		"no Dolt port":         func(o *Options) { o.DoltPort = "" },
		"negative concurrency": func(o *Options) { o.MaxConcurrent = -1 },
		"negative timeout":     func(o *Options) { o.DefaultTimeout = -time.Second },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			opts := base
			mutate(&opts)
			if _, err := New(opts); !errors.Is(err, ErrUnconfigured) {
				t.Errorf("New = %v, want ErrUnconfigured", err)
			}
		})
	}

	e, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := cap(e.slots); got != DefaultMaxConcurrent {
		t.Errorf("default concurrency cap = %d, want %d", got, DefaultMaxConcurrent)
	}
	if e.defaultTimeout != DefaultTimeout {
		t.Errorf("default timeout = %s, want %s", e.defaultTimeout, DefaultTimeout)
	}

	opts := base
	opts.MaxConcurrent = 4
	opts.DefaultTimeout = 5 * time.Second
	e, err = New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := cap(e.slots); got != 4 {
		t.Errorf("concurrency cap = %d, want 4", got)
	}
	if e.defaultTimeout != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", e.defaultTimeout)
	}
}

// fakeProcess is a Process a test drives; nothing is started.
type fakeProcess struct {
	stdout    io.Reader
	stderr    io.Reader
	stdin     *recordingStdin
	heldStdin *heldStdin
	exited    chan struct{}
	killed    chan struct{}
	once      sync.Once
	status    int
	waitErr   error
	killErr   error
}

func newExitedFake(status int, stdout, stderr string) *fakeProcess {
	return newFakeProcess(status, stdout, stderr, nil)
}

func newFakeProcess(status int, stdout, stderr string, waitErr error) *fakeProcess {
	p := &fakeProcess{
		stdout:  strings.NewReader(stdout),
		stderr:  strings.NewReader(stderr),
		stdin:   &recordingStdin{},
		exited:  make(chan struct{}),
		killed:  make(chan struct{}),
		waitErr: waitErr,
	}
	p.finish(status)
	return p
}

// newBlockingFake is a process that exits only when a test finishes it or the
// executor kills it, with stdout repeating output so a broken frame stream has
// something to fail on.
func newBlockingFake(stdout string) *fakeProcess {
	return &fakeProcess{
		stdout: strings.NewReader(stdout),
		stderr: strings.NewReader(""),
		stdin:  &recordingStdin{},
		exited: make(chan struct{}),
		killed: make(chan struct{}),
	}
}

// newUnreadableFake is a process whose stdout fails to read; the executor must
// kill it and report the failed read rather than the kill.
func newUnreadableFake(err error) *fakeProcess {
	return &fakeProcess{
		stdout: errorReader{err: err},
		stderr: strings.NewReader(""),
		stdin:  &recordingStdin{},
		exited: make(chan struct{}),
		killed: make(chan struct{}),
	}
}

func (p *fakeProcess) Stdin() io.WriteCloser {
	if p.heldStdin != nil {
		return p.heldStdin
	}
	return p.stdin
}
func (p *fakeProcess) Stdout() io.Reader { return p.stdout }
func (p *fakeProcess) Stderr() io.Reader { return p.stderr }

// heldStdin blocks a write until it is closed, standing in for bd not reading
// the body while a process outside its group still holds the read end.
type heldStdin struct {
	closed chan struct{}
	once   sync.Once
}

func newHeldStdin() *heldStdin { return &heldStdin{closed: make(chan struct{})} }

func (s *heldStdin) Write([]byte) (int, error) {
	<-s.closed
	return 0, os.ErrClosed
}

func (s *heldStdin) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// CloseOutputs closes the streams the fake can close; a strings.Reader cannot,
// and one that can (a held pipe) is what a test uses to block a pump.
func (p *fakeProcess) CloseOutputs() {
	for _, r := range []io.Reader{p.stdout, p.stderr} {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	}
}

func (p *fakeProcess) Wait() (int, error) {
	<-p.exited
	return p.status, p.waitErr
}

func (p *fakeProcess) Kill() error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	p.finish(-1)
	return p.killErr
}

func (p *fakeProcess) finish(status int) {
	p.once.Do(func() {
		p.status = status
		close(p.exited)
	})
}

func (p *fakeProcess) wasKilled() bool {
	select {
	case <-p.killed:
		return true
	default:
		return false
	}
}

// recordingStdin keeps what the executor fed bd and whether it closed it.
type recordingStdin struct {
	mu     sync.Mutex
	body   bytes.Buffer
	closed bool
}

func (s *recordingStdin) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(b)
}

func (s *recordingStdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *recordingStdin) contents() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String(), s.closed
}

// failingWriter is a client stream that broke.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the client's stream is gone") }

// errorReader is a request body that broke while it was read.
type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// heldReader blocks in Read until it is closed, standing in for a pipe that a
// process which outlived bd's group still holds open.
type heldReader struct {
	closed chan struct{}
	once   sync.Once
}

func newHeldReader() *heldReader { return &heldReader{closed: make(chan struct{})} }

func (r *heldReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, os.ErrClosed
}

func (r *heldReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// startRecord is one call the fake runner was handed.
type startRecord struct {
	binary string
	argv   []string
	env    []string
	dir    string
}

// fakeRunner stands in for the process runner. Its queue is consumed one
// process per Start, then an immediately successful process is returned.
type fakeRunner struct {
	mu      sync.Mutex
	queue   []Process
	calls   []startRecord
	started chan struct{}
	once    sync.Once
	// err makes Start fail, as a missing or unexecutable binary would.
	err error
}

func (r *fakeRunner) Start(binary string, argv, env []string, dir string) (Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, startRecord{binary: binary, argv: slices.Clone(argv), env: slices.Clone(env), dir: dir})
	var proc Process = newExitedFake(0, "", "")
	if len(r.queue) > 0 {
		proc, r.queue = r.queue[0], r.queue[1:]
	}
	err := r.err
	r.mu.Unlock()
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if err != nil {
		return nil, err
	}
	return proc, nil
}

func (r *fakeRunner) records() []startRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *fakeRunner) only(t *testing.T) startRecord {
	t.Helper()
	records := r.records()
	if len(records) != 1 {
		t.Fatalf("runner started %d calls, want 1", len(records))
	}
	return records[0]
}

// testWorkspace builds the workspace tree the design pushes: the town's .beads
// and one rig's.
func testWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{".beads", filepath.Join("gastown", ".beads")} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newTestExecutor(t *testing.T, root string, runner processRunner) *Executor {
	t.Helper()
	e, err := New(Options{
		Binary:           "/opt/bd/bd",
		WorkspaceRoot:    root,
		Home:             "/home/bdgw",
		Path:             "/usr/local/bin:/usr/bin:/bin",
		DoltHost:         "gt-dolt",
		DoltPort:         "3307",
		TrackerURL:       "http://127.0.0.1:7433",
		TrackerTokenFile: "/run/secrets/tracker.token",
		runner:           runner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func readFrames(t *testing.T, data []byte) []Frame {
	t.Helper()
	frames := make([]Frame, 0, 3)
	for r := bytes.NewReader(data); ; {
		f, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("ReadFrame after %d frames: %v", len(frames), err)
		}
		frames = append(frames, f)
	}
}

func streamOf(frames []Frame, kind FrameKind) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Kind == kind {
			b.Write(f.Payload)
		}
	}
	return b.String()
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
