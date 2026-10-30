package uninstallgt

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Env is the outside world one run reads or acts on.
type Env struct {
	// Getenv reads the variables that name the directory and the town.
	Getenv func(string) string
	// Files is the filesystem the run examines and removes from.
	Files Files
	// Exec runs the commands that clear an OS-immutable flag.
	Exec Exec
	// SelfDir is the running binary's own directory, the tie an install
	// outside a town has to one.
	SelfDir string
	// Signal0 probes whether a process exists, for the town daemon check.
	Signal0 func(pid int) error
}

// OSEnv is Env over the host.
func OSEnv() Env {
	env := Env{Getenv: os.Getenv, Files: OSFiles{}, Exec: OSExec, Signal0: Signal0}
	// A binary whose own path cannot be read has no directory to offer the
	// town check; $GT_TOWN_ROOT and $HOME/gt still name that town (gt-bqzoq).
	if exe, err := os.Executable(); err == nil {
		env.SelfDir = filepath.Dir(exe)
	}
	return env
}

// Run performs one uninstall and returns the run's exit code: it prints the
// directory and what named it, refuses a directory a run must not touch, and
// removes exactly <dir>/gt and <dir>/bd, leaving everything else alone.
func Run(args []string, stdout, stderr io.Writer, env Env) int {
	opts, err := Parse(args)
	if errors.Is(err, ErrHelp) {
		fmt.Fprintln(stdout, Usage)
		return ExitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, Usage)
		return ExitRefused
	}
	target, err := Resolve(opts, env.Getenv)
	if err != nil {
		return refuse(stderr, err)
	}
	home := env.Getenv("HOME")
	town := DetectTown(env.Getenv, env.SelfDir, env.Signal0)
	notice, err := town.CheckLive(target.Dir, home, opts.Force)
	if err != nil {
		return refuse(stderr, err)
	}
	for _, line := range notice {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintf(stdout, "%s: target %s (from %s)\n", Program, target.Dir, target.Source)

	info, err := env.Files.Stat(target.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stdout, "%s: nothing to do — %s does not exist\n", Program, target.Dir)
		return ExitOK
	case err != nil:
		// A directory the run cannot examine is not one it may read as
		// absent: it removes nothing and reports the failure.
		fmt.Fprintf(stderr, "%s: cannot examine %s; nothing was removed\n", Program, target.Dir)
		return ExitFailed
	case !info.IsDir():
		// Resolve refuses a file it can stat, so this path became one after
		// that check, and there is nothing to remove from it.
		fmt.Fprintf(stdout, "%s: nothing to do — %s is not a directory\n", Program, target.Dir)
		return ExitOK
	}

	removed := 0
	failed := false
	for _, name := range names {
		path := filepath.Join(target.Dir, name)
		switch err := Remove(env.Files, env.Exec, path); {
		case err == nil:
			fmt.Fprintf(stdout, "%s: removed %s\n", Program, path)
			removed++
		case errors.Is(err, ErrAbsent):
			fmt.Fprintf(stdout, "%s: not present: %s\n", Program, path)
		default:
			fmt.Fprintf(stderr, "%s: FAILED to remove %s (still present after rm -f)\n", Program, path)
			failed = true
		}
	}
	if failed {
		fmt.Fprintf(stderr, "%s: %d removed, at least one failed; nothing else was touched\n", Program, removed)
		return ExitFailed
	}
	fmt.Fprintf(stdout, "%s: done — %d removed from %s; the workspace was not touched\n",
		Program, removed, target.Dir)
	return ExitOK
}

// refuse prints a refusal and returns the exit code that carries it.
func refuse(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%s: refusing to run: %s\n", Program, err)
	return ExitRefused
}
