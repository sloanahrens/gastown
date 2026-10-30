package tiersweep

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usage is the line a command line this package cannot read prints.
const usage = "usage: tier-sweep [--root dir] [shell] [integration] [race]"

// DefaultRoot is the repository root a run works from when --root names none:
// the working directory. The script changed to its own parent directory; the
// command takes the root from a flag instead.
const DefaultRoot = "."

// Deps are the seams one run reads through. A test fills every field with a
// fake, so no run starts a process, reads the clock or touches a repository.
type Deps struct {
	// Host reads the environment and makes the log directory.
	Host Host
	// Exec runs go, gt and git from the repository root. ProcessExec in
	// production.
	Exec Exec
	// Src lists and reads the repository's files. When nil, the run lists with
	// `git ls-files` through Exec and reads files through ReadFile.
	Src Source
	// ReadFile reads a file for the git-backed Src, as os.ReadFile does, which a
	// nil ReadFile defaults to. The run joins the root onto the name first.
	ReadFile func(name string) ([]byte, error)
	// Environ is the whole process environment, handed to the children the sweep
	// starts.
	Environ []string
	// Now is the clock a tier's elapsed time is read from.
	Now func() time.Time
}

// Run runs the tiers args names, in the order given, and returns the exit code.
// Each tier's summary line goes to stdout as the tier ends; the progress lines,
// the failure excerpts and every error go to stderr. The code is 0 when every
// tier ended GREEN, 1 when any ended RED, and 2 for a command line this
// package cannot read, a log directory it cannot make, or a tier it does not
// offer, reached after the tiers before it have printed their lines. The daemon
// reads the lines byte for byte (internal/daemon/tier_sweep.go).
//
// Every run leaves <logdir>/timings.jsonl, one line per package it started, and
// replaces TIER_SWEEP_TIMINGS with those lines when the environment names one.
// A timings file that cannot be written is named on stderr once and changes no
// verdict.
func Run(args []string, stdout, stderr io.Writer, d Deps) int {
	root, tiers, err := splitRoot(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n%s\n", Name, err, usage)
		return UsageExit
	}
	host := d.Host
	if host.WriteFile == nil {
		host.WriteFile = os.WriteFile
	}
	if host.Rename == nil {
		host.Rename = os.Rename
	}
	opts, err := ParseOptions(tiers, host)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return UsageExit
	}
	exec := d.Exec
	if root != DefaultRoot {
		exec = inRoot(root, exec)
	}
	src := d.Src
	if src == nil {
		read := d.ReadFile
		if read == nil {
			read = os.ReadFile
		}
		src = gitSource{exec: exec, read: func(name string) ([]byte, error) { return read(filepath.Join(root, name)) }}
	}
	timings := OpenTimings(filepath.Join(opts.LogDir, TimingsName), stderr)
	defer timings.Close()
	s := &Sweep{Opts: opts, Exec: exec, Src: src, Environ: d.Environ, Stderr: stderr, Now: d.Now, Timings: timings}
	red := false
	for _, tier := range opts.Tiers {
		var line string
		var tierRed bool
		switch tier {
		case TierShell:
			line, tierRed = s.Shell()
		case TierIntegration:
			line, tierRed = s.Integration()
		case TierRace:
			line, tierRed = s.Race()
		default:
			fmt.Fprintln(stderr, (&UnknownTierError{Tier: tier}).Error())
			return UsageExit
		}
		fmt.Fprintln(stdout, line)
		red = red || tierRed
	}
	if opts.TimingsFile != "" {
		timings.Replace(opts.TimingsFile, host)
	}
	if red {
		return 1
	}
	return 0
}

// splitRoot takes --root (as "--root dir" or "--root=dir") out of args and
// returns it with the remaining arguments, which are the tiers. Any other
// argument is a tier name, as the script took every argument.
func splitRoot(args []string) (root string, tiers []string, err error) {
	root = DefaultRoot
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--root":
			if i+1 == len(args) || args[i+1] == "" {
				return "", nil, errors.New("--root needs a directory")
			}
			i++
			root = args[i]
		case strings.HasPrefix(a, "--root="):
			if root = strings.TrimPrefix(a, "--root="); root == "" {
				return "", nil, errors.New("--root needs a directory")
			}
		default:
			tiers = append(tiers, a)
		}
	}
	return root, tiers, nil
}

// inRoot runs exec with every directory taken from root, so a run works from the
// repository root wherever the process started.
func inRoot(root string, exec Exec) Exec {
	return func(dir string, env []string, argv []string, out io.Writer) int {
		return exec(filepath.Join(root, dir), env, argv, out)
	}
}

// gitSource is the Source over a repository: `git ls-files` for the listings,
// run through exec, and read for the file contents.
type gitSource struct {
	exec Exec
	read func(name string) ([]byte, error)
}

// LsFiles lists the tracked files under dir that match pattern, as git prints
// them, relative to dir. git's pattern matching crosses slashes, which the
// script's '*/go.mod' relies on.
func (g gitSource) LsFiles(dir, pattern string) ([]string, error) {
	var out bytes.Buffer
	if code := g.exec(dir, nil, []string{"git", "ls-files", "--", pattern}, &out); code != 0 {
		return nil, fmt.Errorf("git ls-files %s in %s exited %d: %s", pattern, dir, code, strings.TrimSpace(out.String()))
	}
	var files []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// ReadFile reads one file of the repository.
func (g gitSource) ReadFile(name string) ([]byte, error) { return g.read(name) }
