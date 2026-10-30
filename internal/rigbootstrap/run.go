package rigbootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Exec runs name with args in dir, sending its output to stdout and stderr, and
// returns its exit code. The error is set only when the program could not start:
// a non-zero exit comes back as the code with a nil error.
type Exec func(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) (int, error)

// Files is the filesystem a run examines and writes.
type Files interface {
	Stat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	MkdirAll(path string, perm fs.FileMode) error
	WriteFile(name string, data []byte, perm fs.FileMode) error
}

// Deps is what a run reaches outside itself: the commands it runs, the
// filesystem it reads and writes, and the two output streams.
type Deps struct {
	Exec   Exec
	Files  Files
	Stdout io.Writer
	Stderr io.Writer
}

// OSFiles is Files on the host's filesystem.
type OSFiles struct{}

// Stat reports the host file info for name, as os.Stat does.
func (OSFiles) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// ReadFile returns the host file's contents, as os.ReadFile does.
func (OSFiles) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// MkdirAll creates path and any missing parents, as os.MkdirAll does.
func (OSFiles) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// WriteFile writes data to the host file name, as os.WriteFile does.
func (OSFiles) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// Run creates the rig named by opts and returns the command's exit code. It
// refuses, before gt runs, a town root or local repo that is not there, a local
// repo that is not a git repository, and a rig name already taken. Then it runs
// gt rig add in the town root, and when opts.PolecatAgent is set it records that
// agent in the rig settings. Refusals print their message to stderr and exit 1.
// A git that cannot start exits 127: that says nothing about the repo, so it is
// not reported as a non-repository.
func Run(ctx context.Context, opts Options, deps Deps) int {
	if !isDir(deps.Files, opts.TownRoot) {
		return refuse(deps.Stderr, "Town root does not exist: "+opts.TownRoot)
	}
	if !isDir(deps.Files, opts.LocalRepo) {
		return refuse(deps.Stderr, "Local repo does not exist: "+opts.LocalRepo)
	}
	isRepo, err := isGitRepo(ctx, deps.Exec, opts.LocalRepo)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(deps.Stderr, err)
			return 1
		}
		fmt.Fprintf(deps.Stderr, "Could not run git: %v\n", err)
		return 127
	}
	if !isRepo {
		return refuse(deps.Stderr, "Local repo is not a git repository: "+opts.LocalRepo)
	}
	remote := opts.Remote
	if remote == "" {
		remote = "file://" + opts.LocalRepo
	}
	// Built once, so the refusal message and the settings write name the same path.
	rigPath := filepath.Join(opts.TownRoot, opts.Rig)
	if _, err := deps.Files.Stat(rigPath); err == nil {
		return refuse(deps.Stderr, "Target rig already exists: "+rigPath)
	}

	args := []string{"rig", "add", opts.Rig, remote, "--local-repo", opts.LocalRepo}
	if opts.Prefix != "" {
		args = append(args, "--prefix", opts.Prefix)
	}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}
	code, err := deps.Exec(ctx, opts.TownRoot, opts.GTBin, args, deps.Stdout, deps.Stderr)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "Could not run %s: %v\n", opts.GTBin, err)
		return 127
	}
	if code != 0 {
		return code
	}

	if opts.PolecatAgent != "" {
		if err := writeSettings(deps.Files, rigPath, opts.PolecatAgent); err != nil {
			fmt.Fprintf(deps.Stderr, "Rig exists at %s, but its settings were not written: %v\n", rigPath, err)
			return 1
		}
	}
	fmt.Fprintf(deps.Stdout, "Rig bootstrapped at %s\n", rigPath)
	return 0
}

// writeSettings records polecat as role_agents.polecat in the rig settings,
// keeping whatever the file already holds. A missing file starts empty.
func writeSettings(files Files, rigPath, polecat string) error {
	settingsPath := filepath.Join(rigPath, SettingsRelPath)
	if err := files.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	existing, err := files.ReadFile(settingsPath)
	if errors.Is(err, fs.ErrNotExist) {
		existing = nil
	} else if err != nil {
		return err
	}
	merged, err := MergeRoleAgents(existing, polecat)
	if err != nil {
		return err
	}
	// The mode applies only when the file is created: WriteFile keeps an existing
	// settings file's mode, so a permission the operator set survives the merge.
	return files.WriteFile(settingsPath, merged, 0o644)
}

// isDir reports whether name is a directory, as the script's -d test did.
func isDir(files Files, name string) bool {
	info, err := files.Stat(name)
	return err == nil && info.IsDir()
}

// isGitRepo is the gate for a local repo: a rig is not made from a directory git
// does not accept as a repository. An error comes back only when git could not
// start or the context ended. A context that ended is an interrupted run, not a
// start failure, so it is wrapped as "interrupted: ..." for the caller to report.
// Neither error is an answer about the repo, so the caller must not read it as one.
func isGitRepo(ctx context.Context, execFn Exec, repo string) (bool, error) {
	code, err := execFn(ctx, "", "git", []string{"-C", repo, "rev-parse", "--git-dir"}, io.Discard, io.Discard)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("interrupted: %w", ctxErr)
		}
		return false, err
	}
	return code == 0, nil
}

// refuse is the exit path of every refusal: the message on stderr and exit 1, as
// the script exited.
func refuse(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, msg)
	return 1
}
