package doltserver

import (
	"bytes"
	"database/sql"
	"errors"
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

// TestCredentialFileUserDSNCarriesFileCredential: with no explicit
// operational.dolt setting, the DSN every daemon and gt dolt connection is
// built from carries the credential file's user and password.
func TestCredentialFileUserDSNCarriesFileCredential(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownCredential(t, town, "user=gt\npassword=hunter2\n", 0o600)

	config := DefaultConfig(town)
	if err := config.CredentialError(); err != nil {
		t.Fatalf("CredentialError = %v, want nil", err)
	}
	if got, want := config.UserDSN(), "gt:hunter2"; got != want {
		t.Errorf("UserDSN = %q, want %q", got, want)
	}
}

// TestCredentialFileNoFileLeavesUserDSNAsToday: a town with no credential file
// connects exactly as before the file was read: the default user, no password.
func TestCredentialFileNoFileLeavesUserDSNAsToday(t *testing.T) {
	t.Parallel()

	config := DefaultConfig(t.TempDir())
	if err := config.CredentialError(); err != nil {
		t.Fatalf("CredentialError = %v, want nil for an absent file", err)
	}
	if got := config.UserDSN(); got != DefaultUser {
		t.Errorf("UserDSN = %q, want %q", got, DefaultUser)
	}
}

// TestCredentialFileUnreadableStaysCredentialError: a credential file the
// config resolution cannot read is kept as CredentialError, naming the file and
// holding nothing from it. A malformed file stands in for any read failure here
// because it refuses under root as well; the insecure-file case is gt-ijc7w1.
func TestCredentialFileUnreadableStaysCredentialError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownCredential(t, town, "user=gt\npassword=hunter2\nextra=1\n", 0o600)

	err := DefaultConfig(town).CredentialError()
	if err == nil {
		t.Fatal("DefaultConfig read a malformed credential file cleanly")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file %q", err, path)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q holds the password", err)
	}
}

// TestCredentialFileUserDSNFailsClosed: with a credential file the town cannot
// read, UserDSN names a user no server has and no password, never the default
// user, so a caller that skips CredentialError fails to connect.
func TestCredentialFileUserDSNFailsClosed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownCredential(t, town, "user=gt\npassword=hunter2\nextra=1\n", 0o600)

	dsn := DefaultConfig(town).UserDSN()
	if !strings.HasPrefix(dsn, unreadableCredentialUser) || strings.Contains(dsn, DefaultUser+"@") || strings.Contains(dsn, "hunter2") {
		t.Errorf("UserDSN() = %q, want the %s user with no password and not the default user", dsn, unreadableCredentialUser)
	}
}

// TestCredentialFileTCPDSNRefuses: with a credential file the town cannot
// read, the TCP DSN the daemon connects with is not built at all: tcpDSN
// returns the credential error and no DSN, never one that would fall back to
// the default user.
func TestCredentialFileTCPDSNRefuses(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownCredential(t, town, "user=gt\npassword=hunter2\nextra=1\n", 0o600)

	dsn, err := DefaultConfig(town).tcpDSN()
	if err == nil {
		t.Fatalf("tcpDSN = %q, want the credential error", dsn)
	}
	if dsn != "" {
		t.Errorf("tcpDSN = %q alongside an error, want no DSN", dsn)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file %q", err, path)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q holds the password", err)
	}
}

// TestCredentialFileDrainSkipsAndWarns: a stop whose credential file cannot be
// read skips the connection drain instead of opening a connection as the
// default user, and warns on the host's warning sink naming the file and no
// value from it.
func TestCredentialFileDrainSkipsAndWarns(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownCredential(t, town, "user=gt\npassword=hunter2\nextra=1\n", 0o600)

	h := newFakeHost().host()
	var warnings bytes.Buffer
	h.warn = &warnings
	opened := false
	h.openDB = func(string) (*sql.DB, error) {
		opened = true
		return nil, errors.New("fake host: no SQL server")
	}

	h.drainConnectionsBeforeStop(h.DefaultConfig(town))

	if opened {
		t.Error("drain opened a connection with an unreadable credential file")
	}
	got := warnings.String()
	if !strings.Contains(got, path) {
		t.Errorf("warning %q does not name the file %q", got, path)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("warning %q holds the password", got)
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
