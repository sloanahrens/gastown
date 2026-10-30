package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// name is the runner's name in its own messages.
const name = "sweep-run"

// The codes the command exits.
const (
	// UsageExit is a command line this runner cannot use: nothing runs and no
	// result is written.
	UsageExit = 2
	// FailureExit is a run that could not produce a result, named on stderr: the
	// work directory could not be made, the output directory could not be
	// prepared, or the result could not be written. A sweep whose tiers failed
	// exits 0: its verdicts are in the result, which is what the daemon reads.
	FailureExit = 1
)

// usage is the line every refusal of the command line prints.
const usage = "usage: sweep-run --rig R --sha S [--tiers shell,integration,race] [--jobs N] [--out DIR] [--repo DIR]"

// maxJobs is the largest --jobs, and the one sweeprunner.Spec.Validate holds a
// render to: a spec the daemon cannot render is one this runner need not accept.
const maxJobs = 16

// envJobs is the worker count the sweep reads. The sweep package does not read
// it yet (S7, gt-0rtgc, adds the knob there); the runner sets it now, so the
// container passes the count as soon as the sweep grows it.
const envJobs = "TIER_SWEEP_JOBS"

// The result's runner block. The engine that started the container is not
// observable from inside it, and the rendered run passes the container the
// rig, sha, tiers and jobs and no image reference, so the entrypoint records
// what it can see: the run happened in a container, and which image it ran is
// not claimed (the daemon's launch record names it).
const (
	runnerBackend = "container"
	runnerImage   = "unrecorded"
)

// Deps are the seams one run reads through: the clone, the sweep, the clock and
// the filesystem. A test fills every field with a fake, so no run starts git or
// a sweep, reads the clock or writes outside t.TempDir().
type Deps struct {
	// Clone copies repo at sha into work: a clone of the bare repository with no
	// hard links, then the sha checked out detached. A sha the repository does
	// not hold is an error naming git's own text, not a panic.
	Clone func(repo, sha, work string) error
	// Sweep runs the tiers args names, with work as the repository root. The
	// overrides reach the sweep's own option reads and the environment of every
	// child it starts; nothing else in the environment changes.
	Sweep func(args []string, overrides map[string]string, stdout, stderr io.Writer) int
	// WriteResult records a result as result.json in dir, as sweeprunner.Write.
	WriteResult func(dir string, r sweeprunner.Result) error
	// MkdirTemp creates the work directory the clone lands in.
	MkdirTemp func(dir, pattern string) (string, error)
	// MkdirAll creates the output directory and its parents.
	MkdirAll func(path string, perm fs.FileMode) error
	// RemoveAll deletes the work directory after the run.
	RemoveAll func(path string) error
	// Environ is the process environment: where a flag left out takes the
	// container's value, and what the sweep's children inherit.
	Environ []string
	// Now is the clock the result's started and finished times are read from.
	Now func() time.Time
}

// options is one resolved command line: the run's inputs, with the container's
// environment filling what a flag did not name.
type options struct {
	// Rig is the rig whose sweep runs, and SHA the commit it tests.
	Rig string
	SHA string
	// Tiers are the tiers to run, in the order given.
	Tiers []string
	// Jobs is how many packages the sweep tests at once.
	Jobs int
	// Out is the directory result.json and the logs are written into, and Repo
	// the rig's bare repository, read-only in the container.
	Out  string
	Repo string
}

// logDir is where the sweep writes its per-package logs: inside the output
// directory, so the daemon reads them from the same mount the result arrived in.
func (o options) logDir() string { return filepath.Join(o.Out, "logs") }

