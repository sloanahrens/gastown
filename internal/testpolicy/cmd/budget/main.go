// Command budget runs `go test -json <args>` and fails when a converted package
// (one not listed in unconverted.txt) uses more user CPU than the budget.
//
//	go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
//
// It measures CPU by running every test binary through itself: `go test
// -exec "<budget> -exec-test"` makes each binary a child of this command,
// which waits for it and records the CPU time wait4 reports for the binary
// and every descendant it waited for. docs/testing.md ("The time budget")
// explains why the budget is on user CPU and not on wall time.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	list := flag.String("unconverted", "internal/testpolicy/unconverted.txt", "packages exempt from the budget")
	overList := flag.String("overbudget", "internal/testpolicy/overbudget.txt", "converted packages exempt from the budget while a bead tracks their overrun; their times are reported on every run")
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
	root, err := moduleRoot()
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	cpuDir, err := os.MkdirTemp("", "gt-budget-cpu-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(cpuDir)

	// go test splits -exec into words, honoring single and double quotes but
	// no escapes; single quotes keep a path with spaces in one word, and a
	// path with a single quote in it cannot be passed at all.
	if strings.Contains(self, "'") {
		return fail(fmt.Errorf("cannot pass %q to go test -exec: it contains a single quote", self))
	}
	// -exec turns off go test's result cache: a cached package runs no
	// binary, so there would be nothing to measure.
	args := append([]string{"test", "-json", "-exec", "'" + self + "' " + execTestArg}, flag.Args()...)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), testpolicy.CPUDirEnv+"="+cpuDir)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	var readErr error
	cpu := func(pkg string) (testpolicy.CPUTime, bool) {
		c, ok, err := testpolicy.ReadCPU(cpuDir, filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil && readErr == nil {
			readErr = fmt.Errorf("reading CPU time of %s: %w", pkg, err)
		}
		return c, ok
	}
	res, scanErr := testpolicy.WatchBudgetTracked(stdout, os.Stdout, *budget, exempt, tracked, "github.com/steveyegge/gastown", cpu)
	if scanErr != nil {
		// WatchBudget stopped reading before the child was done writing (for
		// example a single line over its 16 MB scan buffer); drain the pipe
		// so the child's next Write doesn't block on a full pipe buffer and
		// hang cmd.Wait() forever.
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()

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
	for _, o := range res.Over {
		if o.Unmeasured {
			fmt.Fprintf(os.Stderr, "BUDGET: %s passed but its CPU time was not recorded (wall %s); the budget cannot judge it\n", o.Package, o.Elapsed.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(os.Stderr, "BUDGET: %s used %s (limit %s user CPU); slowest:", o.Package, usage(o.CPU, false, o.Elapsed), *budget)
		for _, t := range o.Slowest {
			fmt.Fprintf(os.Stderr, " %s %s", t.Name, t.Elapsed.Round(time.Millisecond))
		}
		fmt.Fprintln(os.Stderr)
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		return exitErr.ExitCode()
	case waitErr != nil || scanErr != nil || readErr != nil:
		return fail(errors.Join(waitErr, scanErr, readErr))
	case len(res.Over) > 0:
		return 1
	}
	return 0
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
