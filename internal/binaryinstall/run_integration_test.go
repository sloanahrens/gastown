//go:build integration

package binaryinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestIntegrationRunInstallsExecutableAndHoldsItImmutable runs the install-binary
// command on a real directory with the real immutable flag. It creates the
// missing install directory, installs the bytes executable, and then checks
// the kernel refuses a direct write and a remove of the installed binary, the
// protection gt-vya0s exists for, which no fake can show. A host the kernel
// does not grant the flag to skips with the reason.
func TestIntegrationRunInstallsExecutableAndHoldsItImmutable(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := setImmutable(probe); err != nil {
		t.Skipf("the host refuses the immutable flag (no CAP_LINUX_IMMUTABLE, or no chflags/chattr), so the real flag cannot be exercised here: %v", err)
	}
	t.Cleanup(func() { _ = clearImmutable(probe) })
	dir := t.TempDir()
	src := writeSource(t, dir, "built-gt", []byte("built binary"))
	destDir := filepath.Join(dir, "install-dir")
	dest := filepath.Join(destDir, "gt")
	t.Cleanup(func() { _ = clearImmutable(dest) })

	// Signing is off: no keychain is read and no codesign runs on this host.
	unsigned := Signer{Getenv: func(key string) string {
		if key == "INSTALL_BINARY_SIGN" {
			return "0"
		}
		return ""
	}}
	var stderr bytes.Buffer
	if code := Run([]string{src, destDir, "gt"}, &stderr, Installer{}, unsigned); code != 0 {
		t.Fatalf("Run = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}
	if !bytes.Equal(got, []byte("built binary")) {
		t.Fatalf("installed %q, want the built binary", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat the installed binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("installed binary mode %v is not executable", info.Mode())
	}

	if err := os.WriteFile(dest, []byte("STUB"), 0o755); err == nil {
		t.Error("a direct write to the installed binary succeeded, so the immutable flag is not enforced")
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, []byte("built binary")) {
		t.Errorf("a refused write changed the installed binary to %q", got)
	}
	if err := os.Remove(dest); err == nil {
		t.Error("the installed binary could be removed, so the immutable flag is not enforced")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("a refused remove took the installed binary away: %v", err)
	}

	// The installer still replaces its own immutable target.
	second := writeSource(t, dir, "built-gt-2", []byte("second binary"))
	if code := Run([]string{second, destDir, "gt"}, &stderr, Installer{}, unsigned); code != 0 {
		t.Fatalf("replace an immutable target: Run = %d (stderr: %s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, []byte("second binary")) {
		t.Fatalf("installed %q, want the second binary", got)
	}
}
