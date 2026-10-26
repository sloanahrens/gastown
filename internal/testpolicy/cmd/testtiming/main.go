// Command testtiming measures the unit tier inside a tmux server that launchd
// started, which macOS does not exempt from its scan of new executables — the
// town's condition after a reboot. It prints a probe (ms per new executable;
// a taxed pane shows 50 or more), then the tier's seconds against
// TARGET_SECONDS (default 90) (gt-tpg0p.17).
//
//	make test-timing PKGS=./internal/tmux/... TARGET_SECONDS=5
//
// Only a taxed pane's seconds prove the target, so an exempt pane is a
// distinct failure. The exit code is 0 when the tier passed in a taxed pane
// within the target, the tier's own go test code when it failed, 3 when the
// pane was exempt, 1 when the tier was over the target, and 2 when the
// measurement itself failed.
package main

import (
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

	"github.com/steveyegge/gastown/internal/exectax"
)

const (
	// paneFlag makes the binary run the job inside the tmux pane. The outer
	// process types it into the pane's shell, so it is not a user flag.
	paneFlag = "-pane-run"

	// session is the tmux session the pane runs in, and socketPrefix and
	// labelPrefix keep the server and its launchd job to this run: launchctl
	// names jobs town-wide, so the pid separates concurrent runs.
	session      = "timing"
	socketPrefix = "gt-test-timing-"
	labelPrefix  = "com.gastown.test-timing."

	// launchctlBin starts the tmux server, which is the point: a server that
	// launchd starts has no Developer Tools exemption from the scan.
	launchctlBin = "launchctl"

	// probeFile, logFile and doneFile live in the work directory the outer
	// process and the pane share.
	probeFile = "probe"
	logFile   = "log"
	doneFile  = "done"

	// probeRed is the ms per new executable at or above which the pane pays
	// the exec tax; below it the measurement does not prove the target.
	probeRed = 50
	// probeSamples is how many fresh executables the probe runs. One per
	// package the tier builds, roughly, so the median is a tier-wide cost.
	probeSamples = 20

	// defaultTarget is TARGET_SECONDS' default, the wall the unit tier is
	// judged against.
	defaultTarget = 90

	// readyChecks and readyInterval bound how long the launchd-started server
	// has to answer (50 × 0.2 s).
	readyChecks   = 50
	readyInterval = 200 * time.Millisecond

	// resultChecks and resultInterval bound the tier (900 × 2 s = 30 min).
	resultChecks   = 900
	resultInterval = 2 * time.Second

	// cleanupTimeout bounds the calls that stop the server and its job: they
	// run on an uncancelled context so an interrupt still cleans up.
	cleanupTimeout = 10 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process: it installs the interrupt that must still
// reach the deferred cleanup, then hands off to runWith.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWith(ctx, realHost(), args, stdout, stderr)
}

// runWith is run with its effects injected. The pane runs the same binary
// with paneFlag as the first argument; everything else is the outer process,
// which starts the pane and judges what it left.
func runWith(ctx context.Context, h host, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == paneFlag {
		return runPane(ctx, h, args[1:], stderr)
	}
	return runTiming(ctx, h, args, stdout, stderr)
}

