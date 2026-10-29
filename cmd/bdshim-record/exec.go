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
		dir, err := os.MkdirTemp(scratch, "fixture-")
		if err != nil {
			return "", nil, err
		}
		remove := func() error { return os.RemoveAll(dir) }
		if err := copyTree(src, dir); err != nil {
			return "", nil, errors.Join(err, remove())
		}
		if err := checkCopyHoldsDatabase(src, dir); err != nil {
			return "", nil, errors.Join(err, remove())
		}
		return dir, remove, nil
	}
}

// fixtureDatabase is the directory under the fixture root that bd reads its database from.
const fixtureDatabase = ".beads"

// checkCopyHoldsDatabase refuses a copy whose root lacks the fixture's database directory. bd picks
// a database by walking up from the directory it runs in, so a copy without one would answer from a
// database above it instead of the copy's own (gt-p85pw.87). A fixture with no database of its own
// is left for the preflight to refuse, since bd then answers from whatever it finds above.
func checkCopyHoldsDatabase(fixture, copy string) error {
	if _, err := os.Stat(filepath.Join(fixture, fixtureDatabase)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat fixture database %s: %w", fixture, err)
	}
	if _, err := os.Stat(filepath.Join(copy, fixtureDatabase)); err != nil {
		return fmt.Errorf("copy %s does not hold %s, so bd in it would answer from a database above it: %w",
			copy, fixtureDatabase, err)
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

// copyTree copies the directories and regular files under src into dst. Each entry lands at the path
// filepath.Rel names, so the fixture keeps its layout when the operator names it relatively (".",
// "./x", "x/.") instead of as an absolute path. Any other kind of entry is refused: a symlink could
// point the copy back at the live town.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("fixture entry %s is outside the fixture %s", path, src)
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
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
