// Package openshellprov parses the command line of openshell-providers and
// resolves the directories a run reads (gt-tpg0p.53), and runs the import of
// the OpenShell profiles and the providers built from them.
package openshellprov

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the command's name: the binary, the prefix of every message it
// prints, and the name the script's usage line is adapted to.
const Name = "openshell-providers"

// Usage is the one-line help -h, --help and a refused argument print.
const Usage = "usage: " + Name + " [-g NAME]"

// Options is a command line the command accepts.
type Options struct {
	// Gateway is the -g name every openshell call targets; empty is the gateway
	// the CLI has as its default.
	Gateway string
	// Help says -h or --help asked for the usage line, which ends the parse
	// where it stands.
	Help bool
}

// Parse reads args, the command line after the program name. A refused
// argument is an error naming the command and the argument; the caller prints
// it and exits 2.
func Parse(args []string) (Options, error) {
	var opts Options
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			opts.Help = true
			return opts, nil
		case arg == "-g" || arg == "--gateway":
			// Whatever follows the flag is the name, another flag included, and
			// only a missing or empty one is refused — the script's `case`.
			if i+1 == len(args) || args[i+1] == "" {
				return opts, fmt.Errorf("%s: %s needs a gateway name, e.g. -g k8s", Name, arg)
			}
			opts.Gateway = args[i+1]
			i++
		case strings.HasPrefix(arg, "--gateway="):
			name := strings.TrimPrefix(arg, "--gateway=")
			if name == "" {
				return opts, fmt.Errorf("%s: --gateway needs a gateway name, e.g. -g k8s", Name)
			}
			opts.Gateway = name
		default:
			return opts, fmt.Errorf("%s: unknown argument: %s (%s)", Name, arg, Usage)
		}
	}
	return opts, nil
}

// Host is what a run reads from the process: its working directory, its
// environment, its PATH and its filesystem. Production uses ProcessHost; a test
// passes a scripted host instead of chdir-ing, setting env, changing PATH or
// building a tree on disk.
type Host struct {
	// Getwd reports the working directory the repository root is searched from.
	Getwd func() (string, error)
	// Getenv reads XDG_CONFIG_HOME and HOME.
	Getenv func(string) string
	// LookPath reports the openshell executable's path; its error means none is
	// on PATH.
	LookPath func(string) (string, error)
	// Stat reports a path's file information, so the repository-root walk runs
	// against a tree a test scripts rather than one it builds on disk.
	Stat func(string) (fs.FileInfo, error)
	// Exec runs the openshell and gt calls.
	Exec Runner
	// Files is the filesystem the profiles and key files are read from.
	Files Files
	// Env reports the environment a provider create inherits.
	Env func() []string
}

// ProcessHost reads the running process.
var ProcessHost = Host{
	Getwd:    os.Getwd,
	Getenv:   os.Getenv,
	LookPath: exec.LookPath,
	Stat:     os.Stat,
	Exec:     ExecRunner{},
	Files:    OSFiles{},
	Env:      os.Environ,
}

// Paths are the directories a run reads.
type Paths struct {
	// Root is the repository root: the nearest directory at or above the
	// working directory that holds a go.mod.
	Root string
	// Profiles is Root/deploy/polecat/profiles, the profiles a run imports.
	Profiles string
	// ConfigDir is XDG_CONFIG_HOME/gt when it is set to a non-empty value, else
	// HOME/.config/gt: where the key files live.
	ConfigDir string
}

// ResolvePaths derives the directories from h. A working directory that cannot
// be read, or that has no go.mod above it, is an error: the run has no
// profiles to import, which is not the same as an empty set.
func ResolvePaths(h Host) (Paths, error) {
	wd, err := h.Getwd()
	if err != nil {
		return Paths{}, fmt.Errorf("%s: working directory: %w", Name, err)
	}
	root, err := repoRoot(wd, h.Stat)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Root:      root,
		Profiles:  filepath.Join(root, "deploy", "polecat", "profiles"),
		ConfigDir: configDir(h.Getenv),
	}, nil
}

// repoRoot walks up from dir to the nearest directory that holds go.mod. A
// directory the walk cannot examine is an error rather than "no module here":
// otherwise a permission error on a parent would read as a checkout with no
// repository root.
func repoRoot(dir string, stat func(string) (fs.FileInfo, error)) (string, error) {
	start := dir
	for {
		goMod := filepath.Join(dir, "go.mod")
		_, err := stat(goMod)
		switch {
		case err == nil:
			return dir, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%s: reading %s: %w", Name, goMod, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s: no go.mod in %s or any parent, so the repository root cannot be found", Name, start)
		}
		dir = parent
	}
}

// configDir is XDG_CONFIG_HOME/gt when it is set to a non-empty value, else
// HOME/.config/gt: the config home DefaultForgejoTokenDir resolves for the
// forgejo-<role>.env files the forgejo-polecat provider is built from, so the
// key files and the tokens live in one directory. A GT_ override would be a
// new environment read in production code, which the config kernel refuses
// (TestNoNewGTEnvReads, gt-y3pgh.2).
func configDir(getenv func(string) string) string {
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gt")
	}
	return filepath.Join(getenv("HOME"), ".config", "gt")
}
