package shelltier

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Command is one script invocation the runner hands to Exec.
type Command struct {
	// Dir is the directory the command runs in: the repo root.
	Dir string
	// Argv is the command line: `bash <path>` for a script.
	Argv []string
	// Env holds the variables the script adds to the environment it inherits.
	Env []string
	// Log is the file the combined standard output and error go to.
	Log string
}

// Exec runs one command and reports its exit code. A non-nil error means the
// command did not finish with an exit code: it could not start, or its context ended.
type Exec func(ctx context.Context, c Command) (exitCode int, err error)

// Config is the input to Run. Every field is required except Jobs, whose zero
// value runs one worker, and the timing log's: with no Timing or an empty
// TimingPath, the run records nothing and its result is the same.
type Config struct {
	Entries []Entry
	// Root is the directory each script runs in.
	Root string
	// LogDir holds each script's log and its TMPDIR.
	LogDir string
	// Budget bounds each script on its own.
	Budget      time.Duration
	Exec        Exec
	Now         func() time.Time
	WithTimeout func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
	// Jobs is the number of Parallel entries run at once. Below one, one
	// worker runs, so the pool never asks for no workers.
	Jobs int
	// Timing appends each script's wall time to TimingPath. Nil records none.
	Timing TimingFiles
	// TimingPath is the log Timing appends to. Empty records none.
	TimingPath string
	// Err receives the runner's diagnostics: a timing line that could not be
	// appended, and each script a canceled run never started. Nil sends them
	// to the process's error stream.
	Err io.Writer
}

// Result counts the outcomes of one run.
type Result struct {
	Passed, Failed, Skipped int
	// TimedOut counts the failed scripts whose own Budget ran out. A script the
	// parent context ended is failed but not timed out.
	TimedOut int
	// FailedNames are the paths of the failed scripts, in manifest order.
	FailedNames []string
	Elapsed     time.Duration
}

// outcome is what one script's run reports.
type outcome struct {
	// passed is whether the script passed; timedOut whether its own Budget ran out.
	passed, timedOut bool
	// exit is the code Exec reported for a script that finished on its own, or
	// -1 when it reported none or the script's context ended: the command could
	// not start, or it was cut short.
	exit int
	// began and ended bound the script's wall time.
	began, ended time.Time
}

// Run runs each entry as `bash <path>` and counts the outcomes. The entries
// with Mode Parallel run through at most Jobs workers; once that pool has
// drained, the Serial entries run one at a time. A script passes on exit 0. A
// non-zero exit, an Exec error or a Budget timeout fails it, and the run goes
// on: one failure cancels nothing. Results, failed names and timing lines keep
// manifest order whatever the finish order. Run creates no directories: Exec
// creates LogDir and each TMPDIR before it runs a command.
//
// Once ctx ends, no further script is started: the one it interrupts and every
// script after it fail, none counted as timed out, and each one that never ran
// is named on Config.Err. A script that ran at all gets a line in the timing
// log (Config.Timing); one that never started does not.
func Run(ctx context.Context, cfg Config) Result {
	start := cfg.Now()
	outs := make([]outcome, len(cfg.Entries))
	ran := make([]bool, len(cfg.Entries))

	var parallel, serial []int
	for i, e := range cfg.Entries {
		if e.Mode == Parallel {
			parallel = append(parallel, i)
		} else {
			serial = append(serial, i)
		}
	}

	runPool(ctx, cfg, parallel, outs, ran)
	for _, i := range serial {
		if ctx.Err() != nil {
			continue
		}
		outs[i] = runOne(ctx, cfg, cfg.Entries[i])
		ran[i] = true
	}

	var r Result
	for i, e := range cfg.Entries {
		if ran[i] {
			recordTiming(cfg, e, outs[i])
		} else {
			fmt.Fprintf(errorStream(cfg.Err), "shelltier: %s: not run, the run's context ended\n", e.Path)
		}
		if outs[i].passed {
			r.Passed++
			continue
		}
		r.Failed++
		if outs[i].timedOut {
			r.TimedOut++
		}
		r.FailedNames = append(r.FailedNames, e.Path)
	}
	r.Elapsed = cfg.Now().Sub(start)
	return r
}

// runPool runs the entries at idx through at most cfg.Jobs workers. A worker
// starts a script only while ctx is live; one it never starts stays unrun, for
// Run to count and to name.
//
// Each worker writes only the index it was handed, and Run reads the outcomes
// after the pool has drained, so the outcomes need no lock.
func runPool(ctx context.Context, cfg Config, idx []int, outs []outcome, ran []bool) {
	workers := jobs(cfg.Jobs)
	if workers > len(idx) {
		workers = len(idx)
	}
	if workers == 0 {
		return
	}
	work := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil {
					continue
				}
				outs[i] = runOne(ctx, cfg, cfg.Entries[i])
				ran[i] = true
			}
		}()
	}
	for _, i := range idx {
		work <- i
	}
	close(work)
	wg.Wait()
}

// jobs is the number of workers a pool runs, never below one.
func jobs(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// runOne runs one entry under its own Budget and reports how it went. A script
// whose context ended fails even when Exec reports success; it is timed out
// only when the parent context is still live, so a canceled parent is not
// counted as a budget expiry.
func runOne(ctx context.Context, cfg Config, e Entry) outcome {
	name := logName(e.Path)
	sctx, cancel := cfg.WithTimeout(ctx, cfg.Budget)
	defer cancel()
	began := cfg.Now()
	code, err := cfg.Exec(sctx, Command{
		Dir:  cfg.Root,
		Argv: []string{"bash", e.Path},
		Env: []string{
			"GT_TEST_DOCKER=0",
			"TMPDIR=" + filepath.Join(cfg.LogDir, "tmp", name),
		},
		Log: filepath.Join(cfg.LogDir, name+".log"),
	})
	ended := cfg.Now()
	cut := sctx.Err() != nil
	exit := code
	if err != nil || cut {
		exit = -1
	}
	return outcome{
		passed:   !cut && err == nil && code == 0,
		timedOut: cut && ctx.Err() == nil,
		exit:     exit,
		began:    began,
		ended:    ended,
	}
}

// logName is the stem of a script's log file and TMPDIR: its path with each
// slash replaced by an underscore, the names the tier sweep writes.
func logName(path string) string {
	return "shell-" + strings.ReplaceAll(path, "/", "_")
}
