package shelltier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OSExec is the real Exec. It runs c in c.Dir with os/exec, with c.Env added to
// the inherited environment, and writes the command's output to c.Log. Run
// creates no directories, so OSExec creates the log's directory and the TMPDIR
// in c.Env before it starts the command.
//
// A command that exits with a code returns that code and a nil error. A command
// that cannot start returns the error, and so does one whose context ends.
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
	err = start(ctx, c, log)
	// A log that will not close may have lost output, so its error fails a
	// command that would otherwise pass; an exit code still wins.
	if closeErr := log.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// startProcess runs c with os/exec and returns what cmd.Run returned.
func startProcess(ctx context.Context, c Command, log io.Writer) error {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout = log
	cmd.Stderr = log
	return cmd.Run()
}
