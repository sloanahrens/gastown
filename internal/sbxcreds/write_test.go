package sbxcreds

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

const (
	kubeconfigSample = "apiVersion: v1\nkind: Config\nclusters: []\nusers: []\n"
	plantedContent   = "already there"
)

// wantSampleFile checks path is mode 0600 holding the sample kubeconfig.
func wantSampleFile(t *testing.T, path string) {
	t.Helper()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of %s = %v (stat error %v), want 0600", path, info, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != kubeconfigSample {
		t.Errorf("content of %s = %q (read %v), want %q", path, got, err, kubeconfigSample)
	}
}

// TestWriteKubeconfigMode pins 0600, the exact bytes and no temporary left, under
// umask 000 as gt-ldvvrr requires.
//
//testpolicy:allow parallel — a umask is process-wide, so no other test may run beside this one
func TestWriteKubeconfigMode(t *testing.T) {
	dir := testutil.PrivateTempDir(t)
	path := filepath.Join(dir, "kubeconfig")
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	if err := writeKubeconfig(path, []byte(kubeconfigSample)); err != nil {
		t.Fatalf("writeKubeconfig(%s): %v", path, err)
	}
	wantSampleFile(t, path)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("%s holds %d entries (read %v), want only the kubeconfig", dir, len(entries), err)
	}
}

// TestWriteKubeconfigModeUnderUmask pins 0600 under a restrictive umask, where
// CreateTemp's own 0600 is cut to nothing, so only the writer's chmod yields 0600.
//
//testpolicy:allow parallel — a umask is process-wide, so no other test may run beside this one
func TestWriteKubeconfigModeUnderUmask(t *testing.T) {
	path := filepath.Join(testutil.PrivateTempDir(t), "kubeconfig")
	old := syscall.Umask(0o700) // after the temporary directory, which it would lock
	t.Cleanup(func() { syscall.Umask(old) })
	if err := writeKubeconfig(path, []byte(kubeconfigSample)); err != nil {
		t.Fatalf("writeKubeconfig(%s): %v", path, err)
	}
	wantSampleFile(t, path)
}

// TestWriteKubeconfigRefuses drives each refusal row with the sample as content.
func TestWriteKubeconfigRefuses(t *testing.T) {
	t.Parallel()
	runRefusals(t, kubeconfigSample)
}

// TestWriteKubeconfigNeverEchoes drives each refusal row with a marker as content.
func TestWriteKubeconfigNeverEchoes(t *testing.T) {
	t.Parallel()
	runRefusals(t, "key-bytes-that-must-not-be-echoed")
}

// runRefusals drives an existing file (which must stay intact) and a missing
// directory: each returns an error that quotes no data and leaves no temporary.
func runRefusals(t *testing.T, data string) {
	rows := []struct {
		name     string
		setup    func(t *testing.T) string
		existing bool
	}{
		{"an existing file", func(t *testing.T) string {
			path := filepath.Join(testutil.PrivateTempDir(t), "kubeconfig")
			if err := os.WriteFile(path, []byte(plantedContent), 0o600); err != nil {
				t.Fatalf("plant %s: %v", path, err)
			}
			return path
		}, true},
		{"a missing directory", func(t *testing.T) string {
			return filepath.Join(testutil.PrivateTempDir(t), "absent", "kubeconfig")
		}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path := row.setup(t)
			err := writeKubeconfig(path, []byte(data))
			if err == nil {
				t.Fatalf("writeKubeconfig(%s) wrote the path, want a refusal", path)
			}
			if strings.Contains(err.Error(), data) {
				t.Errorf("the refusal of %s quotes its content: %v", path, err)
			}
			if row.existing {
				if got, rerr := os.ReadFile(path); rerr != nil || string(got) != plantedContent {
					t.Errorf("the refusal of %s left %q (read %v), want the planted %q", path, got, rerr, plantedContent)
				}
			}
			entries, rerr := os.ReadDir(filepath.Dir(path))
			if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				t.Fatalf("read %s: %v", filepath.Dir(path), rerr)
			}
			for _, e := range entries {
				if e.Name() != filepath.Base(path) {
					t.Errorf("the refusal of %s left %s in the directory", path, e.Name())
				}
			}
		})
	}
}
