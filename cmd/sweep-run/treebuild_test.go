package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// treeBuildCall holds what a fake Build was handed.
type treeBuildCall struct {
	dir     string
	env     []string
	argv    []string
	timeout time.Duration
}

func TestTreeBuildBuildsTheTreesSweepCommandIntoTheTempDirectory(t *testing.T) {
	t.Parallel()
	const made, work = "/fake/sweep-bin.1", "/fake/work"
	env := []string{"PATH=/bin"}
	var got treeBuildCall
	var removed []string
	ts := treeSweep{
		MkdirTemp: func(string, string) (string, error) { return made, nil },
		RemoveAll: func(path string) error { removed = append(removed, path); return nil },
		Build: func(dir string, env, argv []string, out io.Writer, timeout time.Duration) int {
			got = treeBuildCall{dir: dir, env: env, argv: argv, timeout: timeout}
			fmt.Fprint(out, "build output\n")
			return 0
		},
	}
	var stderr bytes.Buffer
	bin, remove, ok := ts.build(work, env, &stderr)
	want := filepath.Join(made, "tier-sweep")
	if !ok || bin != want {
		t.Fatalf("build = (%q, ok %v), want (%q, true), stderr %q", bin, ok, want, stderr.String())
	}
	if got.dir != work || !slices.Equal(got.env, env) {
		t.Errorf("Build ran in %q with %d env entries, want %q with %d", got.dir, len(got.env), work, len(env))
	}
	if wantArgv := []string{"go", "build", "-o", want, "./cmd/tier-sweep"}; !slices.Equal(got.argv, wantArgv) || got.timeout != 10*time.Minute {
		t.Errorf("Build ran %q with timeout %v, want %q with 10m", got.argv, got.timeout, wantArgv)
	}
	if stderr.String() != "build output\n" {
		t.Errorf("stderr = %q, want the build's output", stderr.String())
	}
	remove()
	if !slices.Equal(removed, []string{made}) {
		t.Errorf("RemoveAll paths = %q, want exactly %q", removed, made)
	}
}

func TestTreeBuildStopsWhenTheTempDirectoryFails(t *testing.T) {
	t.Parallel()
	built := false
	ts := treeSweep{
		MkdirTemp: func(string, string) (string, error) { return "", errors.New("no space left") },
		Build:     func(string, []string, []string, io.Writer, time.Duration) int { built = true; return 0 },
	}
	var stderr bytes.Buffer
	bin, remove, ok := ts.build("/fake/work", nil, &stderr)
	if ok || bin != "" || built {
		t.Errorf("build = (%q, ok %v) with Build run %v, want no build after a failed MkdirTemp", bin, ok, built)
	}
	if remove == nil {
		t.Fatal("remove is nil, want a no-op the caller can defer")
	}
	if !strings.HasPrefix(stderr.String(), "sweep-run: ") {
		t.Errorf("stderr = %q, want the tempDir line", stderr.String())
	}
}

func TestTreeBuildReportsAFailedBuildAndRemovesTheDirectory(t *testing.T) {
	t.Parallel()
	const made = "/fake/sweep-bin.2"
	var removed []string
	ts := treeSweep{
		MkdirTemp: func(string, string) (string, error) { return made, nil },
		RemoveAll: func(path string) error { removed = append(removed, path); return nil },
		Build:     func(string, []string, []string, io.Writer, time.Duration) int { return 3 },
	}
	var stderr bytes.Buffer
	bin, remove, ok := ts.build("/fake/work", nil, &stderr)
	line := stderr.String()
	if ok || bin != "" {
		t.Errorf("build = (%q, ok %v) after a failed go build, want ok false and no bin", bin, ok)
	}
	if !strings.HasPrefix(line, "sweep-run: ") || !strings.Contains(line, "build the tree sweep") || !strings.Contains(line, "exited 3") || strings.Count(line, "\n") != 1 {
		t.Errorf("stderr = %q, want one sweep-run: line naming the build step and exit status 3", line)
	}
	if !slices.Equal(removed, []string{made}) {
		t.Errorf("RemoveAll paths = %q, want exactly %q", removed, made)
	}
	remove()
	if !slices.Equal(removed, []string{made}) {
		t.Errorf("RemoveAll after the failed build = %q, want the directory removed once", removed)
	}
}
