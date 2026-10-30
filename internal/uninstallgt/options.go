// Package uninstallgt removes exactly the installed gt and bd binaries from a
// directory it names explicitly, and refuses a directory a run must not touch.
//
// The directory is named explicitly — --install-dir, $INSTALL_GT_BIN_DIR,
// $INSTALL_DIR or the default $HOME/.local/bin — and never looked up on PATH:
// `which` answers what runs when gt is typed, and inside a town that is the
// live install whose removal took every agent's bd down (gt-acdfp).
package uninstallgt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Program is the command's name, the prefix of every line it prints.
const Program = "uninstall-gt"

// Usage is the help text: printed to stdout for -h and to stderr after a usage
// error.
const Usage = "usage: uninstall-gt [--install-dir DIR] [--force]"

// Exit codes, the gt installer's (internal/installer): ExitRefused says the run
// was asked to touch a directory it will not, and ExitFailed says a removal it
// did attempt failed.
const (
	ExitOK      = 0
	ExitFailed  = 1
	ExitRefused = 2
)

// ErrHelp reports that -h or --help was given: the command prints Usage and
// exits 0.
var ErrHelp = errors.New("help requested")

// Refusal is a directory a run will not touch. The command prints
// "uninstall-gt: refusing to run: <reason>" and exits 2.
type Refusal struct{ Reason string }

// Error is the reason, without the command's prefix.
func (r *Refusal) Error() string { return r.Reason }

// Options is one run's command line.
type Options struct {
	// InstallDir is the directory --install-dir named, which Parse sets; empty
	// means the flag named none, so Resolve reads the environment instead.
	InstallDir string
	// Force is --force: remove the live install of the town this clone belongs
	// to anyway.
	Force bool
}

// Parse reads the arguments a run accepts. A bad argument returns an error
// naming it, and -h or --help returns ErrHelp.
func Parse(args []string) (Options, error) {
	var opts Options
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--install-dir":
			if i+1 == len(args) {
				return Options{}, errors.New(Program + ": --install-dir needs a directory")
			}
			i++
			opts.InstallDir = args[i]
		case strings.HasPrefix(arg, "--install-dir="):
			opts.InstallDir = strings.TrimPrefix(arg, "--install-dir=")
		case arg == "--force":
			opts.Force = true
		case arg == "-h" || arg == "--help":
			return Options{}, ErrHelp
		default:
			return Options{}, fmt.Errorf("%s: unknown argument: %s", Program, arg)
		}
	}
	return opts, nil
}

// Target is the directory a run removes from and what named it.
type Target struct {
	// Dir is the directory in filepath.Clean form: the spelling a run prints
	// and removes from.
	Dir string
	// Source is what named Dir, one of the Source constants. A run prints it,
	// so the directory is never implicit.
	Source string
}

// The sources a Target records, in the order Resolve tries them.
const (
	SourceFlag            = "--install-dir"
	SourceInstallGTBinDir = "$INSTALL_GT_BIN_DIR"
	SourceInstallDir      = "$INSTALL_DIR"
	SourceDefault         = "the default (HOME/.local/bin)"
)

// Resolve returns the directory a run removes from and the source that named
// it: --install-dir, else $INSTALL_GT_BIN_DIR, else $INSTALL_DIR, else
// $HOME/.local/bin. A directory a run must not touch returns a *Refusal.
//
// The refusals compare canonical paths, not the spellings: "." and ".." inside
// a path, a trailing slash and a symlink all name the same directory, and a
// spelling that differs from the root or from HOME must be refused just the
// same.
//
// $INSTALL_GT_BIN_DIR comes first because it is the gt installer's own name for
// the directory, while $INSTALL_DIR is the Makefile's and the name a test or a
// host-safety prompt sets to keep a run off the live install (gt-8xqic). PATH
// is never read, so no run can take its target from what gt resolves to.
func Resolve(opts Options, getenv func(string) string) (Target, error) {
	home := getenv("HOME")
	if home == "" {
		return Target{}, &Refusal{Reason: "HOME is not set; name the directory with --install-dir"}
	}
	dir, source := opts.InstallDir, SourceFlag
	if dir == "" {
		binDir := getenv("INSTALL_GT_BIN_DIR")
		installDir := getenv("INSTALL_DIR")
		switch {
		case binDir != "":
			dir, source = binDir, SourceInstallGTBinDir
		case installDir != "":
			dir, source = installDir, SourceInstallDir
		default:
			dir, source = filepath.Join(home, ".local", "bin"), SourceDefault
		}
	}
	// The script names --install-dir in this message whichever source gave the
	// path, and the value is the one before any cleaning.
	if !filepath.IsAbs(dir) {
		return Target{}, &Refusal{Reason: "--install-dir must be an absolute path: '" + dir + "'"}
	}
	dir = filepath.Clean(dir)
	canonical := canonicalPath(dir)
	if canonical == "/" {
		return Target{}, &Refusal{Reason: "the filesystem root is not an install directory"}
	}
	canonicalHome := filepath.Clean(home)
	if filepath.IsAbs(canonicalHome) {
		canonicalHome = canonicalPath(canonicalHome)
	}
	if canonical == canonicalHome {
		return Target{}, &Refusal{Reason: "the home directory itself is not an install directory"}
	}
	// A path that is absent is not refused: the run reports it has nothing to
	// do. A stat that fails for another reason (a parent that is a file) reads
	// the same way, as the script's -e test did.
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return Target{}, &Refusal{Reason: "--install-dir is not a directory: " + dir}
	}
	return Target{Dir: dir, Source: source}, nil
}

// canonicalPath returns the path a run compares: dir with its symlinks
// resolved where it exists, and its cleaned form where it does not. Resolving
// only an existing path keeps a target that is not there yet resolvable, and
// catches a symlinked alias of the root or of HOME that would otherwise slip
// past a refusal.
func canonicalPath(dir string) string {
	clean := filepath.Clean(dir)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return resolved
}
