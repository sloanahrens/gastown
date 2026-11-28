package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestTreeRunStartsTheBinWithTheWorkArgsAndEnvUnchanged(t *testing.T) {
	t.Parallel()
	const bin, work = "/fake/sweep-bin.1/tier-sweep", "/fake/work"
	args := []string{"integration", "race"}
	env := []string{"PATH=/bin", "TIER_SWEEP_LOGDIR=/fake/logs"}
	var gotProgram, gotDir string
	var gotArgs, gotEnv []string
	ts := treeSweep{
		Run: func(program string, a []string, dir string, e []string, _, _ io.Writer) int {
			gotProgram, gotDir, gotArgs, gotEnv = program, dir, a, e
			return 0
		},
	}
	var stderr bytes.Buffer
	ts.runBuilt(bin, work, args, env, io.Discard, &stderr)
	if gotProgram != bin || gotDir != work || !slices.Equal(gotArgs, args) {
		t.Errorf("Run got program %q dir %q args %q, want %q %q %q", gotProgram, gotDir, gotArgs, bin, work, args)
	}
	if !slices.Equal(gotEnv, env) {
		t.Errorf("Run got %d environment entries, want the %d it was given unchanged", len(gotEnv), len(env))
	}
}

func TestTreeRunWritesTheNamingLineBeforeTheRunStarts(t *testing.T) {
	t.Parallel()
	const bin = "/fake/sweep-bin.1/tier-sweep"
	var stderr bytes.Buffer
	var atStart string
	ts := treeSweep{
		Run: func(string, []string, string, []string, io.Writer, io.Writer) int {
			atStart = stderr.String()
			return 0
		},
	}
	ts.runBuilt(bin, "/fake/work", nil, nil, io.Discard, &stderr)
	if strings.Count(atStart, "\n") != 1 || !strings.HasPrefix(atStart, "sweep-run: ") || !strings.Contains(atStart, bin) {
		t.Errorf("stderr when Run started = %q, want one sweep-run: line naming %q", atStart, bin)
	}
}

func TestTreeRunReturnsTheStatusTheRunReports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"zero", 0},
		{"positive", 3},
		{"negative", sweepNoExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := treeSweep{
				Run: func(string, []string, string, []string, io.Writer, io.Writer) int { return tc.status },
			}
			if got := ts.runBuilt("/fake/bin", "/fake/work", nil, nil, io.Discard, io.Discard); got != tc.status {
				t.Errorf("runBuilt = %d, want %d", got, tc.status)
			}
		})
	}
}
