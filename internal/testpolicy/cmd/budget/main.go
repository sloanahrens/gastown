// Command budget runs go test over the module's packages and fails when a
// converted package (one not listed in unconverted.txt) uses more user CPU
// than the budget.
//
//	go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
//
// It measures CPU by running every converted package's test binary through
// itself: `go test -json -exec "<budget> -exec-test"` makes each binary a
// child of this command, which waits for it and records the CPU time wait4
// reports for the binary and every descendant it waited for. docs/testing.md
// ("The time budget") explains why the budget is on user CPU and not on wall
// time.
//
// -exec turns off go test's result cache (a cached package runs no binary,
// so there would be nothing to measure), and the budget judges nothing in
// an unconverted package, so the runner splits the package list: the
// converted packages run first through the exec wrapper, then the
// unconverted ones through plain go test, cache and all, with the same
// arguments. The two halves run side by side (gt-qe4b0): run one after the
// other, the unconverted half added its whole wall to every gate, 14-41 s.
// They share go test's -p cap (GOMAXPROCS unless the arguments set it) so
// the host sees no more test processes than one go test would start: the
// unconverted half gets as many slots as it has packages, at most half the
// cap, and the converted half the rest. The unconverted half's output is held
// and printed after the converted half's, so the two never interleave. The
// budget report comes after both, and either half failing fails the run.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

// execTestArg is the first argument `go test -exec` passes back to this
// command, before the test binary and its arguments.
const execTestArg = "-exec-test"

func main() {
	if len(os.Args) > 1 && os.Args[1] == execTestArg {
		os.Exit(execTest(os.Args[2:]))
	}
	os.Exit(run())
}

// run runs the budgeted go test and returns the exit code: go test's own when
// it fails, 1 when only the budget fails, 2 when the runner itself fails.
func run() int {
	budget := flag.Duration("budget", 10*time.Second, "per-package user CPU limit for converted packages (the test binary and the processes it waited for)")
	list := flag.String("unconverted", "internal/testpolicy/unconverted.txt", "packages exempt from the budget; they run through plain go test, with its result cache")
	overList := flag.String("overbudget", "internal/testpolicy/overbudget.txt", "converted packages exempt from the budget while a bead tracks their overrun; their times are reported on every run")
	fastTier := flag.Bool("fast-tier", false, "check the unit tier's wall time (make gate): warn about any package that ran longer than testpolicy.FastTierMaxWall")
	strictWall := flag.Bool("strict-wall", false, "with -fast-tier, fail a package over testpolicy.FastTierMaxWall of wall time instead of warning (make tier-check)")
	flag.Parse()

	exempt, err := testpolicy.ReadList(*list)
	if err != nil {
		return fail(err)
	}
	overEntries, err := testpolicy.ReadOverBudget(*overList)
	if err != nil {
		return fail(err)
	}
	tracked := make(map[string]string, len(overEntries))
	for _, e := range overEntries {
		tracked[e.Package] = e.Bead
	}
	if *strictWall && !*fastTier {
		return fail(errors.New("-strict-wall needs -fast-tier"))
	}
	root, err := moduleRoot()
	if err != nil {
		return fail(err)
	}
	// The load is read before go test adds its own, so it measures what
	// the run competes with.
	load1, loadErr := hostLoad1()
	enforce, why := testpolicy.EnforceBudget(load1, loadErr == nil, runtime.NumCPU(), os.Getenv(testpolicy.StrictBudgetEnv) == "1")
	if enforce {
		fmt.Fprintf(os.Stderr, "budget: user-CPU budget enforced (%s)\n", why)
	} else {
		fmt.Fprintf(os.Stderr, "budget: user-CPU budget reported only (%s); %s=1 enforces it\n", why, testpolicy.StrictBudgetEnv)
	}

	before, patterns, after := testpolicy.SplitTestArgs(flag.Args())
	pkgs, err := listPackages(testpolicy.TagsArgs(flag.Args()), patterns)
	if err != nil {
		return fail(err)
	}
	judged, cached := testpolicy.PartitionPackages(pkgs, module, exempt)
	if len(pkgs) == 0 {
		// Nothing matched: let go test say so, in one budgeted run over
		// the arguments as given.
		judged = patterns
	}
	judgedP, cachedP := splitParallelism(parallelism(before, after), len(cached))
	withPkgs := func(procs int, p []string) []string {
		// The last -p wins, so this overrides one in the arguments.
		b := append(append([]string{}, before...), "-p", strconv.Itoa(procs))
		return append(append(b, p...), after...)
	}

	// Both halves print plain go test text; a copy of its package summary
	// lines feeds the -fast-tier wall check after the run.
	var summary bytes.Buffer
	probe := &summaryLines{w: &summary}
	out := io.Writer(os.Stdout)
	if *fastTier {
		out = io.MultiWriter(os.Stdout, probe)
	}

	var res testpolicy.BudgetResult
	var judgedHalf, cachedHalf half
	if len(judged) > 0 {
		judgedHalf = func(_ context.Context, out, errOut io.Writer) (int, error) {
			start := time.Now()
			r, code, err := runJudged(withPkgs(judgedP, judged), root, *budget, exempt, tracked, out)
			res = r
			fmt.Fprintf(errOut, "budget: %d converted packages took %s\n", len(judged), time.Since(start).Round(time.Second))
			return code, err
		}
	}
	if len(cached) > 0 {
		cachedHalf = func(ctx context.Context, out, errOut io.Writer) (int, error) {
			start := time.Now()
			code, err := runCached(ctx, withPkgs(cachedP, cached), out, errOut)
			fmt.Fprintf(errOut, "budget: %d unconverted packages took %s\n", len(cached), time.Since(start).Round(time.Second))
			return code, err
		}
	}
	code, err := runHalves(judgedHalf, cachedHalf, out, os.Stderr)
	if err != nil {
		return fail(err)
	}

	if len(res.SlowWall) > 0 {
		fmt.Fprintf(os.Stderr, "over %s of wall time (reported only; the budget is on user CPU):\n", *budget)
		for _, r := range res.SlowWall {
			fmt.Fprintf(os.Stderr, "  %s used %s\n", r.Package, usage(r.CPU, false, r.Elapsed))
		}
	}
	if len(res.Tracked) > 0 {
		fmt.Fprintf(os.Stderr, "over budget (tracked, limit %s user CPU):\n", *budget)
		for _, r := range res.Tracked {
			fmt.Fprintf(os.Stderr, "  %s used %s (%s)\n", r.Package, usage(r.CPU, r.Unmeasured, r.Elapsed), r.Bead)
		}
	}
	if reportOver(os.Stderr, res.Over, *budget, enforce, why) && code == 0 {
		code = 1
	}
	if *fastTier && !interrupted(code) {
		if c := checkFastTier(probe, &summary, len(judged)+len(cached), *strictWall); code == 0 {
			code = c
		}
		reportIdle(res.Judged)
	}
	return code
}