// runTiming starts the launchd-started tmux server, runs the job inside its
// pane, and judges the result.
func runTiming(ctx context.Context, h host, args []string, stdout, stderr io.Writer) int {
	if h.goos != "darwin" {
		fmt.Fprintln(stderr, "test-timing: macOS only")
		return 2
	}
	target, err := targetSeconds(h.getenv)
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v\n", err)
		return 2
	}
	tmuxBin, err := h.lookPath("tmux")
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: tmux: %v\n", err)
		return 2
	}
	exe, err := h.executable()
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v\n", err)
		return 2
	}
	repo, err := h.getwd()
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v\n", err)
		return 2
	}
	work, err := h.tempDir("gt-test-timing-")
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v\n", err)
		return 2
	}
	sock := socketPrefix + strconv.Itoa(h.pid)
	label := labelPrefix + strconv.Itoa(h.pid)
	logPath := filepath.Join(work, logFile)
	donePath := filepath.Join(work, doneFile)
	probePath := filepath.Join(work, probeFile)

	// The server outlives this process, so every exit path stops it; the
	// launchd job is removed here too because KeepAlive would respawn it.
	// The context is uncancelled: an interrupted run still has to clean up.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = h.run(cleanupCtx, command{name: tmuxBin, args: []string{"-L", sock, "kill-server"}})
		_ = h.run(cleanupCtx, command{name: launchctlBin, args: []string{"remove", label}})
	}()

	if err := h.run(ctx, command{name: launchctlBin, args: []string{
		"submit", "-l", label, "--", tmuxBin, "-L", sock, "new-session", "-d", "-s", session,
	}}); err != nil {
		fmt.Fprintf(stderr, "test-timing: launchctl submit: %v\n", err)
		return 2
	}
	for i := range readyChecks {
		if err := h.run(ctx, command{name: tmuxBin, args: []string{"-L", sock, "has-session", "-t", session}}); err == nil {
			break
		}
		if i < readyChecks-1 && ctx.Err() == nil {
			h.sleep(readyInterval)
		}
	}
	// The server daemonized, so the job is no longer needed; leaving it would
	// keep it alive.
	_ = h.run(ctx, command{name: launchctlBin, args: []string{"remove", label}})
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "test-timing: interrupted")
		return 2
	}
	if err := h.run(ctx, command{name: tmuxBin, args: []string{"-L", sock, "has-session", "-t", session}}); err != nil {
		fmt.Fprintf(stderr, "test-timing: the launchd-started tmux server never came up on socket %s\n", sock)
		return 2
	}

	line := paneCommandLine(exe, work, repo, pkgsOrDefault(args))
	if err := h.run(ctx, command{name: tmuxBin, args: []string{"-L", sock, "send-keys", "-t", session, line, "Enter"}}); err != nil {
		fmt.Fprintf(stderr, "test-timing: send-keys: %v\n", err)
		return 2
	}

	if !waitFor(ctx, h, donePath, resultChecks, resultInterval) {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "test-timing: interrupted while waiting for the pane")
			return 2
		}
		fmt.Fprintf(stderr, "test-timing: no result after 30 min; log: %s\n", logPath)
		return 2
	}
	probe, err := readProbe(probePath)
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v; log: %s\n", err, logPath)
		return 2
	}
	rc, secs, err := readDone(donePath)
	if err != nil {
		fmt.Fprintf(stderr, "test-timing: %v; log: %s\n", err, logPath)
		return 2
	}

	fmt.Fprintf(stdout, "probe: %d ms per new executable (taxed pane expected >= %d)\n", probe, probeRed)
	fmt.Fprintf(stdout, "unit tier: %ds (target %ds), go test exit %d; log: %s\n", secs, target, rc, logPath)
	if probe < probeRed {
		fmt.Fprintf(stderr, "test-timing: pane is EXEMPT (probe %d ms); this measurement does not prove the target\n", probe)
		return 3
	}
	if rc != 0 {
		return rc
	}
	if secs > target {
		fmt.Fprintln(stderr, "test-timing: over target")
		return 1
	}
	return 0
}

// paneCommandLine is what send-keys types into the pane's shell: this binary
// running itself in pane mode, with every word quoted against the shell.
func paneCommandLine(exe, work, repo string, pkgs []string) string {
	words := append([]string{exe, paneFlag, "-work", work, "-repo", repo}, pkgs...)
	for i, w := range words {
		words[i] = shellQuote(w)
	}
	return strings.Join(words, " ")
}

// shellQuote quotes w for a POSIX shell: single quotes keep every byte
// literal, and an embedded quote is the four-character close-escape-open.
func shellQuote(w string) string {
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// pkgsOrDefault is the package list the tier runs: the whole module when the
// recipe names no PKGS. A leading "--" is the recipe's go run separator,
// which go passes through as an argument.
func pkgsOrDefault(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return []string{"./..."}
	}
	return args
}

// targetSeconds reads TARGET_SECONDS, the wall the tier is judged against.
func targetSeconds(getenv func(string) string) (int, error) {
	v := getenv("TARGET_SECONDS")
	if v == "" {
		return defaultTarget, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("TARGET_SECONDS=%q is not a number of seconds", v)
	}
	return n, nil
}

// waitFor polls path until it holds something, up to checks tries spaced by
// interval, and reports whether it found it. A canceled context ends the
// wait, so an interrupt does not sit out the remaining tries.
func waitFor(ctx context.Context, h host, path string, checks int, interval time.Duration) bool {
	for i := range checks {
		if ctx.Err() != nil {
			return false
		}
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return true
		}
		if i < checks-1 {
			h.sleep(interval)
		}
	}
	return false
}

// readProbe reads the probe's integer ms per new executable.
func readProbe(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, errors.New("the pane recorded no probe")
	}
	text := strings.TrimSpace(string(b))
	ms, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("the pane's probe %q is not a number of milliseconds", text)
	}
	return ms, nil
}

// readDone reads the "<go test exit> <seconds>" the pane left when it
// finished.
func readDone(path string) (rc, secs int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("reading the pane's result: %w", err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d %d", &rc, &secs); err != nil {
		return 0, 0, fmt.Errorf("the pane's result %q is not \"<exit> <seconds>\"", strings.TrimSpace(string(b)))
	}
	return rc, secs, nil
}

// paneOptions are what the outer process hands the pane: the directory they
// share and the module root the tier runs in.
type paneOptions struct {
	work, repo string
	pkgs       []string
}

// runPane runs the job inside the pane the outer process started.
func runPane(ctx context.Context, h host, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("test-timing "+paneFlag, flag.ContinueOnError)
	fs.SetOutput(stderr)
	work := fs.String("work", "", "directory the job shares with the outer process")
	repo := fs.String("repo", "", "module root the unit tier runs in")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *work == "" || *repo == "" {
		fmt.Fprintln(stderr, "test-timing: "+paneFlag+" needs -work and -repo")
		return 2
	}
	return runPaneJob(ctx, h, paneOptions{work: *work, repo: *repo, pkgs: pkgsOrDefault(fs.Args())}, stderr)
}

