// Command bump-version bumps the version across the Gas Town release files: the gt
// version, the npm package, flake.nix when nix is on PATH, and CHANGELOG.md. It runs
// from the repository root. With --commit it commits the edits, with --tag it tags the
// commit, and with --install it rebuilds gt and checks the installed version. It never
// pushes. The command line and its rules are in internal/bumpversion; this file wires
// the real git, process, file and clock seams into the run.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/bumpversion"
)

func main() {
	// The working directory is the repository root the run edits. It is resolved once
	// here, as an absolute path, and every later file and process call is made under it.
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(run(os.Args[1:], systemEnv(root), os.Stderr))
}

// run is the command with its seams and error stream injected, so main_test.go can
// pin the lines and the exit code without a subprocess or the working copy.
func run(args []string, env bumpversion.Env, errw io.Writer) int {
	opts, err := bumpversion.Parse(args)
	if err == nil {
		err = bumpversion.Run(env, opts)
	}
	if err == nil {
		return 0
	}
	var usage *bumpversion.UsageError
	switch {
	case errors.Is(err, bumpversion.ErrUsage):
		fmt.Fprintf(errw, "%s", bumpversion.Usage)
		return 1
	case errors.As(err, &usage):
		fmt.Fprintln(errw, usage.Msg)
		fmt.Fprintf(errw, "%s", bumpversion.Usage)
		return 1
	}
	fmt.Fprintln(errw, err)
	return 1
}

// systemEnv is the run against the repository at root, an absolute path. Files are
// read and written under root, git runs in it, and make and gt are the real tools.
// The home directory is resolved here too, and a failure to resolve it is carried to
// the install check rather than replaced with an empty path.
func systemEnv(root string) bumpversion.Env {
	home, homeErr := os.UserHomeDir()
	return bumpversion.Env{
		FS:         diskFS{root: root},
		Git:        bumpversion.Git{Run: runGit},
		Exec:       bumpversion.Exec{Run: runProcess, LookPath: exec.LookPath, MkdirTemp: os.MkdirTemp},
		Root:       root,
		Home:       home,
		HomeErr:    homeErr,
		Date:       time.Now().Format("2006-01-02"),
		Executable: isExecutable,
		Out:        os.Stdout,
	}
}

// diskFS reads and writes files under root. Every name is joined onto root, so no file
// is found through the working directory.
type diskFS struct{ root string }

// path is the absolute location of name, a slash-separated path relative to root.
func (d diskFS) path(name string) string { return filepath.Join(d.root, name) }

func (d diskFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(d.path(name)) } //nolint:gosec // G304: the version files named by the command, under the repository root

func (d diskFS) WriteFile(name string, data []byte) error {
	return os.WriteFile(d.path(name), data, 0o644) //nolint:gosec // G306: rewrites an existing version file, so its mode is kept; the mode applies only to a file that is created
}

// newCmd builds the command a runner starts. The runners take it as a parameter, so a
// test can hand them a program that is not on PATH and see the start failure without
// starting a process.
type newCmd func(name string, args ...string) *exec.Cmd

// runGit runs git in dir and returns its exit status. git's output goes to the
// terminal, so the operator sees the commit it made.
func runGit(dir string, args ...string) (int, error) {
	return runGitWith(exec.Command, dir, args...)
}

// runGitWith is runGit with its command constructor injected. A status is not an
// error; only a git that could not start is.
func runGitWith(command newCmd, dir string, args ...string) (int, error) {
	cmd := command("git", args...) //nolint:gosec // G204: git is the fixed tool; the arguments are the commit, tag and diff steps the command builds
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// runProcess runs name in dir with env added to the environment and returns what it
// wrote to each output stream.
func runProcess(dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	return runProcessWith(exec.Command, dir, env, name, args...)
}

// runProcessWith is runProcess with its command constructor injected.
func runProcessWith(command newCmd, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := command(name, args...) //nolint:gosec // G204: name is make, go, nix or the installed gt, each fixed by the run; never user input
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// isExecutable reports whether path is a regular file with an execute bit set. The
// install check uses it to choose the gt in the home directory over the gt on PATH.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}