// reportIdle prints the gate's "packages mostly waiting" warning. A tier
// whose packages ran long without computing was waiting on the host -- a
// macOS scan per executable, a throttled process, a lock -- and its wall time
// says nothing about the tree (gt-2ycne.1). The warning fails nothing: make
// gate is judged by its exit code, and a loaded host is not a defect in the
// branch under test.
func reportIdle(times []testpolicy.PkgTime) {
	rep := testpolicy.IdleJudged(times)
	if !rep.Waiting {
		return
	}
	top := make([]string, 0, len(rep.Top))
	for _, p := range rep.Top {
		top = append(top, fmt.Sprintf("%s %s wall / %s CPU", p.Package, round(p.Wall), round(p.CPU.User+p.CPU.Sys)))
	}
	fmt.Fprintf(os.Stderr, "gate: WARNING packages mostly waiting, not computing: the median of %d judged packages waited %s of wall for %s of CPU; top %d by wall: %s\n",
		rep.Packages, round(rep.Wall), round(rep.CPU), len(top), strings.Join(top, ", "))
}

// round trims a duration to the unit that reads best in one line.
func round(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}

// reportOver writes a BUDGET: line for each overrun to w and reports
// whether they fail the run (testpolicy.BudgetFails). When the budget is not
// enforced, a CPU overrun's line says it is reported only, and why.
func reportOver(w io.Writer, over []testpolicy.Overrun, budget time.Duration, enforce bool, why string) bool {
	label := "BUDGET:"
	if !enforce {
		label = fmt.Sprintf("BUDGET (reported only, %s; %s=1 enforces):", why, testpolicy.StrictBudgetEnv)
	}
	for _, o := range over {
		if o.Unmeasured {
			fmt.Fprintf(w, "BUDGET: %s passed but its CPU time was not recorded (wall %s); the budget cannot judge it\n", o.Package, o.Elapsed.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(w, "%s %s used %s (limit %s user CPU); slowest:", label, o.Package, usage(o.CPU, false, o.Elapsed), budget)
		for _, t := range o.Slowest {
			fmt.Fprintf(w, " %s %s", t.Name, t.Elapsed.Round(time.Millisecond))
		}
		fmt.Fprintln(w)
	}
	return testpolicy.BudgetFails(over, enforce)
}

// checkFastTier reads the package summary lines the probe kept. A package
// that ran longer than testpolicy.FastTierMaxWall is named on a TIER: line,
// and the check returns 1 for it only when strictWall is set. Wall time
// depends on host load (a package measured at 13.9 s took 37.7 s beside six
// other gates), and a landing must not be refused for contention, so `make
// gate` only warns and `make tier-check` fails. Output it cannot read fails
// closed either way: a run whose output yields no wall time and no cached
// package, or a summary line it cannot read, returns 1, because silence there
// would read as every package being fast.
func checkFastTier(probe *summaryLines, summary io.Reader, ran int, strictWall bool) int {
	if err := probe.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "TIER: reading package summaries:", err)
		return 1
	}
	sum, err := testpolicy.ParsePackageWalls(summary, module)
	if err != nil {
		fmt.Fprintln(os.Stderr, "TIER: reading package wall times:", err)
		return 1
	}
	code := 0
	for _, line := range sum.Unparsed {
		fmt.Fprintf(os.Stderr, "TIER: cannot read the wall time in go test's summary line %q; the fast-tier check fails closed\n", line)
		code = 1
	}
	if ran > 0 && len(sum.Walls) == 0 && sum.Cached == 0 {
		fmt.Fprintf(os.Stderr, "TIER: %d packages ran but the wall-time probe parsed no package summary line (ok/FAIL <package> <time>); has go test's output format changed? The fast-tier check fails closed\n", ran)
		code = 1
	}
	for _, o := range testpolicy.WallOverruns(sum.Walls, testpolicy.FastTierMaxWall) {
		note := "warning only, since wall time depends on host load and make gate never fails on it; make tier-check does"
		if strictWall {
			note = "failing: make tier-check judges wall time"
		}
		fmt.Fprintf(os.Stderr, "TIER: %s took %s of wall time, over the unit tier's %s per package (%s). Make its tests faster: there is no slow tier to move it to (gt-ik4a1.9)\n",
			o.Package, o.Wall.Round(100*time.Millisecond), testpolicy.FastTierMaxWall, note)
		if strictWall {
			code = 1
		}
	}
	return code
}

