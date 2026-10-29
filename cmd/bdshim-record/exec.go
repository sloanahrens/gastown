package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// runLimit bounds one bd run, so a hung bd fails the recording instead of stalling it.
const runLimit = time.Minute

// execBD returns a Runner that runs the bd binary bin. The bd target variables are stripped from
// the environment, so bd selects the database in the directory it runs in, not the operator's town.
func execBD(bin string) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd binary the operator names; args are the table's fixed rows
		c.Dir = dir
		c.Env = beads.StripBDTargetEnv(os.Environ())
		c.WaitDelay = time.Second
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return interpretRun(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

// interpretRun turns how a bd run ended into a golden's stdout and exit code. A non-zero exit is a
// result; a run that timed out or was killed, or that never started, is an error, so its partial
// output is never recorded as bd's answer.
func interpretRun(stdout []byte, runErr error, timedOut bool) ([]byte, int, error) {
	if timedOut {
		return nil, 0, fmt.Errorf("bd ran past %s", runLimit)
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if ee.ExitCode() < 0 {
			return nil, 0, errors.New("bd was killed by a signal")
		}
		return stdout, ee.ExitCode(), nil
	}
	if runErr != nil {
		return nil, 0, runErr
	}
	return stdout, 0, nil
}

// copyFixture returns a Copier that makes a fresh directory under scratch, which must exist, and
// copies the fixture tree into it. The function it returns removes the copy.
func copyFixture(src, scratch string) Copier {
	return func() (string, func() error, error) {
		dir, err := os.MkdirTemp(scratch, "fixture-")
		if err != nil {
			return "", nil, err
		}
		remove := func() error { return os.RemoveAll(dir) }
		if err := copyTree(src, dir); err != nil {
			return "", nil, errors.Join(err, remove())
		}
		return dir, remove, nil
	}
}

// copyTree copies the directories and regular files under src into dst. Any other kind of entry is
// refused: a symlink could point the copy back at the live town.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src))
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return fmt.Errorf("fixture entry %s is neither a directory nor a regular file", path)
		}
	})
}

// copyFile copies one regular file to dst, which must not exist yet.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is inside the fixture the operator names
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: dst is inside the scratch copy this package made
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}
