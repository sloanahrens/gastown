package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clusterCaptureNoFiles fails when root holds any regular file: a run that
// refused an input creates none.
func clusterCaptureNoFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			t.Errorf("the run created the file %s, want none", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the work root: %v", err)
	}
}

// TestClusterCaptureRunRefuse drives the two inputs runClusterCapture refuses
// before it reads anything: a bad reference and a path outside workDir. Each
// returns an error naming the input, reads no cluster and writes no file.
func TestClusterCaptureRunRefuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ref  string
		out  string
		want string
	}{
		{name: "a bad ref is refused", ref: "deployment", out: "capture.json", want: "deployment"},
		{name: "a path outside workDir is refused", ref: "deployment/daemon", out: "../escape.json", want: "escape.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workDir := filepath.Join(root, "work")
			if err := os.Mkdir(workDir, 0o700); err != nil {
				t.Fatalf("creating the work dir: %v", err)
			}
			err := runClusterCapture(context.Background(), clusterCaptureDeps{
				kubectl: driftNoRead(t),
				workDir: workDir,
			}, tc.ref, "", tc.out)
			if err == nil {
				t.Fatalf("runClusterCapture(%q, %q) = nil, want a refusal", tc.ref, tc.out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			clusterCaptureNoFiles(t, root)
		})
	}
}

// TestClusterCaptureRunReadFails drives a read that fails: the run returns the
// read's error and creates no file.
func TestClusterCaptureRunReadFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	failed := errors.New("kubectl is not installed")
	err := runClusterCapture(context.Background(), clusterCaptureDeps{
		kubectl: captureLiveAnswer("", 0, failed),
		now:     time.Now,
		workDir: root,
	}, "deployment/daemon", "gt-sbx", "capture.json")
	if !errors.Is(err, failed) {
		t.Fatalf("runClusterCapture() = %v, want the read's error %v", err, failed)
	}
	clusterCaptureNoFiles(t, root)
}

// clusterCaptureRunWorkDir returns a fresh work dir under t.TempDir for a run
// that writes a capture.
func clusterCaptureRunWorkDir(t *testing.T) string {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatalf("creating the work dir: %v", err)
	}
	return workDir
}

// TestClusterCaptureRunWritesFile drives a run whose checks and read pass: it
// reads the named object and writes the capture under workDir at mode 0600. A
// write at 0644 fails the mode assertion; a run that writes nothing fails the
// Stat.
func TestClusterCaptureRunWritesFile(t *testing.T) {
	t.Parallel()
	workDir := clusterCaptureRunWorkDir(t)
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	var args []string
	err := runClusterCapture(context.Background(), clusterCaptureDeps{
		kubectl: func(_ context.Context, _ string, _ []string, _ string, got ...string) (string, int, error) {
			args = got
			return `{"kind":"Deployment","metadata":{"name":"daemon","uid":"d-1"}}`, 0, nil
		},
		now:     func() time.Time { return at },
		workDir: workDir,
	}, "deployment/daemon", "gt-sbx", "capture.json")
	if err != nil {
		t.Fatalf("runClusterCapture() error = %v, want none", err)
	}
	if got, want := strings.Join(args, " "), "get deployment/daemon -n gt-sbx --ignore-not-found -o json"; got != want {
		t.Errorf("read with %q, want %q", got, want)
	}
	path := filepath.Join(workDir, "capture.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("statting the capture: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("the capture is mode %04o, want %04o", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the capture: %v", err)
	}
	for _, want := range []string{`"uid": "d-1"`, `"at": "2026-11-26T12:00:00Z"`, `"name": "daemon"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the capture does not hold %s: %s", want, data)
		}
	}
}

// TestClusterCaptureRunReplacesFile drives a run onto a path that already holds
// a file at mode 0644: the capture replaces its bytes and the file ends at mode
// 0600. A run that leaves the old file is red on the content; a replacement at
// the old mode is red on the mode assertion.
func TestClusterCaptureRunReplacesFile(t *testing.T) {
	t.Parallel()
	workDir := clusterCaptureRunWorkDir(t)
	path := filepath.Join(workDir, "capture.json")
	if err := os.WriteFile(path, []byte("an older capture\n"), 0o644); err != nil {
		t.Fatalf("writing the older capture: %v", err)
	}
	if err := runClusterCapture(context.Background(), clusterCaptureDeps{
		kubectl: captureLiveAnswer(`{"kind":"Deployment","metadata":{"name":"daemon","uid":"d-1"}}`, 0, nil),
		now:     time.Now,
		workDir: workDir,
	}, "deployment/daemon", "gt-sbx", "capture.json"); err != nil {
		t.Fatalf("runClusterCapture() error = %v, want none", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("statting the capture: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("the capture is mode %04o, want %04o", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the capture: %v", err)
	}
	if want := `"uid": "d-1"`; !strings.Contains(string(data), want) {
		t.Errorf("the capture does not hold %s: %s", want, data)
	}
}

// TestClusterCaptureRunWriteFails drives a write that cannot happen: the
// capture's parent directory does not exist, so the write fails and the run
// returns its error. A run that swallowed the write error would return nil.
func TestClusterCaptureRunWriteFails(t *testing.T) {
	t.Parallel()
	workDir := clusterCaptureRunWorkDir(t)
	err := runClusterCapture(context.Background(), clusterCaptureDeps{
		kubectl: captureLiveAnswer(`{"kind":"Deployment","metadata":{"name":"daemon","uid":"d-1"}}`, 0, nil),
		now:     time.Now,
		workDir: workDir,
	}, "deployment/daemon", "gt-sbx", filepath.Join("missing", "capture.json"))
	if err == nil {
		t.Fatalf("runClusterCapture() = nil, want the write's error")
	}
	for _, want := range []string{"deployment/daemon", filepath.Join(workDir, "missing", "capture.json")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	clusterCaptureNoFiles(t, workDir)
}
