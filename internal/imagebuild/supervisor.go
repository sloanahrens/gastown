// Package imagebuild supervises the sandbox image build that
// scripts/install-gt.sh starts (gt-k6v12): it runs the build in a process group
// of its own under an alarm, stops the whole group when the alarm goes off, and
// reports a build that did not finish.
//
// The group is the unit. A build that outlives the alarm is stopped with a
// signal to its process group, and the lock the build took is removed only once
// the group is confirmed gone: removing it while a docker the build started
// still ran would let the next install build beside it. Nothing walks a
// snapshot of pids, so a child started after any snapshot would be missed.
package imagebuild

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExitAlarm is the status Run returns when the alarm stopped the build, or
// could not confirm that it did.
const ExitAlarm = 124

// ExitStart is the status Run returns when the build could not be started.
const ExitStart = 127

const (
	// termGrace is how long the group has to leave after TERM before KILL.
	termGrace = 1 * time.Second
	// killGrace is how long the group has to be gone after KILL before the stop
	// is reported as unconfirmed.
	killGrace = 5 * time.Second
	// pollInterval is how often the group is probed while it leaves.
	pollInterval = 50 * time.Millisecond
)

// Config is one supervised build.
type Config struct {
	// Alarm is how long the build may run. It must be positive.
	Alarm time.Duration
	// Lock is the lock directory the build takes; it holds a pid file naming the
	// build. After a stop it is removed only if the pid file names the build
	// that was stopped, so another install's lock is left alone. Empty means the
	// build takes none.
	Lock string
	// GT is the gt binary that raises and clears the escalation. Empty means no
	// escalation, whatever Fingerprint says.
	GT string
	// Fingerprint, when set with GT, is the escalation the build reports under:
	// raised when the build does not finish (twice tried), cleared when it does.
	// A detached build has no caller left to report it, so the supervisor does.
	Fingerprint string
	// Command is the build's argv.
	Command []string
	// Env is the build's environment; nil means the process's.
	Env []string
	// Stdout and Stderr are the build's streams, and Stderr is where Run's own
	// messages go. Nil means the process's.
	Stdout io.Writer
	Stderr io.Writer
}

// Run supervises one build and returns the status the caller exits with: the
// build's own, 128+signal when a signal ended it, ExitAlarm when the alarm
// stopped it, or ExitStart when it could not be started.
func Run(cfg Config) int {
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	return realSystem().run(cfg)
}

// Detach makes this process safe to leave running after the install that
// started it ends: a session of its own, and none of the descriptors it
// inherited. The install lock is one of those descriptors, so a detached build
// must not keep it past the install. Only descriptors that are not
// close-on-exec are closed, which leaves the Go runtime's own alone.
func Detach() {
	_, _ = unix.Setsid() // fails only if already a group leader, which is as detached
	closeInheritable(3, 255)
}

// closeInheritable closes every descriptor from first to last that a child
// would inherit, and returns the ones it closed.
func closeInheritable(first, last int) []int {
	var closed []int
	for fd := first; fd <= last; fd++ {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if unix.Close(fd) == nil {
			closed = append(closed, fd)
		}
	}
	return closed
}

// build is one started build.
type build interface {
	// Pid is the build's pid, which is also its process group.
	Pid() int
	// Wait blocks until the build ends. It returns its exit status, or the
	// signal that ended it.
	Wait() (code int, sig syscall.Signal)
}

// system carries every effect Run has, so the unit tests can replace each.
type system struct {
	start     func(Config) (build, error)
	signal    func(pgid int, sig syscall.Signal) error
	after     func(time.Duration) <-chan time.Time
	sleep     func(time.Duration)
	readFile  func(string) ([]byte, error)
	removeAll func(string) error
	command   func(name string, args ...string) error

	termGrace, killGrace, pollInterval time.Duration
}

func realSystem() system {
	return system{
		start: startBuild,
		signal: func(pgid int, sig syscall.Signal) error {
			return syscall.Kill(-pgid, sig)
		},
		after:     time.After,
		sleep:     time.Sleep,
		readFile:  os.ReadFile,
		removeAll: os.RemoveAll,
		command: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
		termGrace:    termGrace,
		killGrace:    killGrace,
		pollInterval: pollInterval,
	}
}

