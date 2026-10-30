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

// Runner runs one tier's packages one at a time, so no single go test outlives
// its timeout and a red package is named on its own.
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
	// Timings receives one line per package the runner starts, in run order. A
	// package it skips, refuses or cannot log writes none. Nil writes no
	// timings.
	Timings *Timings
	// Now is the clock a package's wall time is read from. Nil means time.Now.
	Now func() time.Time
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
func (r *Runner) Tier(tier, dir string, prefix, args, pkgs []string) Counts {
	var c Counts
	for _, pkg := range pkgs {
		name := packageName(dir, pkg)
		if strings.HasPrefix(pkg, "-") {
			// go test would read it as a flag, so it fails closed and never runs.
			c.Failed++
			c.FailedNames = append(c.FailedNames, name)
			fmt.Fprintf(r.Stderr, "%s: package argument starts with a dash\n", name)
			continue
		}
		whole, pattern := SkipFor(r.SkipList, tier, name)
		if whole {
			c.Skipped++
			fmt.Fprintf(r.Stderr, "%s: %s %s SKIPPED (%s)\n", Name, tier, name, EnvSkip)
			continue
		}
		fmt.Fprintf(r.Stderr, "%s: %s %s ...\n", Name, tier, name)
		log := filepath.Join(r.LogDir, LogName(tier, name))
		f, err := os.Create(log)
		if err != nil {
			// Without a log the package cannot be run, and it is not passed.
			c.Failed++
			c.FailedNames = append(c.FailedNames, name)
			fmt.Fprintf(r.Stderr, "    create log %s: %v\n", log, err)
			continue
		}
		argv := append(append([]string{}, prefix...), "go", "test", "-count=1", "-timeout", r.Timeout)
		argv = append(argv, args...)
		if pattern != "" {
			argv = append(argv, "-skip", pattern)
		}
		argv = append(argv, pkg)
		start := r.now()
		code := r.Exec(dir, r.Env, argv, f)
		r.addTiming(tier, name, start, code)
		closeErr := f.Close()
		if code == 0 && closeErr == nil {
			c.Passed++
			continue
		}
		c.Failed++
		c.FailedNames = append(c.FailedNames, name)
		if closeErr != nil {
			fmt.Fprintf(r.Stderr, "    close log %s: %v\n", log, closeErr)
		}
		if err := r.excerpt(log); err != nil {
			fmt.Fprintf(r.Stderr, "    read log %s: %v\n", log, err)
		}
	}
	return c
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
