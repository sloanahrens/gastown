// Package tiersweep is the hourly tier sweep: the tiers `make gate` does not
// run (shell, integration, race), each reported as one summary line the daemon
// parses byte for byte.
package tiersweep

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Name is the command's name: the binary, and the prefix of every line it
// prints.
const Name = "tier-sweep"

// The tiers a run may name. A command line that names none runs them in this
// order.
const (
	TierShell       = "shell"
	TierIntegration = "integration"
	TierRace        = "race"
)

// The environment names the sweep reads. SHELL_TESTS is not among them: the
// shell tier is the shell-tier runner's, and it passes SHELL_TESTS there.
const (
	EnvSkip    = "TIER_SWEEP_SKIP"
	EnvTimeout = "TIER_SWEEP_TIMEOUT"
	EnvNice    = "TIER_SWEEP_NICE"
	EnvLogDir  = "TIER_SWEEP_LOGDIR"
	EnvTrigger = "TIER_SWEEP_TRIGGER"
	EnvTested  = "TIER_SWEEP_TESTED"
	EnvResume  = "TIER_SWEEP_RESUME"
	EnvTimings = "TIER_SWEEP_TIMINGS"
	EnvJobs    = "TIER_SWEEP_JOBS"
)

// The values a name takes when the environment leaves it unset.
const (
	// DefaultTimeout is TIER_SWEEP_TIMEOUT: the per-package go test timeout.
	DefaultTimeout = "10m"
	// DefaultTempDir is TMPDIR's stand-in when it is unset or empty.
	DefaultTempDir = "/tmp"
	// MaxJobs is the most packages TIER_SWEEP_JOBS may run at once: a sweep is
	// sized for the one host it runs on, and a typo must not swamp it.
	MaxJobs = 16
	// logDirPattern names the default log directory: tier-sweep.XXXXXX under
	// TMPDIR.
	logDirPattern = "tier-sweep."
)

// UsageExit is the code the command exits for every refusal that is not a RED
// tier: a tier it does not offer (reached once the tiers before it have printed
// their summary lines), a command line it cannot read, and a log directory it
// cannot make.
const UsageExit = 2

// Options is one tier-sweep invocation: the tiers a command line named and the
// environment the run reads.
type Options struct {
	// Tiers are the tiers to run, in the order given. A command line that
	// names none runs TierShell, TierIntegration and TierRace in that order.
	// A tier the sweep does not offer stays here: the run reports it when it
	// reaches it, so the tiers before it keep their summary lines.
	Tiers []string
	// SkipList is TIER_SWEEP_SKIP, the known-red entries SkipFor reads.
	SkipList string
	// Timeout is TIER_SWEEP_TIMEOUT, the per-package go test timeout.
	Timeout string
	// Nice is TIER_SWEEP_NICE parsed: the CPU niceness each package's
	// `gt slot run` takes, 0 to 19. Nil means the variable was unset, and the
	// sweep passes no --nice, leaving gt slot run its own default.
	Nice *int
	// LogDir is TIER_SWEEP_LOGDIR: the directory the run writes one log per
	// package into, created when a run reads its options.
	LogDir string
	// OwnsLogDir reports whether the run made LogDir itself, the default
	// tier-sweep.XXXXXX under TMPDIR, rather than taking a caller's
	// TIER_SWEEP_LOGDIR. A GREEN run removes a directory it owns.
	OwnsLogDir bool
	// LogDirParent is the directory LogDir was made in when OwnsLogDir, TMPDIR
	// or DefaultTempDir: the directory a run prunes old log directories from.
	// Empty when the caller named LogDir.
	LogDirParent string
	// Trigger is TIER_SWEEP_TRIGGER as given; "0" turns the container trigger
	// off, and every other value (including the default) leaves it on.
	Trigger string
	// Tested is TIER_SWEEP_TESTED, the commit a run tests.
	Tested string
	// ResumeFile is TIER_SWEEP_RESUME: the file a run appends each package it
	// passed to, and reads the packages an earlier run at this commit and skip
	// list already passed, so a sweep cut by a restart runs only what is left.
	// Empty names none.
	ResumeFile string
	// TimingsFile is TIER_SWEEP_TIMINGS: a persistent file a run replaces with
	// its own timings lines at the end, so the next run can read this one's.
	// Empty names none.
	TimingsFile string
	// Jobs is TIER_SWEEP_JOBS: how many packages the sweep runs at once, 1 to
	// MaxJobs. Unset is 1, one package at a time, the sweep's original shape.
	Jobs int
}

