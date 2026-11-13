package bdgateway

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/steveyegge/gastown/internal/util"
)

// Process is one started bd process as the executor drives it: stdin to feed,
// streams to frame into stdout and stderr frames, a wait for its status, and a
// kill that reaches the whole process group.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait returns the exit status, or -1 for a process that ended on a signal.
	// A nonzero status is bd's answer, not an error; err is the wait failing.
	Wait() (int, error)
	// Kill terminates the process group, so a bd that started a child leaves no
	// orphan when the call ends early.
	Kill() error
}

// processRunner starts one process. It is the seam the executor's tests put a
// fake behind; executor_real_test.go points the real one at a helper binary.
type processRunner interface {
	Start(binary string, argv, env []string, dir string) (Process, error)
}

// realRunner runs bd with an argv vector, never a shell, in its own process
// group.
type realRunner struct{}

// Start starts binary with pipes the executor owns. The parent's copies of the
// child's ends are closed once the child holds them, so a pump's read ends at
// bd's exit (and at the group's death when the executor kills it) without the
// executor having to close anything to signal EOF.
func (realRunner) Start(binary string, argv, env []string, dir string) (Process, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closePipe(stdinR, stdinW)
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closePipe(stdinR, stdinW)
		closePipe(stdoutR, stdoutW)
		return nil, err
	}
	cmd := exec.Command(binary, argv...)
	// The executor owns cancellation: it kills the group itself and classifies
	// the answer, so the command needs the group but no context hook.
	util.SetDetachedProcessGroup(cmd)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		closePipe(stdinR, stdinW)
		closePipe(stdoutR, stdoutW)
		closePipe(stderrR, stderrW)
		return nil, err
	}
	// The child holds a duplicate of each end; the parent's copies would keep a
	// pump from seeing EOF.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	return &realProcess{cmd: cmd, stdin: stdinW, stdout: stdoutR, stderr: stderrR}, nil
}

func closePipe(r, w *os.File) {
	_ = r.Close()
	_ = w.Close()
}

// realProcess is a started command. done and mu keep a Kill that races a
// finished Wait from signaling a process group id the kernel may have reused.
type realProcess struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	done   bool
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

func (p *realProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *realProcess) Stdout() io.Reader     { return p.stdout }
func (p *realProcess) Stderr() io.Reader     { return p.stderr }

func (p *realProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	_ = p.stderr.Close()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, err
}

func (p *realProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return os.ErrProcessDone
	}
	return util.KillProcessGroup(p.cmd)
}
