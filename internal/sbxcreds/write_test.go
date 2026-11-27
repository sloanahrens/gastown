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

// TestWriteKubeconfigFails makes each filesystem step of the writer fail in
// turn: the error names the step and quotes no content, and neither a temporary
// nor a partial file at path is left.
func TestWriteKubeconfigFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected failure")
	const marker = "key-bytes-that-must-not-be-echoed"
	for _, tc := range []struct {
		name, step, fail string
		cause            error
	}{
		{"create the temporary", "create a temporary kubeconfig", "create", boom},
		// A create refused by permissions holds for a root process, where a
		// chmod of a real directory does not.
		{"an unwritable directory", "create a temporary kubeconfig", "create", &fs.PathError{Op: "open", Path: "unwritable", Err: fs.ErrPermission}},
		{"write the temporary", "write the temporary kubeconfig", "write", boom},
		{"chmod the temporary", "chmod the temporary kubeconfig", "chmod", boom},
		{"close the temporary", "close the temporary kubeconfig", "close", boom},
		{"link into place", "linking the temporary into place", "link", boom},
		{"remove the temporary", "remove the temporary kubeconfig", "remove", boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := testutil.PrivateTempDir(t)
			path := filepath.Join(dir, "kubeconfig")
			ops := osKubeconfigOps
			switch tc.fail {
			case "create":
				ops.createTemp = func(string, string) (*os.File, error) { return nil, tc.cause }
			case "write":
				ops.write = func(*os.File, []byte) error { return tc.cause }
			case "chmod":
				ops.chmod = func(*os.File, fs.FileMode) error { return tc.cause }
			case "close":
				ops.close = func(f *os.File) error { _ = f.Close(); return tc.cause }
			case "link":
				ops.link = func(string, string) error { return tc.cause }
			case "remove":
				// The row's removal still unlinks, so the check below for no
				// leftover temporary is real.
				ops.remove = func(name string) error { _ = os.Remove(name); return tc.cause }
			}
			err := ops.publish(path, []byte(marker))
			if err == nil {
				t.Fatalf("write(%s) succeeded, want the %s failure", path, tc.fail)
			}
			if !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), tc.step) || strings.Contains(err.Error(), marker) {
				t.Errorf("write(%s) = %v, want a wrap of %v naming %q and no content", path, err, tc.cause, tc.step)
			}
			// Only the removal runs after the link, so its row alone leaves the
			// complete kubeconfig at path.
			if tc.fail == "remove" {
				if got, rerr := os.ReadFile(path); rerr != nil || string(got) != marker {
					t.Errorf("%s = %q (read %v), want the complete kubeconfig", path, got, rerr)
				}
			} else if _, serr := os.Lstat(path); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("%s exists after the %s failure (lstat %v), want no file", path, tc.fail, serr)
			}
			if tmps, gerr := filepath.Glob(filepath.Join(dir, ".kubeconfig.tmp-*")); gerr != nil || len(tmps) != 0 {
				t.Errorf("temporaries %v left after the %s failure (glob %v)", tmps, tc.fail, gerr)
			}
		})
	}
}