// maxSummaryLine bounds the partial line summaryLines holds. A package
// summary line is an import path and a time, far shorter; a longer line is
// test output and is dropped unread.
const maxSummaryLine = 64 * 1024

// summaryLines keeps only the package summary lines ("ok", "FAIL") of the go
// test text written to it, so a failing package's full output is not held
// twice in memory. Flush passes on a last line that had no newline.
type summaryLines struct {
	w       io.Writer
	partial []byte
	// skipping is set while the rest of an over-long line is dropped.
	skipping bool
}

func (s *summaryLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !s.skipping {
				s.partial = append(s.partial, p...)
				if len(s.partial) > maxSummaryLine {
					s.partial, s.skipping = s.partial[:0], true
				}
			}
			break
		}
		if s.skipping || len(s.partial)+i+1 > maxSummaryLine {
			s.skipping = false
		} else {
			s.partial = append(s.partial, p[:i+1]...)
			if err := s.emit(); err != nil {
				return 0, err
			}
		}
		s.partial = s.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

// Flush passes on a final line that ended without a newline.
func (s *summaryLines) Flush() error {
	defer func() { s.partial, s.skipping = s.partial[:0], false }()
	if s.skipping || len(s.partial) == 0 {
		return nil
	}
	s.partial = append(s.partial, '\n')
	return s.emit()
}

func (s *summaryLines) emit() error {
	if bytes.HasPrefix(s.partial, []byte("ok ")) || bytes.HasPrefix(s.partial, []byte("FAIL\t")) {
		_, err := s.w.Write(s.partial)
		return err
	}
	return nil
}

// module is the main module's path; package names in unconverted.txt and
// overbudget.txt are relative to it.
const module = "github.com/steveyegge/gastown"

// interrupted reports whether go test's exit code says a signal ended it
// (exec.ExitError.ExitCode is -1 then): the run is being stopped, so the
// second half must not start.
func interrupted(code int) bool { return code < 0 }

