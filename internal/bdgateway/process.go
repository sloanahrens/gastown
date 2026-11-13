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
	// Pid is the process's own id, named when the kill could not end it.
	Pid() int
	// CloseOutputs closes the ends this side reads bd's output from. A process
	// that outlived bd's group can still hold the pipes open, so a read would
	// otherwise block past the call's deadline; the executor closes its ends
	// when it stops waiting.
	CloseOutputs()
}

// processRunner starts one process. It is the seam the executor's tests put a
// fake behind; executor_real_test.go replaces it with the real one that runs
// this package's test binary as bd.
type processRunner interface {
	Start(binary string, argv, env []string, dir string) (Process, error)
}

// realRunner runs bd with an argv vector, never a shell, in its own process
// group. pipe makes one pipe pair; nil means os.Pipe, and a test replaces it to
// drive the start's failure path without exhausting the process's files.
type realRunner struct {
	pipe func() (*os.File, *os.File, error)
}

// Start starts binary with pipes the executor owns. The parent's copies of the
// child's ends are closed once the child holds them, so a pump's read ends at
// bd's exit (and at the group's death when the executor kills it) without the
// executor having to close anything to signal EOF.
func (r realRunner) Start(binary string, argv, env []string, dir string) (Process, error) {
	pipe := r.pipe
	if pipe == nil {
		pipe = os.Pipe
	}
	// One pair for each of bd's standard streams; the loop gives the three
	// creations one failure path, which a test drives through the pipe seam.
	var ends [6]*os.File
	for i := 0; i < 3; i++ {
		cr, cw, err := pipe()
		if err != nil {
			closePipeEnds(ends[:])
			return nil, err
		}
		ends[2*i], ends[2*i+1] = cr, cw
	}
	stdinR, stdinW := ends[0], ends[1]
	stdoutR, stdoutW := ends[2], ends[3]
	stderrR, stderrW := ends[4], ends[5]

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
		closePipeEnds(ends[:])
		return nil, err
	}
	// The child holds a duplicate of each end; the parent's copies would keep a
	// pump from seeing EOF.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	return &realProcess{cmd: cmd, stdin: stdinW, stdout: stdoutR, stderr: stderrR}, nil
}

// closePipeEnds closes every end a failed start opened, ignoring the close
// errors: the start's own error is what the caller acts on.
func closePipeEnds(ends []*os.File) {
	for _, f := range ends {
		if f != nil {
			_ = f.Close()
		}
	}
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
func (p *realProcess) Pid() int              { return p.cmd.Process.Pid }

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

// CloseOutputs closes the pipes bd's stdout and stderr are read from. Closing
// a descriptor that is already gone is not a failure: Wait closes both ends as
// well, and the call is ending either way, so there is nothing for the caller
// to act on.
func (p *realProcess) CloseOutputs() {
	_ = p.stdout.Close()
	_ = p.stderr.Close()
}