// runPaneJob measures the pane's exec tax, runs the tier, and leaves the
// probe and the result for the outer process to judge.
func runPaneJob(ctx context.Context, h host, o paneOptions, stderr io.Writer) int {
	logPath := filepath.Join(o.work, logFile)
	probePath := filepath.Join(o.work, probeFile)
	donePath := filepath.Join(o.work, doneFile)

	// The probe runs before the clock starts, so the tier's seconds are the
	// tier's. A failed probe leaves no probe file: the outer process then
	// reports it rather than judging a run that proves nothing.
	probe, probeErr := measure(ctx, h)

	start := h.now()
	rc := runTier(ctx, h, o, logPath)
	secs := int(h.now().Sub(start).Seconds())

	// The probe's verdict lands in the log the tier just created; writing it
	// before the tier would have the tier's own redirect truncate it.
	if probeErr != nil {
		appendToLog(logPath, fmt.Sprintf("test-timing: probe: %v\n", probeErr))
	} else if err := os.WriteFile(probePath, []byte(strconv.Itoa(probe)), 0o644); err != nil {
		appendToLog(logPath, fmt.Sprintf("test-timing: probe: %v\n", err))
	}

	if err := os.WriteFile(donePath, []byte(fmt.Sprintf("%d %d\n", rc, secs)), 0o644); err != nil {
		fmt.Fprintf(stderr, "test-timing: %v\n", err)
		return 2
	}
	return 0
}

// measure is the probe in this pane: the exec tax every test binary the tier
// builds will pay.
func measure(ctx context.Context, h host) (int, error) {
	res, err := h.probe(ctx)
	if err != nil {
		return 0, err
	}
	return int(res.Median.Milliseconds()), nil
}

// runTier runs the unit tier in this pane and returns go test's exit code, or
// 127 when the runner could not start at all (the shell's code for that).
func runTier(ctx context.Context, h host, o paneOptions, logPath string) int {
	log, err := os.Create(logPath)
	if err != nil {
		return 127
	}
	defer log.Close()
	args := append([]string{"run", "./internal/testpolicy/cmd/budget", "--", "-count=1"}, o.pkgs...)
	// A target is only comparable run to run if the parallelism is fixed, so
	// the tier takes the same -p=8 every time and no containers.
	if err := h.run(ctx, command{
		name:   "go",
		args:   args,
		dir:    o.repo,
		env:    []string{"GT_TEST_DOCKER=0", "GOFLAGS=-p=8"},
		stdout: log,
		stderr: log,
	}); err != nil {
		if code, ok := exitCode(err); ok {
			return code
		}
		fmt.Fprintf(log, "test-timing: %v\n", err)
		return 127
	}
	return 0
}

// exitCode is the exit status a run error carries, for the real runner's
// *exec.ExitError and a test's scripted one alike. A run ended by a signal
// reports -1: it has no verdict, so 128 stands in for it.
func exitCode(err error) (int, bool) {
	var c interface{ ExitCode() int }
	if !errors.As(err, &c) {
		return 0, false
	}
	if code := c.ExitCode(); code >= 0 {
		return code, true
	}
	return 128, true
}

// appendToLog adds a line to the pane's log, whose absence is not fatal: the
// outer process reports the missing result either way.
func appendToLog(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.WriteString(f, line)
}

// command is one program the tool runs, with the stream its output goes to.
type command struct {
	name   string
	args   []string
	dir    string   // "" inherits this process's directory
	env    []string // nil inherits this process's environment
	stdout io.Writer
	stderr io.Writer
}

// host is every effect outside this process: the programs it runs, the
// clock and the waits it polls on, and the probe's execs. Tests substitute
// one, so a unit test starts no process and waits no real time.
type host struct {
	goos       string
	getenv     func(string) string
	lookPath   func(string) (string, error)
	executable func() (string, error)
	getwd      func() (string, error)
	pid        int
	tempDir    func(string) (string, error)
	run        func(context.Context, command) error
	sleep      func(time.Duration)
	now        func() time.Time
	probe      func(context.Context) (exectax.Result, error)
}

// realHost is the host the command runs against.
func realHost() host {
	return host{
		goos:       runtime.GOOS,
		getenv:     os.Getenv,
		lookPath:   exec.LookPath,
		executable: os.Executable,
		getwd:      os.Getwd,
		pid:        os.Getpid(),
		tempDir:    func(pattern string) (string, error) { return os.MkdirTemp("", pattern) },
		run:        runCommand,
		sleep:      time.Sleep,
		now:        time.Now,
		probe: func(ctx context.Context) (exectax.Result, error) {
			return exectax.Probe(ctx, exectax.Options{Samples: probeSamples})
		},
	}
}

// runCommand runs one command; a non-zero exit comes back as an error, the
// exit status included.
func runCommand(ctx context.Context, c command) error {
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Dir = c.dir
	if c.env != nil {
		cmd.Env = append(os.Environ(), c.env...)
	}
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr
	return cmd.Run()
}