type waited struct {
	code int
	sig  syscall.Signal
}

func (s system) run(cfg Config) int {
	b, err := s.start(cfg)
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "polecat image: cannot start the build: %v\n", err)
		return s.finish(cfg, ExitStart)
	}
	done := make(chan waited, 1)
	go func() {
		code, sig := b.Wait()
		done <- waited{code, sig}
	}()

	select {
	case w := <-done:
		code := w.code
		if w.sig != 0 {
			code = 128 + int(w.sig)
		}
		return s.finish(cfg, code)
	case <-s.after(cfg.Alarm):
		s.stop(cfg, b.Pid())
		return s.finish(cfg, ExitAlarm)
	}
}

// stop ends the build's process group. The lock is removed only after the group
// is gone; a group that cannot be confirmed gone keeps the lock.
func (s system) stop(cfg Config, pgid int) {
	secs := int(cfg.Alarm / time.Second)
	_ = s.signal(pgid, syscall.SIGTERM)
	gone := s.waitGone(pgid, s.termGrace)
	if !gone {
		_ = s.signal(pgid, syscall.SIGKILL)
		gone = s.waitGone(pgid, s.killGrace)
	}
	if !gone {
		fmt.Fprintf(cfg.Stderr, "polecat image: alarm after %d s could not confirm the build stopped; the lock was not removed\n", secs)
		return
	}
	s.removeLock(cfg.Lock, pgid)
	fmt.Fprintf(cfg.Stderr, "polecat image: alarm after %d s stopped the build\n", secs)
}

// waitGone probes the process group until no process is left in it, for at
// most limit. A group the supervisor may not signal (EPERM) is still there.
func (s system) waitGone(pgid int, limit time.Duration) bool {
	for waited := time.Duration(0); ; waited += s.pollInterval {
		if err := s.signal(pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if waited >= limit {
			return false
		}
		s.sleep(s.pollInterval)
	}
}

// removeLock removes the lock directory when its pid file names pid, the build
// that was stopped. Any other lock, or one without a readable pid, stays.
func (s system) removeLock(lock string, pid int) {
	if lock == "" {
		return
	}
	data, err := s.readFile(filepath.Join(lock, "pid"))
	if err != nil {
		return
	}
	if got, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || got != pid {
		return
	}
	_ = s.removeAll(lock)
}

// finish reports the build to the town when a fingerprint is set, then returns
// code: a build that did not finish escalates (tried twice), and one that did
// clears the escalation. If neither try reaches the town the build log, which
// is where this process writes, is the last record.
func (s system) finish(cfg Config, code int) int {
	if cfg.Fingerprint == "" || cfg.GT == "" {
		return code
	}
	args := []string{"escalate", "clear", "--fingerprint", cfg.Fingerprint,
		"--reason", "install-gt: a detached image build finished"}
	if code != 0 {
		args = []string{"escalate",
			fmt.Sprintf("install-gt: the detached image build did not finish (exit %d); the binary stands, and stale-image slings are refused until it is rebuilt", code),
			"-s", "medium", "--source", "script:install-gt", "--fingerprint", cfg.Fingerprint}
	}
	err := s.command(cfg.GT, args...)
	if err != nil {
		err = s.command(cfg.GT, args...)
	}
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "polecat image: the escalation for this build did not reach the town (exit %d)\n", exitCode(err))
	}
	return code
}

// exitCode is the status a failed command exited with, or -1 when it did not
// run to an exit.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// realBuild is a started build.
type realBuild struct{ cmd *exec.Cmd }

func startBuild(cfg Config) (build, error) {
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = cfg.Env
	cmd.Stdout, cmd.Stderr = cfg.Stdout, cfg.Stderr
	// A process group of its own, so one signal reaches the build and everything
	// it started, and nothing else. A nil Stdin is /dev/null.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return realBuild{cmd}, nil
}

func (b realBuild) Pid() int { return b.cmd.Process.Pid }

func (b realBuild) Wait() (int, syscall.Signal) {
	_ = b.cmd.Wait()
	st := b.cmd.ProcessState
	if st == nil {
		return 1, 0
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 0, ws.Signal()
	}
	return st.ExitCode(), 0
}