// listPackages resolves go test's package patterns to import paths with go
// list, under the same build tags. -e keeps packages that fail to load in
// the list, so go test reports their errors as it would have.
func listPackages(tags, patterns []string) ([]string, error) {
	args := append(append([]string{"list", "-e", "-f", "{{.ImportPath}}"}, tags...), patterns...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", strings.Join(patterns, " "), err)
	}
	return strings.Fields(string(out)), nil
}

// half runs go test over one half of the package list, writing go test's
// output to out and errOut, and returns go test's exit code. The error is for
// the runner's own failures; ctx is canceled when the other half has one.
type half func(ctx context.Context, out, errOut io.Writer) (int, error)

// runHalves runs the converted half (judged) and the unconverted half
// (cached) at the same time; either may be nil. The judged half writes to out
// and errOut as it goes; the cached half's output is held and written after
// the judged half's, so the two never interleave. The exit code is the
// judged half's when it is not 0 (a negative one, a signal, included), else
// the cached half's. A runner error in either half is returned; a judged one,
// or a signal ending the judged half, cancels the cached half first.
func runHalves(judged, cached half, out, errOut io.Writer) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cachedOut, cachedErrOut bytes.Buffer
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	if cached != nil {
		go func() {
			c, err := cached(ctx, &cachedOut, &cachedErrOut)
			done <- result{c, err}
		}()
	} else {
		done <- result{}
	}
	code := 0
	var judgedErr error
	if judged != nil {
		code, judgedErr = judged(ctx, out, errOut)
		if judgedErr != nil || interrupted(code) {
			cancel()
		}
	}
	r := <-done
	if _, err := out.Write(cachedOut.Bytes()); err != nil && judgedErr == nil {
		judgedErr = err
	}
	if _, err := errOut.Write(cachedErrOut.Bytes()); err != nil && judgedErr == nil {
		judgedErr = err
	}
	if err := errors.Join(judgedErr, r.err); err != nil {
		return 0, err
	}
	if code == 0 {
		code = r.code
	}
	return code, nil
}

// parallelism is go test's -p cap for the run: the last -p in its arguments,
// else GOMAXPROCS, go test's default.
func parallelism(before, after []string) int {
	p := runtime.GOMAXPROCS(0)
	args := append(append([]string{}, before...), after...)
	for i, a := range args {
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "p" {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
		}
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			p = n
		}
	}
	return p
}

// splitParallelism shares the -p cap between the halves: the cached half
// gets one slot per package, at most half the cap, and the judged half the
// rest. Each gets at least one, so a cap of 1 runs them at 1 each.
func splitParallelism(procs, cachedPkgs int) (judgedP, cachedP int) {
	cachedP = max(1, min(cachedPkgs, procs/2))
	return max(1, procs-cachedP), cachedP
}

// runCached runs plain go test (no -json, no -exec, so its result cache
// applies) and returns its exit code. Canceling ctx interrupts go test,
// which stops its test binaries in turn.
func runCached(ctx context.Context, args []string, out, errOut io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"test"}, args...)...)
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	return exitCode(cmd.Run())
}

// exitCode is the exit code of a command that ran, or the error when it did
// not run at all.
func exitCode(err error) (int, error) {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), nil
	default:
		return 0, err
	}
}

