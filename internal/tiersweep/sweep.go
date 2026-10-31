package tiersweep

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usage is the line a command line this package cannot read prints.
const usage = "usage: tier-sweep [--root dir] [shell] [integration] [race]"

// sweepGoesOn ends a cleanup failure's stderr line: the run's verdict is its
// tiers', never its log directory's (gt-xk9my).
const sweepGoesOn = "the sweep goes on"

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
	// BuildExec runs the sweep's one go build under a wall-clock bound.
	// ProcessBuildExec in production. Nil runs the build through Exec, which
	// takes no deadline.
	BuildExec BuildExec
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
	// Began is told each package the integration tier's pool hands to Exec, in
	// the order it hands them out. Nil records nothing; a test reads the order.
	Began func(pkg string)
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
// verdict. TIER_SWEEP_JOBS above 1 runs that many packages at once, longest
// first by the file's walls, with each package serial.txt names run alone.
//
// TIER_SWEEP_RESUME names the record of packages this run may take as passed:
// one line is appended for each package that passes, and a package already
// recorded at this TIER_SWEEP_TESTED commit, skip list and GOFLAGS is counted as
// passed and not run, so a sweep a restart cut finishes the work it had left. The
// variable names the record file and the run uses it. A GREEN run deletes that
// record only when it is one regular file, in a real directory, made of resume
// records; anything else the variable names is left alone and named once. A run
// that names no commit resumes nothing.
//
// A run that made its own log directory first prunes the old ones beside it
// (PruneMaxAge), and removes its own after the last summary line when every
// tier was GREEN; a RED run keeps it for diagnosis. A prune or removal that
// fails is one stderr line and changes no verdict, and a host that does not
// wire the calls they need leaves every directory alone rather than falling
// back to os (gt-xk9my).
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
	// The prune and the removal run only through the host's own calls. Run
	// fills no default for them, unlike WriteFile and Rename above: a host that
	// wired Env, MkdirTemp and MkdirAll but not these has no filesystem seam,
	// and defaulting to os would list, stat and delete on the machine the run
	// happens to be on (gt-xk9my).
	cleanup := cleanupReady(host)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	opts, err := ParseOptions(tiers, host)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return UsageExit
	}
	if opts.OwnsLogDir && !cleanup {
		fmt.Fprintf(stderr, "%s: log directory cleanup skipped: host has no ReadDir, Lstat and RemoveAll (%s)\n", Name, sweepGoesOn)
	}
	if opts.OwnsLogDir && cleanup {
		pruneOldLogDirs(host, opts.LogDirParent, now(), opts.LogDir, stderr)
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
	// A parallel run starts the longest packages first, so it reads the last
	// run's walls before the first package starts (gt-0rtgc).
	var walls map[string]int64
	if opts.Jobs > 1 && opts.TimingsFile != "" {
		walls = readWalls(host, opts.TimingsFile, stderr)
	}
	resume := OpenResume(opts, host, stderr)
	buildExec := d.BuildExec
	if buildExec != nil && root != DefaultRoot {
		buildExec = inRootBuild(root, buildExec)
	}
	s := &Sweep{Opts: opts, Exec: exec, BuildExec: buildExec, Src: src, Environ: d.Environ, Stderr: stderr,
		Now: now, Timings: timings, Walls: walls, Began: d.Began, Resume: resume}
	code := runTiers(s, opts.Tiers, stdout, stderr)
	if code == UsageExit {
		timings.Close()
		return code
	}
	if opts.TimingsFile != "" {
		timings.Replace(opts.TimingsFile, host)
	}
	// The file is closed before the directory holding it can go, and Run closes
	// it on every path out, so a GREEN run's removal never leaves a handle open.
	timings.Close()
	if code != 0 {
		return code
	}
	// Every package passed at this commit and skip list, so the record has
	// nothing left to save and the next run starts clean (gt-loqew). Only the
	// record this run derives goes, one regular file (om b49f3e, om 70902e).
	if resume != nil {
		resume.Remove(host)
	}
	if opts.OwnsLogDir && cleanup {
		if err := host.RemoveAll(opts.LogDir); err != nil {
			fmt.Fprintf(stderr, "%s: remove %s: %v (%s)\n", Name, opts.LogDir, err, sweepGoesOn)
		}
	}
	return 0
}

// readWalls reads the previous run's timings file. A file that is not there is
// the first run, and no line is printed; any other failure is one stderr line.
// Either way the run keeps its list order and goes on (gt-0rtgc).
func readWalls(host Host, path string, stderr io.Writer) map[string]int64 {
	read := host.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err == nil {
		return ParseWalls(data)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	fmt.Fprintf(stderr, "%s: timings: read %s: %v (%s)\n", Name, path, err, timingsGoesOn)
	return nil
}

// cleanupReady reports whether host can list a directory, describe an entry and
// delete one. A host missing any of them leaves every log directory alone: Run
// calls no os function in their place (gt-xk9my).
func cleanupReady(host Host) bool {
	return host.ReadDir != nil && host.Lstat != nil && host.RemoveAll != nil
}

// runTiers runs the tiers in the order given, printing each summary line as its
// tier ends, and returns the exit code they make: 0 when every tier was GREEN,
// 1 when any was RED, and UsageExit for a tier the sweep does not offer.
func runTiers(s *Sweep, tiers []string, stdout, stderr io.Writer) int {
	red := false
	for _, tier := range tiers {
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
	if red {
		return 1
	}
	return 0
}

// pruneOldLogDirs deletes the finished runs' log directories under dir, never
// the current run's own, and names each removal on stderr, so a host does not
// accumulate a directory per sweep. A failure is one stderr line: the run's
// verdicts are its tiers', and the directories that were removed are still
// named when one is not.
func pruneOldLogDirs(host Host, dir string, now time.Time, current string, stderr io.Writer) {
	removed, err := pruneExcept(host, dir, now, PruneMaxAge, current)
	for _, path := range removed {
		fmt.Fprintf(stderr, "%s: pruned %s\n", Name, path)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: prune %s: %v (%s)\n", Name, dir, err, sweepGoesOn)
	}
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

// inRootBuild is inRoot for the build seam, so a run started with --root builds
// gt in that tree rather than where the sweep was started.
func inRootBuild(root string, exec BuildExec) BuildExec {
	return func(dir string, env []string, argv []string, out io.Writer, timeout time.Duration) int {
		return exec(filepath.Join(root, dir), env, argv, out, timeout)
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
