// Package exectax measures what one exec of a freshly written program costs
// in the calling process tree (gt-2ycne.1).
//
// A Mac whose tree is not exempt from the Gatekeeper/XProtect scan, or whose
// process runs at background QoS, pays tens to hundreds of milliseconds per
// new executable where an exempt tree pays single digits. The unit tier links
// a test binary per package, so that cost is paid hundreds of times per gate
// and is visible in no single test. The daemon measures it every heartbeat
// and publishes it in the town health report; the gate measures it before it
// starts (gt-2ycne).
package exectax

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

const (
	// DefaultSamples is how many programs one probe writes and execs.
	DefaultSamples = 10
	// DefaultRed is the median exec cost at or above which a tree is taxed.
	DefaultRed = 50 * time.Millisecond
)

// program is what every sample writes. A probe measures the cost of exec'ing
// a new file, so it must write a new file per sample and the program must do
// nothing but exit.
const program = "#!/bin/sh\nexit 0\n"

// Exec runs one program and reports how long the exec took. It is the seam a
// test substitutes for the real exec.
type Exec func(ctx context.Context, path string) (time.Duration, error)

// Result is one probe's measurement.
type Result struct {
	// Median is the middle sample, the lower middle for an even count.
	Median time.Duration
	// Samples are the individual exec times, in the order taken.
	Samples []time.Duration
}

// Taxed reports whether the tree pays the exec tax: the median at or above
// red. A red of zero never trips, so a settings block that turns the check
// off reads as clear.
func (r Result) Taxed(red time.Duration) bool {
	return red > 0 && r.Median >= red
}

// Options are one probe's inputs; a zero field takes its default.
type Options struct {
	// Samples is how many programs to write and exec; 0 is DefaultSamples.
	Samples int
	// Dir holds the samples; "" writes them to a fresh temporary directory,
	// which Probe removes.
	Dir string
	// Exec is the exec to time; nil runs the real one.
	Exec Exec
}

// Probe writes Samples fresh programs into a temporary directory, execs each
// once and returns their median cost. The programs are written to be exec'd,
// never read, so a probe inside a town leaves nothing behind.
func Probe(ctx context.Context, o Options) (Result, error) {
	n := o.Samples
	if n <= 0 {
		n = DefaultSamples
	}
	run := o.Exec
	if run == nil {
		run = runFile
	}
	dir := o.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "gt-exectax-")
		if err != nil {
			return Result{}, err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	res := Result{Samples: make([]time.Duration, 0, n)}
	for i := range n {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		path := filepath.Join(dir, fmt.Sprintf("sample-%d", i))
		if err := os.WriteFile(path, []byte(program), 0o755); err != nil {
			return Result{}, err
		}
		d, err := run(ctx, path)
		if err != nil {
			return Result{}, fmt.Errorf("exec %s: %w", path, err)
		}
		res.Samples = append(res.Samples, d)
	}
	res.Median = median(res.Samples)
	return res, nil
}

// runFile is the real exec: the sample path is absolute, so the kernel runs
// the file itself rather than a PATH lookup.
func runFile(ctx context.Context, path string) (time.Duration, error) {
	start := time.Now()
	if err := exec.CommandContext(ctx, path).Run(); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// median returns the middle sample (the lower middle for an even count).
func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)/2]
}

// State is the exec-tax state one health report carries.
type State struct {
	// Known is false when the report carried no measurement.
	Known bool
	// Taxed is the median at or above the report's threshold.
	Taxed bool
	// Median is the measured cost, zero when unknown.
	Median time.Duration
}

// StateOf is the state a report's exec-tax field and raw milliseconds make:
// unknown when no measurement was published, taxed when the millisecond
// count is at or above red.
func StateOf(ms *float64, red time.Duration) State {
	if ms == nil || *ms < 0 {
		return State{}
	}
	m := time.Duration(*ms * float64(time.Millisecond))
	return State{Known: true, Taxed: red > 0 && m >= red, Median: m}
}

// Transition returns the one line to log when the state changed from prev to
// now, and "" when it did not: an unchanged state, or a first measurement
// with no baseline to compare against. The daemon logs the line, so a town
// that stays taxed says so once per change rather than once per heartbeat.
func Transition(prev, now State) string {
	if !now.Known || !prev.Known || prev.Taxed == now.Taxed {
		return ""
	}
	if now.Taxed {
		return fmt.Sprintf("exec-tax: %s/exec, over the threshold: every fresh executable in this tree pays it (gt-2ycne.1)", round(now.Median))
	}
	return fmt.Sprintf("exec-tax: clear again at %s/exec", round(now.Median))
}

// round trims a measurement to the millisecond, the unit the field reports.
func round(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