// runJudged runs go test -json over the converted packages with every test
// binary under this command's exec wrapper, copies plain go test text to
// stdout, and returns what the budget found and go test's exit code. The
// error is for the runner's own failures.
func runJudged(goTestArgs []string, root string, budget time.Duration, exempt map[string]bool, tracked map[string]string, out io.Writer) (testpolicy.BudgetResult, int, error) {
	var res testpolicy.BudgetResult
	self, err := os.Executable()
	if err != nil {
		return res, 0, err
	}
	cpuDir, err := os.MkdirTemp("", "gt-budget-cpu-")
	if err != nil {
		return res, 0, err
	}
	defer os.RemoveAll(cpuDir)

	// go test splits -exec into words, honoring single and double quotes but
	// no escapes; single quotes keep a path with spaces in one word, and a
	// path with a single quote in it cannot be passed at all.
	if strings.Contains(self, "'") {
		return res, 0, fmt.Errorf("cannot pass %q to go test -exec: it contains a single quote", self)
	}
	// -exec turns off go test's result cache: a cached package runs no
	// binary, so there would be nothing to measure.
	args := append([]string{"test", "-json", "-exec", "'" + self + "' " + execTestArg}, goTestArgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), testpolicy.CPUDirEnv+"="+cpuDir)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, 0, err
	}
	if err := cmd.Start(); err != nil {
		return res, 0, err
	}
	var readErr error
	cpu := func(pkg string) (testpolicy.CPUTime, bool) {
		c, ok, err := testpolicy.ReadCPU(cpuDir, filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil && readErr == nil {
			readErr = fmt.Errorf("reading CPU time of %s: %w", pkg, err)
		}
		return c, ok
	}
	res, scanErr := testpolicy.WatchBudgetTracked(stdout, out, budget, exempt, tracked, module, cpu)
	if scanErr != nil {
		// WatchBudget stopped reading before the child was done writing (for
		// example a single line over its 16 MB scan buffer); drain the pipe
		// so the child's next Write doesn't block on a full pipe buffer and
		// hang cmd.Wait() forever.
		_, _ = io.Copy(io.Discard, stdout)
	}
	code, waitErr := exitCode(cmd.Wait())
	if code != 0 {
		return res, code, nil
	}
	if err := errors.Join(waitErr, scanErr, readErr); err != nil {
		return res, 0, err
	}
	return res, 0, nil
}

func usage(c testpolicy.CPUTime, unmeasured bool, wall time.Duration) string {
	w := wall.Round(time.Millisecond)
	if unmeasured {
		return fmt.Sprintf("unmeasured CPU, wall %s", w)
	}
	return fmt.Sprintf("%s user CPU (sys %s, wall %s)", c.User.Round(time.Millisecond), c.Sys.Round(time.Millisecond), w)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "budget:", err)
	return 2
}

// moduleRoot is the directory of the main module's go.mod: WatchBudget names
// packages relative to it, and go test runs each test binary in its package
// directory under it.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("not inside a Go module")
	}
	return filepath.Dir(gomod), nil
}

// execTest runs one test binary (argv) for `go test -exec`, passing its
// stdio and exit status through, and records the CPU time the binary and the
// descendants it waited for used. It returns the exit code for go test.
func execTest(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "budget "+execTestArg+": no test binary")
		return 2
	}
	cpuDir := os.Getenv(testpolicy.CPUDirEnv)
	// go test signals this process, not the binary: SIGQUIT when -timeout
	// runs out (for the goroutine dump), SIGINT/SIGTERM when interrupted.
	// Pass each one on; this process exits when the binary does.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	cmd, err := startTestBinary(func() *exec.Cmd {
		c := exec.Command(argv[0], argv[1:]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		c.Env = withoutEnv(os.Environ(), testpolicy.CPUDirEnv)
		return c
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget "+execTestArg+":", err)
		return 2
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	_ = cmd.Wait()
	signal.Stop(sigs)
	st := cmd.ProcessState
	if cpuDir == "" {
		fmt.Fprintln(os.Stderr, "budget "+execTestArg+": "+testpolicy.CPUDirEnv+" is not set; CPU time not recorded")
	} else if wd, err := os.Getwd(); err != nil {
		fmt.Fprintln(os.Stderr, "budget "+execTestArg+": CPU time not recorded:", err)
	} else if err := testpolicy.WriteCPU(cpuDir, wd, testpolicy.CPUTime{User: st.UserTime(), Sys: st.SystemTime()}); err != nil {
		fmt.Fprintln(os.Stderr, "budget "+execTestArg+": CPU time not recorded:", err)
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return dieLike(ws.Signal())
	}
	return st.ExitCode()
}

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// startTestBinary starts the command newCmd builds, retrying while exec fails
// with ETXTBSY: go test has just written the test binary, and a descriptor
// for it that another process inherited across fork (before that process's
// own exec closed it) keeps it "busy" for a moment. go test retries the same
// error when it runs a test binary itself (go.dev/issue/22315). Each attempt waits twice as
// long as the last, about 5 s in all; newCmd is called per attempt because an
// exec.Cmd cannot be started twice.
func startTestBinary(newCmd func() *exec.Cmd) (*exec.Cmd, error) {
	wait := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		cmd := newCmd()
		err := cmd.Start()
		if err == nil || !errors.Is(err, syscall.ETXTBSY) || attempt == 9 {
			return cmd, err
		}
		time.Sleep(wait)
		wait *= 2
	}
}
