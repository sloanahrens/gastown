//go:build integration

package uninstallgt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noFlagTools answers every immutable-flag command as a host without the tool
// does: the seam's errors are ignored, and the removal is what this test
// exercises against the real filesystem.
func noFlagTools(string, ...string) error { return errors.New("no such tool") }

// TestIntegrationSymlinkedBdLeavesItsTargetAlone runs a real removal: a
// directory holds gt, a bd symlink pointing at a file outside it, and two
// unrelated files. Only gt and the link go — the link's target, the unrelated
// files and the directory itself remain (gt-tpg0p.62).
func TestIntegrationSymlinkedBdLeavesItsTargetAlone(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "bin")
	home := filepath.Join(base, "home")
	mustMkdir(t, bin)
	mustMkdir(t, home)

	target := filepath.Join(base, "bd-target")
	mustWrite(t, target, "target bytes\n")
	link := filepath.Join(bin, "bd")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	gt := filepath.Join(bin, "gt")
	mustWrite(t, gt, "stub\n")
	unrelated := []string{filepath.Join(bin, "unrelated"), filepath.Join(bin, "notes.txt")}
	for _, path := range unrelated {
		mustWrite(t, path, "keep\n")
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--install-dir", bin}, &stdout, &stderr, Env{
		Getenv:  vars(map[string]string{"HOME": home}),
		Files:   OSFiles{},
		Exec:    noFlagTools,
		Signal0: neverSignals,
	})

	if code != ExitOK {
		t.Fatalf("Run = %d, want %d (stdout: %s, stderr: %s)", code, ExitOK, stdout.String(), stderr.String())
	}
	for _, path := range []string{gt, link} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after the run (err: %v)", path, err)
		}
	}
	for _, path := range append(unrelated, target, bin) {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s did not survive the run: %v", path, err)
		}
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "target bytes\n" {
		t.Errorf("the symlink's target was changed: %q, %v", data, err)
	}
	if want := "done — 2 removed from " + bin; !strings.Contains(stdout.String(), want) {
		t.Errorf("Run printed %q, want it to hold %q", stdout.String(), want)
	}
}