// UnknownTierError is a command line naming a tier the sweep does not offer.
// Error is the line the command prints on stderr, which the daemon matches
// with its tierSweepUnknownTierRE.
type UnknownTierError struct{ Tier string }

func (e *UnknownTierError) Error() string {
	return fmt.Sprintf("%s: unknown tier %s (want %s, %s or %s)",
		Name, e.Tier, TierShell, TierIntegration, TierRace)
}

// KnownTier reports whether name is one of the three tiers a sweep runs.
func KnownTier(name string) bool {
	switch name {
	case TierShell, TierIntegration, TierRace:
		return true
	}
	return false
}

// Host is what a run reads and writes the machine through: its environment, the
// directory calls the log directory and the prune need, and the file calls the
// timings files go through.
type Host struct {
	// Env reads an environment name. An empty value means the name is unset,
	// so its default applies.
	Env func(string) string
	// MkdirTemp creates the default log directory under TMPDIR.
	MkdirTemp func(dir, pattern string) (string, error)
	// MkdirAll creates TIER_SWEEP_LOGDIR and its parents. It runs for the
	// default directory too, where it finds the directory there.
	MkdirAll func(path string, perm fs.FileMode) error
	// WriteFile writes a file, as os.WriteFile does: it is how a run writes the
	// temp file TIER_SWEEP_TIMINGS is replaced through. A nil WriteFile takes
	// os.WriteFile in Run, as a nil Deps.ReadFile takes os.ReadFile.
	WriteFile func(name string, data []byte, perm fs.FileMode) error
	// ReadFile reads a file, as os.ReadFile does: it is how a parallel run
	// reads the previous run's lines from TIER_SWEEP_TIMINGS. A nil ReadFile
	// takes os.ReadFile in Run.
	ReadFile func(name string) ([]byte, error)
	// Rename renames a file, as os.Rename does: it puts the temp file over
	// TIER_SWEEP_TIMINGS, so a reader never sees a torn file. A nil Rename takes
	// os.Rename in Run.
	Rename func(oldpath, newpath string) error
	// AppendFile appends to a file, creating it when it is not there, as one
	// O_APPEND write does: TIER_SWEEP_RESUME grows one line per passed package
	// and is never rewritten. A nil AppendFile takes the process's filesystem in
	// Run, as a nil ReadFile takes os.ReadFile.
	AppendFile func(name string, data []byte) error
	// Remove deletes one file, as os.Remove does: TIER_SWEEP_RESUME's own record
	// after a GREEN run, and nothing else. A directory is never removed through
	// it, so a path that is not the record is a refusal, not a recursive delete
	// (om b49f3e). A nil Remove leaves the record in place, named once.
	Remove func(name string) error
	// RemoveAll deletes a log directory and everything under it, as os.RemoveAll
	// does: a GREEN run's own directory, and the old ones a prune deletes.
	// Remove, RemoveAll, ReadDir and Lstat are the only way Run reaches a
	// filesystem for cleanup, and it fills no os default for them: a host that
	// leaves one nil
	// has every cleanup skipped, named once on stderr (gt-xk9my).
	RemoveAll func(path string) error
	// ReadDir lists a directory, as os.ReadDir does: the prune reads TMPDIR
	// through it.
	ReadDir func(name string) ([]fs.DirEntry, error)
	// Lstat describes a path without following a symlink, as os.Lstat does:
	// the prune never deletes a link, only the directory it finds there.
	Lstat func(name string) (fs.FileInfo, error)
}

