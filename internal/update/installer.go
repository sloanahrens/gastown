package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/installer"
)

const (
	// installerResultPrefix begins the one machine-readable line a run ends
	// with.
	installerResultPrefix = "install-gt: RESULT "

	// installerSource is the --source this wrapper passes, so a receipt or an
	// escalation names the command that asked for the install.
	installerSource = "gt-update"

	// installerLockWaitSeconds bounds the installer's wait for the install
	// lock. A minute, as the daemon gives it: a longer wait keeps this step
	// running after the lock's holder has plainly not finished.
	installerLockWaitSeconds = 60

	// installerOutputTail is how much of a failed command's output an error
	// carries. A build log is unbounded and unread; what a failure is read
	// back from is its end.
	installerOutputTail = 20
)

// installerEvents is every event the installer names in internal/installer,
// with how a run that exited 0 carrying it stands. The exit is read first: the
// installer names a busy lock or a busy gate slot "refused" too, and defers
// those with exit 3, so an event alone cannot classify a run.
var installerEvents = map[string]StepResult{
	"installed": ResultOK,
	"noop":      ResultOK,
	"failed":    ResultFailed,
	"refused":   ResultFailed,
}

// InstallResult is the installer's verdict for one run plus the status the
// program exited with.
type InstallResult struct {
	Event  string
	Commit string
	Prev   string
	Reason string
	Exit   int
}

// RunFunc runs one command and reports its combined output and exit status.
// err means the command could not be run to a status; a nonzero status is
// exit, not err. A nil env inherits the process environment.
type RunFunc func(ctx context.Context, dir string, env []string, name string, args ...string) (output string, exit int, err error)

// Installer installs sha, the build the run set out to put in force.
type Installer interface {
	Install(ctx context.Context, sha string) (InstallResult, error)
}

// ExecInstaller builds and runs the real installer, the way the daemon's
// rebuild_gt job does. It runs commands only through Run.
type ExecInstaller struct {
	// RepoRoot is the checkout to build the installer from and run it in.
	RepoRoot string
	// Run runs commands; nil runs them for real through ExecRun.
	Run RunFunc
	// ExtraEnv is appended to the installer's environment after the wrapper's
	// own entries, so it can override one of them. B10 supplies the
	// polecat-image trim context the daemon passes.
	ExtraEnv []string
}

// ExecRun is the production RunFunc: it runs name with args in dir and reports
// the combined output and exit status. A non-nil env is the complete
// environment; a nil env inherits the process environment.
var ExecRun RunFunc = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var exitErr *exec.ExitError
	// A nonzero exit is a status; a run killed with its context, or one that
	// could not start, reached none and is an error.
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), -1, err
}

// Install builds the installer from RepoRoot, runs it for sha, and returns its
// verdict. A run that names a RESULT line always returns that verdict, even
// when RunFunc also reports an error; a run that names none is an error.
func (e *ExecInstaller) Install(ctx context.Context, sha string) (InstallResult, error) {
	run := e.Run
	if run == nil {
		run = ExecRun
	}

	// The build inherits the environment: it is `make`, not the installer, and
	// the daemon lets it inherit.
	out, exit, err := run(ctx, e.RepoRoot, nil, "make", "gt-installer")
	if err != nil {
		return InstallResult{}, fmt.Errorf("run make gt-installer in %s: %w%s", e.RepoRoot, err, outputTail(out))
	}
	if exit != 0 {
		return InstallResult{}, fmt.Errorf("make gt-installer in %s exited %d%s", e.RepoRoot, exit, outputTail(out))
	}

	program := filepath.Join(e.RepoRoot, "gt-installer")
	// The installer starts with the process environment, so it keeps PATH and
	// HOME; a bare replacement would leave it unable to find make or a home
	// directory.
	env := append(os.Environ(),
		fmt.Sprintf("INSTALL_GT_LOCK_WAIT=%d", installerLockWaitSeconds),
		"INSTALL_GT_RIG_DIR="+e.RepoRoot,
	)
	env = append(env, e.ExtraEnv...)

	out, exit, err = run(ctx, e.RepoRoot, env, program, "--sha", sha, "--source", installerSource)
	res, ok := ParseInstallerResult(out)
	if !ok {
		if err != nil {
			return InstallResult{}, fmt.Errorf("run %s: %w%s", program, err, outputTail(out))
		}
		return InstallResult{}, fmt.Errorf("%s named no result line (exit %d)%s", program, exit, outputTail(out))
	}
	res.Exit = exit
	return res, nil
}

// ParseInstallerResult reads the last RESULT line in output. ok is false when
// output holds none, or when the line's event is absent: a verdict this
// wrapper cannot classify is never read as a result.
func ParseInstallerResult(output string) (InstallResult, bool) {
	var res InstallResult
	ok := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		rest, found := strings.CutPrefix(line, installerResultPrefix)
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 4 || fields[0] == "-" {
			// The last RESULT line wins even when it is the unreadable one.
			res, ok = InstallResult{}, false
			continue
		}
		res = InstallResult{
			Event:  emptyIfDash(fields[0]),
			Commit: emptyIfDash(fields[1]),
			Prev:   emptyIfDash(fields[2]),
			Reason: emptyIfDash(strings.Join(fields[3:], " ")),
		}
		ok = true
	}
	return res, ok
}

// ClassifyInstall is where one installer verdict leaves the install step. A
// busy wait is deferred; a success that names a failing event, an event this
// wrapper does not know, or any other status is failed.
func ClassifyInstall(r InstallResult) StepResult {
	switch r.Exit {
	case int(installer.ExitBusy):
		// A busy lock or gate slot is retried, whatever the event: the
		// installer names both "refused".
		return ResultDeferred
	case int(installer.ExitOK):
		if result, known := installerEvents[r.Event]; known {
			return result
		}
		// An exit 0 whose event is not a success this wrapper knows is not
		// read as one (fail closed).
		return ResultFailed
	default:
		return ResultFailed
	}
}

// emptyIfDash reads the installer's absent-field marker as an empty string.
func emptyIfDash(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

// outputTail renders the last installerOutputTail lines of a command's output
// for an error, or "" when the command printed nothing.
func outputTail(out string) string {
	if strings.TrimSpace(out) == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > installerOutputTail {
		lines = lines[len(lines)-installerOutputTail:]
	}
	return fmt.Sprintf("\nlast %d lines of output:\n%s", len(lines), strings.Join(lines, "\n"))
}