// Run runs one container sweep and returns the process's exit code: 0 whenever
// a result was written, whatever verdicts it holds, and non-zero only when the
// run could not produce one (UsageExit for a command line it cannot use,
// FailureExit for a result that could not be written).
func Run(args []string, stdout, stderr io.Writer, d Deps) int {
	opts, err := parseArgs(args, d.Environ)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n%s\n", name, err, usage)
		return UsageExit
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	if err := d.MkdirAll(opts.Out, 0o755); err != nil {
		fmt.Fprintf(stderr, "%s: prepare output directory %s: %v\n", name, opts.Out, err)
		return FailureExit
	}
	work, err := d.MkdirTemp("", name+".")
	if err != nil {
		fmt.Fprintf(stderr, "%s: create the work directory: %v\n", name, err)
		return FailureExit
	}
	defer removeWork(d.RemoveAll, work, stderr)

	res := sweeprunner.Result{
		Rig:     opts.Rig,
		SHA:     opts.SHA,
		Started: now(),
		Runner:  runnerRecord(),
	}
	if err := d.Clone(opts.Repo, opts.SHA, work); err != nil {
		// No tier ran, so every requested tier is RED with the reason: a run
		// that could not start must never leave a tier it did not test looking
		// GREEN (gt-dibnm).
		res.Tiers = redTiers(opts.Tiers, opts.logDir(), oneLine(err.Error()))
	} else {
		overrides := map[string]string{
			tiersweep.EnvLogDir: opts.logDir(),
			tiersweep.EnvTested: opts.SHA,
			envJobs:             strconv.Itoa(opts.Jobs),
		}
		// The sweep's summary lines are echoed as it prints them, so the
		// container's log reads like a host sweep's, and captured for the result.
		var captured bytes.Buffer
		code := d.Sweep(sweepArgs(work, opts.Tiers), overrides, io.MultiWriter(stdout, &captured), stderr)
		res.Tiers = tiersFrom(opts.Tiers, captured.String(), code, opts.logDir())
	}
	res.Finished = now()
	if err := d.WriteResult(opts.Out, res); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return FailureExit
	}
	return 0
}

// parseArgs resolves the command line and the environment it falls back to,
// refusing a value no run can use before anything is created or started.
func parseArgs(args, env []string) (options, error) {
	opts := options{
		Rig:  envValue(env, sweeprunner.EnvRig),
		SHA:  envValue(env, sweeprunner.EnvSHA),
		Out:  sweeprunner.OutputDir,
		Repo: sweeprunner.RepoMount,
		Jobs: 1,
	}
	tiers := envValue(env, sweeprunner.EnvTiers)
	if raw := envValue(env, sweeprunner.EnvJobs); raw != "" {
		jobs, err := strconv.Atoi(raw)
		if err != nil {
			return options{}, fmt.Errorf("%s=%q: want a whole number from 1 to %d", sweeprunner.EnvJobs, raw, maxJobs)
		}
		opts.Jobs = jobs
	}
	// Each flag takes its value as "--flag value" or "--flag=value"; anything
	// else on the command line is refused rather than guessed at.
	for i := 0; i < len(args); i++ {
		flag, value, hasValue := strings.Cut(args[i], "=")
		switch flag {
		case "--rig", "--sha", "--tiers", "--jobs", "--out", "--repo":
			if !hasValue {
				if i+1 == len(args) {
					return options{}, fmt.Errorf("%s needs a value", flag)
				}
				i++
				value = args[i]
			}
		default:
			return options{}, fmt.Errorf("unknown argument %q", args[i])
		}
		switch flag {
		case "--rig":
			opts.Rig = value
		case "--sha":
			opts.SHA = value
		case "--tiers":
			tiers = value
		case "--jobs":
			jobs, err := strconv.Atoi(value)
			if err != nil {
				return options{}, fmt.Errorf("--jobs %q: want a whole number from 1 to %d", value, maxJobs)
			}
			opts.Jobs = jobs
		case "--out":
			opts.Out = value
		case "--repo":
			opts.Repo = value
		}
	}
	opts.Tiers = splitTiers(tiers)
	if err := opts.validate(); err != nil {
		return options{}, err
	}
	return opts, nil
}

