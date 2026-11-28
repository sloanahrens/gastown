package main

import (
	"bytes"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// treeSweepFake holds what the fakes of one treeSweep were handed, and the
// order of the calls, so a test can read the sequence the sweep ran in.
type treeSweepFake struct {
	made       string
	buildCode  int
	runStatus  int
	calls      []string
	buildDir   string
	buildEnv   []string
	runProgram string
	runDir     string
	runArgs    []string
	runEnv     []string
	removed    []string
}

// seams returns the treeSweep whose every seam is this fake, with environ as
// its process environment.
func (f *treeSweepFake) seams(environ []string) treeSweep {
	return treeSweep{
		Environ: environ,
		MkdirTemp: func(string, string) (string, error) {
			f.calls = append(f.calls, "mkdir")
			return f.made, nil
		},
		RemoveAll: func(path string) error {
			f.calls = append(f.calls, "remove")
			f.removed = append(f.removed, path)
			return nil
		},
		Build: func(dir string, env, _ []string, _ io.Writer, _ time.Duration) int {
			f.calls = append(f.calls, "build")
			f.buildDir, f.buildEnv = dir, env
			return f.buildCode
		},
		Run: func(program string, args []string, dir string, env []string, _, _ io.Writer) int {
			f.calls = append(f.calls, "run")
			f.runProgram, f.runDir, f.runArgs, f.runEnv = program, dir, args, env
			return f.runStatus
		},
	}
}

func TestTreeSweepRunsTheBuiltSweepInTheWorkTreeWithTheOverriddenEnvironment(t *testing.T) {
	t.Parallel()
	const made, work = "/fake/sweep-bin.1", "/fake/work"
	args := sweepArgs(work, []string{"integration", "race"})
	f := &treeSweepFake{made: made}
	var stderr bytes.Buffer
	got := f.seams([]string{"PATH=/bin"}).sweep(args, map[string]string{"TIER_SWEEP_LOGDIR": "/fake/logs"}, io.Discard, &stderr)
	wantBin := filepath.Join(made, "tier-sweep")
	wantEnv := []string{"PATH=/bin", "TIER_SWEEP_LOGDIR=/fake/logs"}
	if got != 0 {
		t.Fatalf("sweep = %d, want the run's status 0; stderr %q", got, stderr.String())
	}
	if f.runProgram != wantBin || f.runDir != work || !slices.Equal(f.runArgs, args) {
		t.Errorf("Run got program %q dir %q args %q, want %q %q %q", f.runProgram, f.runDir, f.runArgs, wantBin, work, args)
	}
	if f.buildDir != work {
		t.Errorf("Build ran in %q, want the work tree %q", f.buildDir, work)
	}
	if !slices.Equal(f.buildEnv, wantEnv) || !slices.Equal(f.runEnv, wantEnv) {
		t.Errorf("build env %q and run env %q, want both %q", f.buildEnv, f.runEnv, wantEnv)
	}
}

func TestTreeSweepReturnsTheRunStatusAndRemovesTheDirectoryAfterTheRun(t *testing.T) {
	t.Parallel()
	const made = "/fake/sweep-bin.1"
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
			f := &treeSweepFake{made: made, runStatus: tc.status}
			got := f.seams(nil).sweep(sweepArgs("/fake/work", nil), nil, io.Discard, io.Discard)
			if got != tc.status {
				t.Errorf("sweep = %d, want the run's status %d", got, tc.status)
			}
			if want := []string{"mkdir", "build", "run", "remove"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q: the directory is removed after the run", f.calls, want)
			}
			if !slices.Equal(f.removed, []string{made}) {
				t.Errorf("RemoveAll paths = %q, want exactly %q", f.removed, made)
			}
		})
	}
}

func TestTreeSweepRefusesAFailedBuildAndRunsNothing(t *testing.T) {
	t.Parallel()
	f := &treeSweepFake{made: "/fake/sweep-bin.2", buildCode: 3}
	var stderr bytes.Buffer
	got := f.seams(nil).sweep(sweepArgs("/fake/work", nil), nil, io.Discard, &stderr)
	if got != 1 {
		t.Errorf("sweep = %d after a failed build, want 1", got)
	}
	if slices.Contains(f.calls, "run") {
		t.Errorf("calls = %q, want no run after a failed build", f.calls)
	}
}

func TestTreeSweepRefusesArgumentsTreeRootRefusesAndBuildsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"first argument not root", []string{"shell", "/fake/work"}},
		{"--root alone", []string{"--root"}},
		{"empty value", []string{"--root", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &treeSweepFake{made: "/fake/sweep-bin.3"}
			var stderr bytes.Buffer
			got := f.seams(nil).sweep(tc.args, nil, io.Discard, &stderr)
			if got != 1 {
				t.Errorf("sweep = %d for arguments %q, want 1", got, tc.args)
			}
			if len(f.calls) != 0 {
				t.Errorf("calls = %q for arguments %q, want none: no directory, build or run", f.calls, tc.args)
			}
		})
	}
}
