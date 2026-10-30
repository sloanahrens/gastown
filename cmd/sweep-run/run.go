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
	"unicode/utf8"

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
	// prepared, the sweep produced no exit status, or the result could not be
	// written. A sweep whose tiers failed exits 0: its verdicts are in the
	// result, which is what the daemon reads.
	FailureExit = 1
)

// usage is the line every refusal of the command line prints.
const usage = "usage: sweep-run --rig R --sha S [--tiers shell,integration,race] [--jobs N] [--out DIR] [--repo DIR]"

// envJobs is the worker count the runner passes to the sweep. The sweep reads
// its own knobs from the environment it is given, so the runner sets this one
// beside the log directory and the tested commit (tiersweep.Options).
const envJobs = "TIER_SWEEP_JOBS"

// The bounds on what the runner keeps of the sweep's output. The sweep is the
// untrusted side of the boundary: a runaway or hostile line must not decide how
// much the result holds or what a reader is shown (gt-dibnm).
const (
	// maxSummaryBytes bounds one summary line in the result.
	maxSummaryBytes = 4 << 10
	// maxCaptureBytes bounds the END of the sweep's stdout the summary lines are
	// scanned out of. The summary lines come last, so a sweep that prints
	// megabytes of progress before its verdicts still has them found.
	maxCaptureBytes = 64 << 10
	// maxStderrCapture bounds the END of the sweep's stderr kept: the progress
	// lines and failure excerpts come before the complaint, so the end is the
	// part a reason is quoted from.
	maxStderrCapture = 8 << 10
	// maxStderrTail bounds the sweep's own words quoted in a RED reason.
	maxStderrTail = 512
	// failedLabel opens the group a summary line carries its reasons in, as
	// tiersweep.Summary writes it and the daemon reads it.
	failedLabel = " failed: "
)