// validate refuses what a run cannot start with. The rules on the rig, sha,
// tiers and jobs are sweeprunner.Spec.Validate's, so a spec the daemon renders
// is one this runner starts; a test holds the two to the same answers.
func (o options) validate() error {
	if o.Rig == "" {
		return fmt.Errorf("--rig is required (or %s)", sweeprunner.EnvRig)
	}
	if !nameSegment(o.Rig) {
		return fmt.Errorf("--rig %q: want one name segment, not a path", o.Rig)
	}
	if o.SHA == "" {
		return fmt.Errorf("--sha is required (or %s)", sweeprunner.EnvSHA)
	}
	if !shaPattern.MatchString(o.SHA) {
		return fmt.Errorf("--sha %q: want 40 lower-case hex digits", o.SHA)
	}
	seen := make(map[string]bool, len(o.Tiers))
	for _, tier := range o.Tiers {
		if !tiersweep.KnownTier(tier) {
			return fmt.Errorf("--tiers: %q is not a tier the sweep runs (want %s, %s or %s)",
				tier, tiersweep.TierShell, tiersweep.TierIntegration, tiersweep.TierRace)
		}
		if seen[tier] {
			return fmt.Errorf("--tiers: %q appears twice", tier)
		}
		seen[tier] = true
	}
	if o.Jobs < 1 || o.Jobs > maxJobs {
		return fmt.Errorf("--jobs %d: want 1 to %d", o.Jobs, maxJobs)
	}
	if !filepath.IsAbs(o.Out) {
		return fmt.Errorf("--out %q: want an absolute path", o.Out)
	}
	if !filepath.IsAbs(o.Repo) {
		return fmt.Errorf("--repo %q: want an absolute path", o.Repo)
	}
	return nil
}

// splitTiers reads the comma-separated tier list. A list naming none runs the
// three a tier-sweep command line naming none runs, which is what the rendered
// container always names (sweeprunner.TiersOrDefault).
func splitTiers(list string) []string {
	var tiers []string
	for _, tier := range strings.Split(list, ",") {
		if tier = strings.TrimSpace(tier); tier != "" {
			tiers = append(tiers, tier)
		}
	}
	if len(tiers) == 0 {
		return []string{tiersweep.TierShell, tiersweep.TierIntegration, tiersweep.TierRace}
	}
	return tiers
}

// sweepArgs is the sweep's command line: the work tree as its root, then the
// tiers in the order the run was asked for.
func sweepArgs(work string, tiers []string) []string {
	return append([]string{"--root", work}, tiers...)
}

// tiersFrom turns the sweep's stdout into the result's tiers: one entry per
// requested tier, in the requested order, holding the exact summary line the
// sweep printed. A tier with no line — the sweep never reached it, or refused
// its arguments — is RED with the reason, because a tier that did not run is
// not a tier that passed.
func tiersFrom(tiers []string, stdout string, code int, logDir string) []sweeprunner.Tier {
	lines := make(map[string]string, len(tiers))
	for _, line := range strings.Split(stdout, "\n") {
		if tier, ok := summaryTier(line); ok {
			lines[tier] = line
		}
	}
	out := make([]sweeprunner.Tier, 0, len(tiers))
	for _, tier := range tiers {
		line, ok := lines[tier]
		if !ok {
			line, _ = tiersweep.Summary(tier, logDir, 0, 1, 0, fmt.Sprintf("no summary line (exit %d)", code), 0)
		}
		out = append(out, sweeprunner.Tier{Tier: tier, Summary: line})
	}
	return out
}

// redTiers is every requested tier RED for reason: the run could not start, so
// nothing was tested. The line is the sweep's own format, which the daemon
// parses byte for byte, with the reason standing in for the unit the sweep
// would have named.
func redTiers(tiers []string, logDir, reason string) []sweeprunner.Tier {
	out := make([]sweeprunner.Tier, 0, len(tiers))
	for _, tier := range tiers {
		line, _ := tiersweep.Summary(tier, logDir, 0, 1, 0, reason, 0)
		out = append(out, sweeprunner.Tier{Tier: tier, Summary: line})
	}
	return out
}

// summaryTier reports the tier a sweep summary line names.
func summaryTier(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, tiersweep.Name+": ")
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return "", false
	}
	return fields[0], true
}