// ProcessHost reads and writes the running process.
var ProcessHost = Host{
	Env: os.Getenv, MkdirTemp: os.MkdirTemp, MkdirAll: os.MkdirAll,
	WriteFile: os.WriteFile, ReadFile: os.ReadFile, Rename: os.Rename,
	AppendFile: appendToFile,
	Remove:     os.Remove,
	RemoveAll:  os.RemoveAll, ReadDir: os.ReadDir, Lstat: os.Lstat,
}

// ParseOptions resolves one tier-sweep command line and the environment it
// reads, creating the log directory so no run starts without one.
func ParseOptions(args []string, host Host) (Options, error) {
	opts := Options{
		Tiers:       append([]string(nil), args...),
		SkipList:    host.Env(EnvSkip),
		Timeout:     orDefault(host.Env(EnvTimeout), DefaultTimeout),
		Trigger:     host.Env(EnvTrigger),
		Tested:      host.Env(EnvTested),
		ResumeFile:  host.Env(EnvResume),
		TimingsFile: host.Env(EnvTimings),
	}
	if len(opts.Tiers) == 0 {
		opts.Tiers = []string{TierShell, TierIntegration, TierRace}
	}
	nice, err := parseNice(host.Env(EnvNice))
	if err != nil {
		return Options{}, err
	}
	opts.Nice = nice
	jobs, err := parseJobs(host.Env(EnvJobs))
	if err != nil {
		return Options{}, err
	}
	opts.Jobs = jobs
	logDir := host.Env(EnvLogDir)
	if logDir == "" {
		parent := orDefault(host.Env("TMPDIR"), DefaultTempDir)
		dir, err := host.MkdirTemp(parent, logDirPattern)
		if err != nil {
			return Options{}, fmt.Errorf("%s: create the default log directory: %w", Name, err)
		}
		logDir = dir
		opts.OwnsLogDir = true
		opts.LogDirParent = parent
	}
	if err := host.MkdirAll(logDir, 0o755); err != nil {
		return Options{}, fmt.Errorf("%s: create log directory %s: %w", Name, logDir, err)
	}
	opts.LogDir = logDir
	return opts, nil
}

// SkipFor reports how TIER_SWEEP_SKIP treats one package in one tier: whole
// for a package it skips entirely, or the -skip pattern for a package that
// runs some of its tests. The first entry that names a pattern wins. An entry
// that stops at the third colon names none, so it changes nothing and a later
// entry for the same package still applies (gt-jtyrs).
func SkipFor(list, tier, pkg string) (whole bool, pattern string) {
	prefix := tier + ":" + pkg
	for _, entry := range strings.Fields(list) {
		switch {
		case entry == prefix:
			return true, ""
		case strings.HasPrefix(entry, prefix+":"):
			if rest := entry[len(prefix)+1:]; rest != "" {
				return false, rest
			}
		}
	}
	return false, ""
}

// parseNice reads TIER_SWEEP_NICE: nil when the variable is unset, and
// otherwise a whole number from 0 to 19. Anything else is the caller's usage
// error, so a typo never reaches a package as a changed niceness.
func parseNice(value string) (*int, error) {
	if value == "" {
		return nil, nil
	}
	nice, err := strconv.Atoi(value)
	if err != nil || nice < 0 || nice > 19 {
		return nil, fmt.Errorf("%s: %s=%q: want a whole number from 0 to 19", Name, EnvNice, value)
	}
	return &nice, nil
}

// parseJobs reads TIER_SWEEP_JOBS: unset is one package at a time, and anything
// else is a whole number from 1 to MaxJobs, so a typo never changes how many
// go test children run at once.
func parseJobs(value string) (int, error) {
	if value == "" {
		return 1, nil
	}
	jobs, err := strconv.Atoi(value)
	if err != nil || jobs < 1 || jobs > MaxJobs {
		return 0, fmt.Errorf("%s: %s=%q: want a whole number from 1 to %d", Name, EnvJobs, value, MaxJobs)
	}
	return jobs, nil
}

// orDefault returns fallback when value is empty.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
