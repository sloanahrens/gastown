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
// output is never recorded as bd's answer. Any error carrying an exit code counts as bd's exit, as
// *exec.ExitError does in production and as a test's fake error does (gt-p85pw.87).
func interpretRun(stdout []byte, runErr error, timedOut bool) ([]byte, int, error) {
	if timedOut {
		return nil, 0, fmt.Errorf("bd ran past %s", runLimit)
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(runErr, &exitErr) {
		if exitErr.ExitCode() < 0 {
			return nil, 0, errors.New("bd was killed by a signal")
		}
		return stdout, exitErr.ExitCode(), nil
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
		if err := refuseScratchInsideFixture(src, scratch); err != nil {
			return "", nil, err
		}
		if err := refuseFixtureWithoutDatabase(src); err != nil {
			return "", nil, err
		}
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

// fixtureDatabase is the directory under the fixture root that bd reads its database from.
const fixtureDatabase = ".beads"

// refuseFixtureWithoutDatabase refuses a fixture that holds no database of its own. bd picks a
// database by walking up from the directory it runs in, so a copy of such a fixture would answer
// from a database above it, and the recording would capture that database's state as bd's answer
// (gt-p85pw.87). The refusal comes before anything is copied.
func refuseFixtureWithoutDatabase(fixture string) error {
	if _, err := os.Stat(filepath.Join(fixture, fixtureDatabase)); err != nil {
		return fmt.Errorf("fixture %s holds no %s of its own, so bd in a copy of it would read a database above the copy: %w",
			fixture, fixtureDatabase, err)
	}
	return nil
}

// refuseScratchInsideFixture refuses a scratch directory at or under the fixture: a copy made there
// sits inside the tree the next copy walks, so that walk copies the copy into itself and never ends.
// The refusal comes before anything is created, so a refused run leaves the fixture as it was.
func refuseScratchInsideFixture(src, scratch string) error {
	inside, err := scratchInsideFixture(src, scratch)
	if err != nil {
		return err
	}
	if inside {
		return fmt.Errorf("scratch directory %s is inside the fixture %s", scratch, src)
	}
	return nil
}

// scratchInsideFixture reports whether scratch is at or under the fixture. It compares the
// directories the paths name by file identity, so a relative path, an absolute one and a symlinked
// one all compare, and it walks up from the deepest part of scratch that exists, so it answers
// before scratch is made.
func scratchInsideFixture(fixture, scratch string) (bool, error) {
	fixtureInfo, err := os.Stat(fixture)
	if err != nil {
		return false, fmt.Errorf("stat fixture %s: %w", fixture, err)
	}
	for dir := scratch; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil && os.SameFile(fixtureInfo, info) {
			return true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("stat scratch directory %s: %w", dir, err)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false, nil
		}
	}
}

// copyTree copies the directories and regular files under src into dst, each under the name it has
// inside the fixture. The walk runs over os.DirFS, which reports those names relative to the
// fixture root, so the fixture's path is never part of them (gt-p85pw.87); writes go through an
// os.Root on dst, so no entry reaches outside the copy. Any other kind of entry is refused: a
// symlink could point the copy back at the live town.
func copyTree(src, dst string) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the copy is written; a close failure leaves nothing to report that a write would not
	fixture := os.DirFS(src)
	return fs.WalkDir(fixture, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil // the fixture's root is the copy's root
		}
		switch {
		case d.IsDir():
			return root.MkdirAll(name, 0o755)
		case d.Type().IsRegular():
			return copyFile(root, fixture, name)
		default:
			return fmt.Errorf("fixture entry %s is neither a directory nor a regular file", name)
		}
	})
}

// copyFile copies one regular file to name in the copy, which must not exist yet.
func copyFile(root *os.Root, fixture fs.FS, name string) error {
	in, err := fixture.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // a read of the fixture: a close failure cannot lose a copy
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}