// runnerRecord is the result's runner block: what this entrypoint can observe
// of the engine it runs under.
func runnerRecord() sweeprunner.Runner {
	return sweeprunner.Runner{
		Backend: runnerBackend,
		Image:   runnerImage,
		// Go 1.26 resolves GOMAXPROCS from the container's CPU limit, so this is
		// the share the run was given rather than the host's core count.
		CPUs: runtime.GOMAXPROCS(0),
	}
}

// removeWork deletes the clone the run worked in. The result and the logs are
// in the output directory, so the work tree is scratch; a removal that fails is
// one stderr line and changes no verdict, as a sweep's log cleanup is (gt-xk9my).
func removeWork(removeAll func(string) error, work string, stderr io.Writer) {
	if removeAll == nil {
		return
	}
	if err := removeAll(work); err != nil {
		fmt.Fprintf(stderr, "%s: remove the work directory %s: %v (the result stands)\n", name, work, err)
	}
}

// gitRunner runs one git command in dir and returns its exit code and output,
// as runGit does. A test substitutes a fake, so the unit tier starts no process.
type gitRunner func(dir string, args ...string) (int, string)

// gitClone makes the tree the sweep runs in.
func gitClone(repo, sha, work string) error { return cloneWith(runGit, repo, sha, work) }

// cloneWith is gitClone over a runner: the bare repository cloned without hard
// links, since the mount is read-only and the clone must not share its objects,
// then the sha checked out detached.
//
// safe.directory is set for the bare repository because the mount can be owned
// by another user, and git refuses to read a repository it does not trust.
func cloneWith(run gitRunner, repo, sha, work string) error {
	if code, out := run("", "-c", "safe.directory="+repo, "clone", "--no-hardlinks", repo, work); code != 0 {
		return fmt.Errorf("git clone %s: %s", repo, out)
	}
	if code, out := run(work, "checkout", "--detach", sha); code != 0 {
		return fmt.Errorf("git checkout %s: %s", sha, out)
	}
	return nil
}

// runGit runs one git command in dir and returns its exit code and its output
// as one line, which is what a result can hold.
func runGit(dir string, args ...string) (int, string) {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the program is the constant git, and the arguments are this file's own repo, sha and work paths
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, oneLine(string(out))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), oneLine(string(out))
	}
	// git could not be started at all: the shell's code for a command it cannot
	// find, so the run reads as a failure rather than a pass.
	return 127, oneLine(err.Error())
}

// processSweep runs the sweep in this process with its real seams: git, go and
// gt as child processes, and the real clock and filesystem. The overrides reach
// the sweep's own option reads through the host and its children through the
// environment, without mutating the process's environment.
func processSweep(args []string, overrides map[string]string, stdout, stderr io.Writer) int {
	host := tiersweep.ProcessHost
	host.Env = func(name string) string {
		if value, ok := overrides[name]; ok {
			return value
		}
		return os.Getenv(name)
	}
	return tiersweep.Run(args, stdout, stderr, tiersweep.Deps{
		Host:     host,
		Exec:     tiersweep.ProcessExec,
		ReadFile: os.ReadFile,
		Environ:  environWith(os.Environ(), overrides),
		Now:      time.Now,
	})
}

// environWith returns base with the overrides appended, in a fixed order, so the
// environment a sweep's children see does not depend on map iteration order.
func environWith(base []string, overrides map[string]string) []string {
	env := append([]string(nil), base...)
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+overrides[name])
	}
	return env
}

// envValue reads name from an environment slice, as os.Getenv reads the process.
// An empty value means the name is unset, so its default applies.
func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
}

// shaPattern is a git commit as git prints it: sweeprunner.Spec's rule, so a
// spec the daemon renders names a commit this runner accepts.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// nameSegment reports whether v can stand as one name: sweeprunner's rule for a
// rig, which is part of every container name the daemon builds from it.
func nameSegment(v string) bool {
	if v == "" || v == "." || v == ".." || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		if r == '/' || r == '\\' || r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// oneLine is text as one summary line can hold it: control characters read as
// spaces, runs of whitespace as one space. A multi-line git error then fits the
// line the daemon parses and cannot pass itself off as a second line.
func oneLine(text string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return strings.Join(strings.Fields(clean), " ")
}
