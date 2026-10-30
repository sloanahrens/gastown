package shelltier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// errNoArgv is the error for a command with no command line to run.
var errNoArgv = errors.New("command has no argv")

// OSExec is the real Exec. It runs c in c.Dir with os/exec, with c.Env added to
// the inherited environment, and writes the command's output to c.Log. Run
// creates no directories, so OSExec creates the log's directory and the TMPDIR
// in c.Env before it starts the command.
//
// A command that exits with a code returns that code and a nil error. A command
// that cannot start returns the error, and so does one whose context ends or
// that a signal ended: neither is an exit code.
func OSExec(ctx context.Context, c Command) (int, error) {
	return execWith(ctx, c, osFiles{}, startProcess)
}

// files is the file system OSExec writes to before it starts a command.
type files interface {
	MkdirAll(dir string) error
	Create(name string) (io.WriteCloser, error)
}

// osFiles is files over the real file system.
type osFiles struct{}

func (osFiles) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func (osFiles) Create(name string) (io.WriteCloser, error) { return os.Create(name) }

// starter runs c with its output written to log and returns what it returned.
type starter func(ctx context.Context, c Command, log io.Writer) error

// execWith is OSExec with its file system and process start passed in, so a
// unit test can drive each failure path without starting a process.
func execWith(ctx context.Context, c Command, fsys files, start starter) (int, error) {
	if len(c.Argv) == 0 {
		return 0, errNoArgv
	}
	if err := fsys.MkdirAll(filepath.Dir(c.Log)); err != nil {
		return 0, fmt.Errorf("create log dir: %w", err)
	}
	for _, kv := range c.Env {
		if dir, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			if err := fsys.MkdirAll(dir); err != nil {
				return 0, fmt.Errorf("create TMPDIR: %w", err)
			}
		}
	}
	log, err := fsys.Create(c.Log)
	if err != nil {
		return 0, fmt.Errorf("create log: %w", err)
	}
	script := c.Argv[len(c.Argv)-1]
	err = start(ctx, c, log)
	// A log that will not close may have lost output, so its error fails a
	// command that would otherwise pass; an exit code still wins.
	if closeErr := log.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	// A context that ended decided the result, whatever the process reported:
	// the kill is not the script's own outcome.
	if ctx.Err() != nil {
		return 0, fmt.Errorf("%s ended with its context: %w", script, ctx.Err())
	}
	var exitErr interface {
		error
		ExitCode() int
	}
	if errors.As(err, &exitErr) {
		// A signal the context did not send exits with code -1: it is not a
		// result the script reported, so it is an error.
		if exitErr.ExitCode() == -1 {
			return 0, fmt.Errorf("%s ended by a signal: %w", script, err)
		}
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