// sweepNoExit is what the Sweep seam returns when the sweep produced no exit
// status: the process died on a signal or never started. Any negative value
// means the same thing.
const sweepNoExit = -1

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
	// child it starts; nothing else in the environment changes. It returns the
	// sweep's exit status, which is the run's verdict: a non-zero status is RED
	// whatever the summary lines say. A negative value means the sweep produced
	// no exit status (sweepNoExit), which is a run that cannot report a result.
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
// FailureExit for a sweep that produced no exit status or a result that could
// not be written).
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
		res.Tiers = redTiers(opts.Tiers, opts.logDir(), sanitize(err.Error(), maxSummaryBytes))
	} else {
		overrides := map[string]string{
			tiersweep.EnvLogDir: opts.logDir(),
			tiersweep.EnvTested: opts.SHA,
			envJobs:             strconv.Itoa(opts.Jobs),
		}
		// The sweep's summary lines are echoed as it prints them, so the
		// container's log reads like a host sweep's, and captured for the result.
		// The capture is bounded and the echo is not: the sweep's whole output
		// reaches the container log, and only the copy the result is built from
		// is capped, at its end (gt-dibnm).
		captured, sweepErrs := newTail(maxCaptureBytes), newTail(maxStderrCapture)
		code := d.Sweep(sweepArgs(work, opts.Tiers), overrides,
			io.MultiWriter(stdout, captured),
			io.MultiWriter(stderr, sweepErrs))
		// No exit status is no verdict: a sweep that was killed or never started
		// did not produce one, so the run reports that it could not produce a
		// result rather than recording a tier it does not know about (gt-dibnm).
		if code < 0 {
			fmt.Fprintf(stderr, "%s: the sweep did not exit (%d): it was killed or never started\n", name, code)
			return FailureExit
		}
		res.Tiers = tiersFrom(opts.Tiers, reportOf(captured, sweepErrs), code, opts.logDir())
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
			return options{}, fmt.Errorf("%s=%q: want a whole number", sweeprunner.EnvJobs, raw)
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
				return options{}, fmt.Errorf("--jobs %q: want a whole number", value)
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

// validate refuses what a run cannot start with. Every rule is
// sweeprunner.Spec.Validate's, so the spec the daemon renders is exactly what
// this runner starts and each rule lives in one place (gt-dibnm). The fields
// the container never receives — the image, the CPU and memory limits, the
// context and the run id — are filled with the values the result records, so
// Validate rules only what the entrypoint was given.
func (o options) validate() error {
	if o.Rig == "" {
		return fmt.Errorf("--rig is required (or %s)", sweeprunner.EnvRig)
	}
	if o.SHA == "" {
		return fmt.Errorf("--sha is required (or %s)", sweeprunner.EnvSHA)
	}
	return sweeprunner.Spec{
		Rig:      o.Rig,
		SHA:      o.SHA,
		Tiers:    o.Tiers,
		Jobs:     o.Jobs,
		OutDir:   o.Out,
		RepoDir:  o.Repo,
		Image:    runnerImage,
		CPUs:     1,
		MemoryGB: 2,
		Context:  runnerBackend,
		RunID:    runnerBackend,
	}.Validate()
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

// sweepReport is what a run holds of the sweep's output: the end of each stream,
// and the note naming what was dropped from their heads.
type sweepReport struct {
	// stdout is the kept end of the sweep's summary stream, stderr the kept end
	// of its progress and complaints.
	stdout string
	stderr string
	// dropped is the note a result carries when the runner had to discard
	// output, and empty when it kept everything the sweep printed.
	dropped string
}

// reportOf reads the captures back once the sweep has exited. What the sweep
// printed beyond the caps is gone, and a run that lost part of its evidence says
// so in the result rather than reading as if it saw everything (gt-dibnm).
func reportOf(stdout, stderrTail *tail) sweepReport {
	rep := sweepReport{stdout: stdout.text(), stderr: stderrTail.text()}
	if dropped := stdout.dropped + stderrTail.dropped; dropped > 0 {
		rep.dropped = fmt.Sprintf("output truncated: %d bytes dropped from the head", dropped)
	}
	return rep
}

// tiersFrom turns the sweep's output into the result's tiers: one entry per
// requested tier, in the requested order.
//
// The sweep's exit status is the verdict, and a summary line only supplies the
// counts and the names (gt-dibnm). A non-zero status with a GREEN line is RED,
// with the contradiction recorded; a tier with no line — the sweep never
// reached it, or refused its arguments — is RED with the status and the tail of
// the sweep's own complaint, because a tier that did not run is not a tier that
// passed.
func tiersFrom(tiers []string, rep sweepReport, code int, logDir string) []sweeprunner.Tier {
	// The log group the runner adds, and the note it may add beside it, are part
	// of the line's cap, so a line that arrived at the limit still ends in a
	// whole group and the daemon still parses it.
	group := " (logs " + logDir + ")"
	budget := lineBudget(rep.dropped)
	room := budget - len(group)
	if room < 1 {
		room = 1
	}
	lines := make(map[string]string, len(tiers))
	for _, raw := range strings.Split(rep.stdout, "\n") {
		line := sanitize(raw, maxSummaryBytes)
		tier, ok := summaryTier(line)
		if !ok {
			continue
		}
		lines[tier] = withFailedNote(withLogDir(capRunes(line, room), logDir), rep.dropped)
	}
	reason := fmt.Sprintf("no summary line (exit %d)", code)
	if tail := tailOf(rep.stderr); tail != "" {
		reason += ": " + tail
	}
	// A non-zero exit is the sweep saying a tier failed. When a summary line
	// already says RED for some tier, the exit is explained and every GREEN tier
	// beside it stays GREEN: the sweep ran those and they passed. Only when the
	// sweep exited non-zero and NO line says RED is there a contradiction no tier
	// can be blamed for, and then every tier that reported GREEN is turned RED,
	// each with the contradiction recorded.
	anyRed := false
	for tier, line := range lines {
		if lineIsRed(line, tier) {
			anyRed = true
		}
	}
	out := make([]sweeprunner.Tier, 0, len(tiers))
	for _, tier := range tiers {
		line, ok := lines[tier]
		switch {
		case !ok:
			line = withFailedNote(generated(tier, logDir, reason, budget), rep.dropped)
		case code != 0 && !anyRed:
			line = redOnFailure(line, tier, code)
		}
		out = append(out, sweeprunner.Tier{Tier: tier, Summary: withinCap(line)})
	}
	return out
}

// lineBudget is what a whole summary line may take once note is added to it.
func lineBudget(note string) int {
	if note == "" {
		return maxSummaryBytes
	}
	return maxSummaryBytes - len(note) - len(", ")
}

// withFailedNote adds a note to a summary line's reasons, which is the only free
// text the daemon reads out of a result; a line with no reason group gains one.
// The note never reaches the log path, which stays the runner's own.
func withFailedNote(line, note string) string {
	if note == "" {
		return line
	}
	i := strings.Index(line, " (logs ")
	if i < 0 {
		return line
	}
	if strings.Contains(line[:i], failedLabel) {
		return line[:i] + ", " + note + line[i:]
	}
	return line[:i] + failedLabel + note + line[i:]
}

// redTiers is every requested tier RED for reason: the run could not start, so
// nothing was tested. The line is the sweep's own format, which the daemon
// parses byte for byte, with the reason standing in for the unit the sweep
// would have named.
func redTiers(tiers []string, logDir, reason string) []sweeprunner.Tier {
	out := make([]sweeprunner.Tier, 0, len(tiers))
	for _, tier := range tiers {
		out = append(out, sweeprunner.Tier{Tier: tier, Summary: generated(tier, logDir, reason, maxSummaryBytes)})
	}
	return out
}

// generated is a RED summary line the runner writes itself: one failing unit
// named by reason, in the sweep's own format, with the whole line bounded at
// budget bytes so its log group and duration are never the part cut off.
func generated(tier, logDir, reason string, budget int) string {
	// The line's fixed parts, then what the reason may take of the rest.
	bare, _ := tiersweep.Summary(tier, logDir, 0, 1, 0, "", 0)
	room := budget - len(bare) - len(failedLabel)
	if room < 1 {
		room = 1
	}
	line, _ := tiersweep.Summary(tier, logDir, 0, 1, 0, capRunes(sanitize(reason, room), room), 0)
	return capRunes(line, budget)
}

// withLogDir replaces a summary line's "(logs ...)" group with logDir, or adds
// one where the daemon's parser reads it: before the " in <duration>" suffix,
// or at the end when the line names no duration. The sweep names a path there,
// and the daemon later opens or deletes what the line names, so the path comes
// from the runner and never from the sweep's text (gt-dibnm).
func withLogDir(line, logDir string) string {
	group := " (logs " + logDir + ")"
	if i := strings.Index(line, " (logs "); i >= 0 {
		if j := strings.Index(line[i:], ")"); j >= 0 {
			return line[:i] + group + line[i+j+1:]
		}
	}
	if i := strings.LastIndex(line, " in "); i >= 0 {
		return line[:i] + group + line[i:]
	}
	return line + group
}

// withinCap keeps a summary line inside maxSummaryBytes once the notes the runner
// adds (the dropped-output note, the exit contradiction) are on it: the part
// before the log group gives way, and the group the daemon parses stays whole.
func withinCap(line string) string {
	if len(line) <= maxSummaryBytes {
		return line
	}
	i := strings.Index(line, " (logs ")
	if i < 0 {
		return capRunes(line, maxSummaryBytes)
	}
	tail := line[i:]
	room := maxSummaryBytes - len(tail)
	if room < 1 {
		return capRunes(line, maxSummaryBytes)
	}
	return capRunes(line[:i], room) + tail
}

// lineIsRed reports whether a tier's summary line carries the RED verdict.
func lineIsRed(line, tier string) bool {
	rest, ok := strings.CutPrefix(line, tiersweep.Name+": "+tier+" ")
	return ok && strings.HasPrefix(rest, "RED")
}

// redOnFailure turns a tier's GREEN line RED when the sweep exited non-zero:
// the exit status is the verdict, and a GREEN line beside it is a contradiction
// the result records rather than hides. The counts and names stay as printed;
// the contradiction joins the failed names so the daemon shows why.
func redOnFailure(line, tier string, code int) string {
	prefix := tiersweep.Name + ": " + tier + " "
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok || !strings.HasPrefix(rest, "GREEN") {
		return line
	}
	line = prefix + "RED" + strings.TrimPrefix(rest, "GREEN")
	note := fmt.Sprintf("sweep exited %d beside a GREEN summary line", code)
	i := strings.Index(line, " (logs ")
	if i < 0 {
		return line
	}
	if strings.Contains(line[:i], failedLabel) {
		return line[:i] + ", " + note + line[i:]
	}
	return line[:i] + failedLabel + note + line[i:]
}

// tailOf is the end of the sweep's stderr as one short line: what a reader
// needs when the sweep said nothing that parses. A sweep's own complaint comes
// after whatever progress it printed, so the end is the part worth keeping.
func tailOf(text string) string {
	if len(text) > maxStderrTail {
		text = text[len(text)-maxStderrTail:]
	}
	return sanitize(text, maxStderrTail)
}

// summaryTier reports the tier a sweep summary line names. A line needs the
// tier and a verdict: the sweep has printed half a line when it was cut, and
// that is not a summary a result can hold.
func summaryTier(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, tiersweep.Name+": ")
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return "", false
	}
	if fields[1] != "GREEN" && fields[1] != "RED" {
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

// gitClone makes the tree the sweep runs in. The unit tier runs no subprocess,
// so its tests drive cloneWith through an injected runner; the real command is
// exercised by the integration tier (clone_integration_test.go).
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
	return sweepWith(tiersweep.Run, args, overrides, os.Getenv, os.Environ(), os.ReadFile, time.Now, stdout, stderr)
}

// sweepRunner runs one sweep: tiersweep.Run in the container, and a fake in a
// test that holds what the sweep was handed.
type sweepRunner func(args []string, stdout, stderr io.Writer, d tiersweep.Deps) int

// sweepWith is processSweep over the runner and the process reads it is given,
// so the wiring is a value a test can hold: the overrides answer the sweep's own
// option reads before the process environment does, and reach each child through
// the environment it is started with.
func sweepWith(run sweepRunner, args []string, overrides map[string]string,
	getenv func(string) string, environ []string, readFile func(string) ([]byte, error), now func() time.Time,
	stdout, stderr io.Writer) int {
	host := tiersweep.ProcessHost
	host.Env = func(key string) string {
		if value, ok := overrides[key]; ok {
			return value
		}
		return getenv(key)
	}
	return run(args, stdout, stderr, tiersweep.Deps{
		Host:     host,
		Exec:     tiersweep.ProcessExec,
		ReadFile: readFile,
		Environ:  environWith(environ, overrides),
		Now:      now,
	})
}

// environWith returns base with the overrides appended, in a fixed order, so the
// environment a sweep's children see does not depend on map iteration order.
func environWith(base []string, overrides map[string]string) []string {
	env := append([]string(nil), base...)
	names := make([]string, 0, len(overrides))
	for key := range overrides {
		names = append(names, key)
	}
	sort.Strings(names)
	for _, key := range names {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

// envValue reads key from an environment slice, as os.Getenv reads the process.
// An empty value means the key is unset, so its default applies.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
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

// ansiRE matches the escape sequences a terminal colors text with: a CSI
// sequence (ESC [ parameters final) and a two-character escape. They carry no
// meaning into a result and would only be quoted back at a reader.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\\\^_]")

// sanitize makes text of unknown provenance safe to store: ANSI escapes
// removed, invalid UTF-8 replaced, control characters read as spaces, runs of
// whitespace collapsed, and the whole cut to max bytes without splitting a
// character. The sweep is the untrusted side of the boundary, so nothing it
// prints reaches a result unchanged (gt-dibnm).
func sanitize(text string, max int) string {
	text = ansiRE.ReplaceAllString(text, "")
	text = strings.ToValidUTF8(text, "�")
	return capRunes(oneLine(text), max)
}

// capRunes cuts text to at most max bytes without splitting a rune.
func capRunes(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := text[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " ")
}

// tail is a bounded copy of the END of a stream the sweep writes: once max bytes
// are held, each write drops the oldest bytes and counts them. The verdicts and
// the complaint come last, so the end is the part a result is built from; the
// writer it is paired with (the process's own stdout or stderr) still receives
// the whole stream, and Write always reports every byte as written.
type tail struct {
	keep    []byte
	max     int
	dropped int64
}

func newTail(max int) *tail { return &tail{max: max} }

// Write keeps the end of what has been written and accepts the rest.
func (t *tail) Write(p []byte) (int, error) {
	t.keep = append(t.keep, p...)
	if over := len(t.keep) - t.max; over > 0 {
		t.keep = t.keep[over:]
		t.dropped += int64(over)
	}
	return len(p), nil
}

// text is the tail as a string. When bytes were dropped the head was cut
// mid-line, so the first line is a fragment of a line the sweep printed and is
// left out: a summary line is read from its start.
func (t *tail) text() string {
	if t.dropped == 0 {
		return string(t.keep)
	}
	if i := bytes.IndexByte(t.keep, '\n'); i >= 0 {
		return string(t.keep[i+1:])
	}
	return ""
}
