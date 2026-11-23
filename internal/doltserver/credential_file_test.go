package doltserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTownCredential writes body as the town's dolt-credentials file in the
// settings directory the loader uses, with mode, creating the directory first.
// The directory is spelled out here, not derived from the function under test,
// so a read from anywhere else fails.
func writeTownCredential(t *testing.T, townRoot, body string, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	path := filepath.Join(dir, "dolt-credentials")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// TestReadTownCredentialFileAbsent: a town with no dolt-credentials file has no
// credential, and that is not an error.
func TestReadTownCredentialFileAbsent(t *testing.T) {
	t.Parallel()
	c, err := readTownCredential(t.TempDir())
	if err != nil {
		t.Fatalf("readTownCredential with no file: %v", err)
	}
	if c.User != "" || c.Password.Reveal() != "" {
		t.Fatalf("readTownCredential with no file = %+v, want the zero Credential", c)
	}
}

// TestReadTownCredentialFileReadsUserAndPassword: a 0600 file's user and
// password are returned.
func TestReadTownCredentialFileReadsUserAndPassword(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownCredential(t, town, "user=gt\npassword=hunter2\n", 0o600)

	c, err := readTownCredential(town)
	if err != nil {
		t.Fatalf("readTownCredential: %v", err)
	}
	if c.User != "gt" {
		t.Errorf("User = %q, want %q", c.User, "gt")
	}
	if got := c.Password.Reveal(); got != "hunter2" {
		t.Errorf("Password = <redacted> (%d bytes), want the file's password", len(got))
	}
}

// TestReadTownCredentialFileRefusesInsecureMode: a 0644 file is refused with an
// error that names the file and holds no value from it.
func TestReadTownCredentialFileRefusesInsecureMode(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		//testpolicy:allow no-skip — root bypasses the permission bits, so chmod 0644 leaves the file open for root and the refusal has no case to drive
		t.Skip("running as root: root reads a 0644 file, so the refusal cannot be tested")
	}
	town := t.TempDir()
	path := writeTownCredential(t, town, "user=gt\npassword=hunter2\n", 0o644)

	_, err := readTownCredential(town)
	if err == nil {
		t.Fatal("readTownCredential accepted a 0644 file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file %q", err, path)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q holds the password", err)
	}
}

// TestReadTownCredentialFileRefusesUnknownKey: a file with an unknown key is
// refused with an error that names the file and holds no value from it.
func TestReadTownCredentialFileRefusesUnknownKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownCredential(t, town, "user=gt\npassword=hunter2\nextra=1\n", 0o600)

	_, err := readTownCredential(town)
	if err == nil {
		t.Fatal("readTownCredential accepted a file with an unknown key")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file %q", err, path)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q holds the password", err)
	}
}
