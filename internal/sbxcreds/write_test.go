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

const kubeconfigSample = "apiVersion: v1\nkind: Config\nclusters: []\nusers: []\n"

// TestWriteKubeconfigMode pins 0600 and the bytes under a restrictive umask, so
// only the writer's own chmod can yield 0600.
//
//testpolicy:allow parallel — a umask is process-wide, so no other test may run beside this one
func TestWriteKubeconfigMode(t *testing.T) {
	path := filepath.Join(testutil.PrivateTempDir(t), "kubeconfig")
	old := syscall.Umask(0o700) // after the temporary directory, which it would lock
	t.Cleanup(func() { syscall.Umask(old) })
	if err := writeKubeconfig(path, []byte(kubeconfigSample)); err != nil {
		t.Fatalf("writeKubeconfig(%s): %v", path, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of %s = %v (stat error %v), want 0600", path, info, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != kubeconfigSample {
		t.Errorf("content of %s = %q (read %v), want %q", path, got, err, kubeconfigSample)
	}
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

// runRefusals drives an existing file (kept intact) and a missing directory:
// each returns an error that quotes no data and leaves no temporary.
func runRefusals(t *testing.T, data string) {
	refusals := map[string]func(t *testing.T) string{
		"an existing file": func(t *testing.T) string {
			path := filepath.Join(testutil.PrivateTempDir(t), "kubeconfig")
			if err := os.WriteFile(path, []byte("already there"), 0o600); err != nil {
				t.Fatalf("plant %s: %v", path, err)
			}
			return path
		},
		"a missing directory": func(t *testing.T) string { return filepath.Join(testutil.PrivateTempDir(t), "absent", "kubeconfig") },
	}
	for name, setup := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := setup(t)
			err := writeKubeconfig(path, []byte(data))
			if err == nil {
				t.Fatalf("writeKubeconfig(%s) wrote the path, want a refusal", path)
			}
			if strings.Contains(err.Error(), data) {
				t.Errorf("the refusal of %s quotes its content: %v", path, err)
			}
			if got, rerr := os.ReadFile(path); rerr == nil && string(got) != "already there" {
				t.Errorf("the refusal of %s changed its file to %q", path, got)
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
