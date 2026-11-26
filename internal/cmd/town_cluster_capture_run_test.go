package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestClusterCaptureRunNotWritten drives a run whose ref and path pass the
// checks: it says the capture is not written yet and creates no file.
func TestClusterCaptureRunNotWritten(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatalf("creating the work dir: %v", err)
	}
	err := runClusterCapture(context.Background(), clusterCaptureDeps{workDir: workDir}, "deployment/daemon", "gt-sbx", "capture.json")
	if err == nil {
		t.Fatalf("runClusterCapture() = nil, want the not-written refusal")
	}
	for _, want := range []string{"deployment/daemon", filepath.Join(workDir, "capture.json"), "not written yet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	clusterCaptureNoFiles(t, root)
}
