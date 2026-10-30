package tiersweep

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Exec runs argv in dir with env (nil inherits the process's environment) and
// sends its stdout and stderr to out. It returns the exit code, where 0 passes.
type Exec func(dir string, env []string, argv []string, out io.Writer) int

// ProcessExec is the production Exec. A command that does not start returns
// 127, the shell's code for a command not found, so the package counts as
// failed rather than passed.
var ProcessExec Exec = func(dir string, env []string, argv []string, out io.Writer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(out, "%s: %v\n", argv[0], err)
	return 127
}

// excerptLines is how many matching lines a failed package shows on stderr:
// enough to name the failure, few enough that a red tier stays readable.
const excerptLines = 5

// Runner runs one tier's packages, one at a time unless Jobs names more, so no
// single go test outlives its timeout and a red package is named on its own.
type Runner struct {
	// Exec runs each package's command. ProcessExec in production.
	Exec Exec
	// LogDir holds one log per package, named by tier and package.
	LogDir string
	// Timeout is the per-package go test timeout, as go test takes it.
	Timeout string
	// SkipList is TIER_SWEEP_SKIP, read by SkipFor.
	SkipList string
	// Env is the environment each package's command runs in. Nil inherits the
	// process's environment.
	Env []string
	// Stderr receives the progress lines and the failure excerpts.
	Stderr io.Writer
	// Timings receives one line per package the runner starts, in the order
	// their commands finished, which is list order for a run of one. A package
	// it skips, refuses or cannot log writes none. Nil writes no timings.
	Timings *Timings
	// Now is the clock a package's wall time is read from. Nil means time.Now.
	Now func() time.Time
	// Jobs is how many packages run at once. Zero or one runs them one at a
	// time in list order; more runs them on that many workers (gt-0rtgc).
	Jobs int
	// Walls is the previous run's wall time per tier and package, keyed by
	// TimingsKey. A run of more than one job starts the longest packages first;
	// nil, or a package with no entry, keeps list order. A run of one job
	// ignores Walls and keeps list order, so it prints what it always has.
	Walls map[string]int64
	// Began, when not nil, is told each package the pool is about to hand to
	// Exec, before the pool starts the next one. The pool cannot see a command's
	// own start, so a test reads the order the pool hands them out from here.
	Began func(pkg string)
	// Resume, when not nil, is the record of the packages already passed at this
	// commit and skip list: a package it names counts as passed and is not run,
	// and each package that passes is appended to it (gt-loqew).
	Resume *Resume
	// stderr keeps the progress lines and the reports whole while a run of more
	// than one job writes them from the dispatcher and the collector at once.
	stderrMu sync.Mutex
}

// Counts is one tier's tally: the packages that passed, failed and were
// skipped, and the names of the ones that failed.
type Counts struct {
	Passed, Failed, Skipped int
	FailedNames             []string
}

// Tier runs go test for each package in pkgs, in dir, under prefix and before
// args, and tallies the results. A package's name is the one the skip list and
// the log use, so a failure in a nested module reads ./tracker/store/pgtest.
// The counts and the failed names follow pkgs' order whatever order the
// packages ran in; a run with Jobs of one or less prints each package's line
// where the package is listed, as the sweep always has.
func (r *Runner) Tier(tier, dir string, prefix, args, pkgs []string) Counts {
	tasks := r.tasks(tier, dir, pkgs, r.passed())
	r.runTasks(tier, dir, prefix, args, tasks)
	return tally(tasks)
}

// passed is the set of packages this tier may count without running them. The
// record is read once, as the tier starts, so packages this run appends cannot
// make a later read think they were passed by an earlier run.
func (r *Runner) passed() map[string]bool {
	if r.Resume == nil {
		return nil
	}
	return r.Resume.Passed()
}

// task is one package's place in a tier run: the package, the name the skip
// list and the log use, the -skip pattern, and what running it produced. Tasks
// keep the caller's list order, which is the order the counts are taken in.
type task struct {
	pkg     string
	name    string
	pattern string
	skip    bool
	resume  bool
	refuse  bool
	result  packageResult
}

// packageResult is what one package's command left behind: the log, the exit
// code, the clock reading before it started, and the failures to report.
type packageResult struct {
	ran       bool
	code      int
	start     time.Time
	log       string
	createErr error
	closeErr  error
}

// tasks resolves each package's fate before anything runs. A skipped package
// stays skipped rather than resumed: the skip list is the operator's decision to
// run nothing there, and counting it as passed would report a tier that ran.
func (r *Runner) tasks(tier, dir string, pkgs []string, passed map[string]bool) []*task {
	tasks := make([]*task, 0, len(pkgs))
	for _, pkg := range pkgs {
		t := &task{pkg: pkg, name: packageName(dir, pkg)}
		switch whole, pattern := SkipFor(r.SkipList, tier, t.name); {
		case strings.HasPrefix(pkg, "-"):
			t.refuse = true
		case whole:
			t.skip = true
		case passed[TimingsKey(tier, t.name)]:
			t.resume = true
		default:
			t.pattern = pattern
		}
		tasks = append(tasks, t)
	}
	return tasks
}

// runTasks prints each package's line and runs the ones with a command to run,
// one at a time in list order when Jobs is one, on a bounded pool in start
// order when it names more.
func (r *Runner) runTasks(tier, dir string, prefix, args []string, tasks []*task) {
	if r.jobs() <= 1 {
		// The serial run keeps the sweep's original lines byte for byte: a
		// package is announced, skipped or refused where it is listed, and the
		// previous run's walls never reorder it (gt-0rtgc).
		for _, t := range tasks {
			if r.announce(tier, t) {
				r.runOne(tier, dir, prefix, args, t)
			}
		}
		return
	}
	var run []*task
	for _, t := range tasks {
		if r.announce(tier, t) {
			run = append(run, t)
		}
	}
	// A serial package runs after the pool has drained, so it is the only child
	// in flight while it runs (gt-0rtgc).
	parallel, serial := splitSerial(startOrder(run, r.Walls, tier))
	r.pool(tier, dir, prefix, args, parallel)
	for _, t := range serial {
		r.runOne(tier, dir, prefix, args, t)
	}
}

// announce prints the line a package earns before it runs and reports whether
// it has a command to run: a refused or skipped package prints its line and
// runs nothing.
func (r *Runner) announce(tier string, t *task) bool {
	switch {
	case t.refuse:
		// go test would read it as a flag, so it fails closed and never runs.
		fmt.Fprintf(r.Stderr, "%s: package argument starts with a dash\n", t.name)
	case t.skip:
		fmt.Fprintf(r.Stderr, "%s: %s %s SKIPPED (%s)\n", Name, tier, t.name, EnvSkip)
	case t.resume:
		fmt.Fprintf(r.Stderr, "%s: %s %s RESUMED (%s)\n", Name, tier, t.name, EnvResume)
	default:
		return true
	}
	return false
}

// jobs is how many packages may run at once; the zero value runs one.
func (r *Runner) jobs() int {
	if r.Jobs < 1 {
		return 1
	}
	return r.Jobs
}

// runOne runs one package in this goroutine and reports it.
func (r *Runner) runOne(tier, dir string, prefix, args []string, t *task) {
	r.start(tier, t)
	t.result = r.runPackage(tier, dir, prefix, args, t, nil)
	r.report(tier, t)
}

// start prints the progress line a package's command is announced with.
func (r *Runner) start(tier string, t *task) {
	r.stderrMu.Lock()
	defer r.stderrMu.Unlock()
	fmt.Fprintf(r.Stderr, "%s: %s %s ...\n", Name, tier, t.name)
}

// runPackage creates one package's log, runs its command and closes the log.
// begin, when not nil, reports that the command has begun; a pool hands the
// next package out only once it has heard that, so commands start in plan
// order. A package whose log cannot be created calls it too, so the pool is
// never left waiting on a command that never runs.
func (r *Runner) runPackage(tier, dir string, prefix, args []string, t *task, begin func()) packageResult {
	began := false
	beganOnce := func() {
		if !began {
			began = true
			if begin != nil {
				begin()
			}
		}
	}
	defer beganOnce()
	log := filepath.Join(r.LogDir, LogName(tier, t.name))
	f, err := os.Create(log)
	if err != nil {
		// Without a log the package cannot be run, and it is not passed.
		beganOnce()
		return packageResult{log: log, createErr: err}
	}
	argv := append(append([]string{}, prefix...), "go", "test", "-count=1", "-timeout", r.Timeout)
	argv = append(argv, args...)
	if t.pattern != "" {
		argv = append(argv, "-skip", t.pattern)
	}
	argv = append(argv, t.pkg)
	start := r.now()
	beganOnce()
	code := r.Exec(dir, r.Env, argv, f)
	closeErr := f.Close()
	return packageResult{ran: true, code: code, start: start, log: log, closeErr: closeErr}
}

// report writes the failure a package's result names and records its timing.
func (r *Runner) report(tier string, t *task) {
	r.stderrMu.Lock()
	defer r.stderrMu.Unlock()
	res := t.result
	if res.createErr != nil {
		fmt.Fprintf(r.Stderr, "    create log %s: %v\n", res.log, res.createErr)
		return
	}
	r.addTiming(tier, t.name, res.start, res.code)
	if res.passed() {
		r.recordPass(tier, t.name)
	}
	if res.closeErr != nil {
		fmt.Fprintf(r.Stderr, "    close log %s: %v\n", res.log, res.closeErr)
	}
	if res.code != 0 || res.closeErr != nil {
		if err := r.excerpt(res.log); err != nil {
			fmt.Fprintf(r.Stderr, "    read log %s: %v\n", res.log, err)
		}
	}
}

// passed reports whether a package's result is one the tier counts as passed:
// its command ran, its log was opened and closed, and it exited zero.
func (res packageResult) passed() bool {
	return res.ran && res.createErr == nil && res.code == 0 && res.closeErr == nil
}

// tally counts the run in the caller's package-list order, so a failure is
// named where the package was listed whatever order it ran in.
func tally(tasks []*task) Counts {
	var c Counts
	for _, t := range tasks {
		switch {
		case t.refuse:
			c.Failed++
			c.FailedNames = append(c.FailedNames, t.name)
		case t.skip:
			c.Skipped++
		case t.resume, t.result.passed():
			c.Passed++
		default:
			c.Failed++
			c.FailedNames = append(c.FailedNames, t.name)
		}
	}
	return c
}

// recordPass appends a package that passed to the resume record, so a sweep a
// restart cuts after it does not run it again (gt-loqew).
func (r *Runner) recordPass(tier, name string) {
	if r.Resume == nil {
		return
	}
	r.Resume.Record(tier, name)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// addTiming records one started package's line in the run's timings.
func (r *Runner) addTiming(tier, name string, start time.Time, code int) {
	if r.Timings == nil {
		return
	}
	r.Timings.Add(Timing{
		Tier:    tier,
		Package: name,
		WallMS:  r.now().Sub(start).Milliseconds(),
		Exit:    code,
		Start:   start.Format(time.RFC3339Nano),
	})
}

// excerpt writes the first excerptLines failure lines of a log to stderr,
// indented, so the red package's cause shows without opening the log. A log
// with no failure line shows its last excerptLines lines instead: a command
// that did not start leaves its reason there, not behind a failure marker.
func (r *Runner) excerpt(log string) error {
	data, err := os.ReadFile(log)
	if err != nil {
		return err
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	shown := 0
	for _, line := range lines {
		if shown == excerptLines {
			break
		}
		if isFailureLine(line) {
			fmt.Fprintf(r.Stderr, "    %s\n", line)
			shown++
		}
	}
	if shown == 0 {
		if len(lines) > excerptLines {
			lines = lines[len(lines)-excerptLines:]
		}
		for _, line := range lines {
			fmt.Fprintf(r.Stderr, "    %s\n", line)
		}
	}
	return nil
}

// isFailureLine reports whether a log line is one the excerpt shows.
func isFailureLine(line string) bool {
	return strings.HasPrefix(line, "--- FAIL") ||
		strings.HasPrefix(line, "panic:") ||
		strings.HasPrefix(line, "FAIL")
}

// packageName is the name a package reports as. Under dir "." it is the
// package as given; under a nested module's dir it is ./dir for the module
// root's ./ and ./dir/pkg for the rest.
func packageName(dir, pkg string) string {
	if dir == "." {
		return pkg
	}
	if pkg == "./" {
		return "./" + dir
	}
	return "./" + dir + "/" + strings.TrimPrefix(pkg, "./")
}

// LogName is the log file for one package: the tier, the package name without
// its leading ./ with each / read as _, then the first four bytes (eight hex
// characters) of the name's sha256. The hash keeps names that read alike, such
// as ./a/b_c and ./a_b/c, in separate files; the tier keeps a name that starts
// with a dash from starting the file name with one. The daemon reads a Go
// tier's failure log under this name (internal/daemon/tier_sweep_bead.go).
func LogName(tier, name string) string {
	sum := sha256.Sum256([]byte(name))
	readable := strings.ReplaceAll(strings.TrimPrefix(name, "./"), "/", "_")
	return fmt.Sprintf("%s-%s-%x.log", tier, readable, sum[:4])
}
