// Package polecatimage is the Go port of the build, lock, publish and cleanup
// part of plugins/polecat-image/run.sh (gt-tpg0p.5, gt-tpg0p.6). Each method is
// one function of the script and keeps its messages; the prune port
// (gt-tpg0p.7) finishes the program that script becomes a shim of.
package polecatimage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Config is everything this part of a run reads from outside itself; the
// program's wiring fills it, from the environment for the values the script
// reads that way.
type Config struct {
	// GT, Docker, Git, Make, Pgrep and Openshell are the programs a run calls.
	GT, Docker, Git, Make, Pgrep, Openshell string
	// RigRepo is the bare repository a build makes its worktree from, BuildRoot
	// holds the worktrees it makes there, and BDSrc is the beads checkout the
	// image's bd is built from.
	RigRepo, BuildRoot, BDSrc string
	// LockDir is the lock one run holds the image under.
	LockDir string
	// StaleAfter is how long the lock may sit unmodified before the next run
	// takes it over.
	StaleAfter time.Duration
	// SandboxRecords is the gateway's per-sandbox directory: a listed sandbox
	// without a record there defers the run.
	SandboxRecords string
	// Image is the tag a build updates, and Label is the label inside it that
	// records the gt commit.
	Image, Label string
	// Registry is the host:port image pushes go to; empty publishes nothing.
	Registry string
	// Published records the "<image id> <registry>" a complete push left, so
	// the next run retries one that did not finish.
	Published string
	// Crane tags and pushes the image's later tags on the registry.
	Crane string
	// FailClosed makes a run that leaves the image unbuilt or unpublished fail
	// instead of deferring (POLECAT_IMAGE_FAIL_CLOSED=1).
	FailClosed bool
	// PID is the pid a claim records and Cleanup compares; 0 means this
	// process's.
	PID int
}

// Run is one plugin run: the configuration, the state Cleanup needs, and the
// seams a test answers with.
type Run struct {
	Cfg Config
	// Worktree is the detached worktree the current build made; Cleanup removes
	// it. Empty when there is none.
	Worktree string
	// TarDir is the publish step's scratch directory, which PushToRegistry sets
	// and Cleanup removes.
	TarDir string
	// FirstPushed is the reference the registry accepted first, which is what
	// tells a later failure it left the image only partly published.
	FirstPushed string
	// HaveLock records that this run took the lock, so Cleanup may release it.
	HaveLock bool

	// run is the program runner; nil means execRun.
	run runnerFunc
	// now is the clock; nil means time.Now.
	now func() time.Time
	// out is where log lines go; nil means os.Stdout.
	out io.Writer
	// rename moves the lock aside during a takeover; nil means os.Rename.
	rename func(oldpath, newpath string) error
	// claim makes the lock directory; nil means ClaimLock.
	claim func() error
	// writePID records the run's pid in the lock directory; nil means
	// os.WriteFile.
	writePID func(path string, data []byte) error
	// unlock holds the flock a run that took the lock keeps until Cleanup;
	// nil while the run does not hold the lock.
	unlock func()
	// look finds a program on PATH; nil means exec.LookPath.
	look func(name string) (string, error)
}

// command is one program invocation: its working directory, program and
// arguments.
type command struct {
	dir  string
	name string
	args []string
}

// result is one program's exit: what it wrote to each stream and its status.
type result struct {
	stdout string
	stderr string
	exit   int
}

// output is the program's stdout alone, the calls the script sent 2>/dev/null.
func (r result) output() string { return r.stdout }

// combined is both streams, the script's 2>&1; the streams are captured apart,
// so stderr reports after stdout rather than interleaved with it.
func (r result) combined() string { return r.stdout + r.stderr }

func (r result) failed() bool { return r.exit != 0 }

// runnerFunc runs one command and answers with its output and status.
type runnerFunc func(command) result

// errFatal is a failure this run has already logged; the caller exits non-zero.
type errFatal struct{ msg string }

func (e *errFatal) Error() string { return e.msg }

func (r *Run) runner() runnerFunc {
	if r.run != nil {
		return r.run
	}
	return execRun
}

func (r *Run) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Run) writer() io.Writer {
	if r.out != nil {
		return r.out
	}
	return os.Stdout
}

func (r *Run) renamer() func(string, string) error {
	if r.rename != nil {
		return r.rename
	}
	return os.Rename
}

func (r *Run) claimer() func() error {
	if r.claim != nil {
		return r.claim
	}
	return r.ClaimLock
}

func (r *Run) pidWriter() func(string, []byte) error {
	if r.writePID != nil {
		return r.writePID
	}
	return func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
}

func (r *Run) looker() func(string) (string, error) {
	if r.look != nil {
		return r.look
	}
	return exec.LookPath
}

func (r *Run) pid() int {
	if r.Cfg.PID != 0 {
		return r.Cfg.PID
	}
	return os.Getpid()
}

// Logf writes one log line the way the script's log does, stamped with the
// clock's time of day.
func (r *Run) Logf(format string, args ...any) {
	fmt.Fprintf(r.writer(), "[polecat-image] %s %s\n", r.clock().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// failf logs the script's die() line and returns the failure behind it.
func (r *Run) failf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	r.Logf("ERROR: %s", msg)
	return &errFatal{msg: msg}
}

// execRun is the real runner: it starts the program with both streams captured
// and reports 127 when the program gave no exit status of its own — it could not
// start, or a signal killed it — so every way a probe fails to answer is
// nonzero and defers rather than reading as "nothing there".
func execRun(c command) result {
	cmd := exec.Command(c.name, c.args...)
	cmd.Dir = c.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		res.exit = exitErr.ExitCode()
		return res
	}
	res.exit = 127
	return res
}

// isFile reports a regular file, following symlinks: the script's [ -f ].
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// isDir reports a directory: the script's [ -d ].
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// exists reports any file or directory: the script's [ -e ].
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (r *Run) call(name string, args ...string) result {
	return r.runner()(command{name: name, args: args})
}

func (r *Run) in(dir, name string, args ...string) result {
	return r.runner()(command{dir: dir, name: name, args: args})
}
