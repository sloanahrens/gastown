package forgejo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const credentialFileSecret = "tok-s3cret-value"

func writeCredentialFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forgejo-landing.env")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

// A mode-600 file returns the login and the token; every file the helper must
// not trust is refused, and no refusal repeats the token.
func TestReadCredentialFile(t *testing.T) {
	t.Parallel()
	good := "FORGEJO_URL=http://scratch-forgejo:3000\nFORGEJO_USER=bot-landing\nFORGEJO_TOKEN=" + credentialFileSecret + "\n"

	user, token, err := ReadCredentialFile(writeCredentialFile(t, good, 0o600))
	if err != nil || user != "bot-landing" || token != credentialFileSecret {
		t.Fatalf("ReadCredentialFile(mode 600) = %q, %q, %v; want bot-landing and the token", user, token, err)
	}

	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"mode 640", func(t *testing.T) string { return writeCredentialFile(t, good, 0o640) }},
		{"mode 644", func(t *testing.T) string { return writeCredentialFile(t, good, 0o644) }},
		{"a missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.env") }},
		{"a directory", func(t *testing.T) string { return t.TempDir() }},
		{"no FORGEJO_USER", func(t *testing.T) string {
			return writeCredentialFile(t, "FORGEJO_TOKEN="+credentialFileSecret+"\n", 0o600)
		}},
		{"an empty FORGEJO_USER", func(t *testing.T) string {
			return writeCredentialFile(t, "FORGEJO_USER=\nFORGEJO_TOKEN="+credentialFileSecret+"\n", 0o600)
		}},
		{"an empty FORGEJO_TOKEN", func(t *testing.T) string {
			return writeCredentialFile(t, "FORGEJO_USER=bot-landing\nFORGEJO_TOKEN=\n", 0o600)
		}},
		{"no FORGEJO_TOKEN", func(t *testing.T) string {
			return writeCredentialFile(t, "FORGEJO_USER=bot-landing\n", 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			user, token, err := ReadCredentialFile(tc.path(t))
			if err == nil {
				t.Fatalf("ReadCredentialFile(%s) = %q, %q, nil; want a refusal", tc.name, user, token)
			}
			if user != "" || token != "" {
				t.Errorf("a refusal returned %q and %q, want both empty", user, token)
			}
			if strings.Contains(err.Error(), credentialFileSecret) {
				t.Errorf("the error carries the token: %v", err)
			}
		})
	}
}

// A refusal for a missing value names the key that is missing, never a value.
func TestReadCredentialFileNamesTheMissingKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, wantKey string }{
		{"an empty token", "FORGEJO_USER=bot-landing\nFORGEJO_TOKEN=\n", "FORGEJO_TOKEN"},
		{"an empty login", "FORGEJO_USER=\nFORGEJO_TOKEN=" + credentialFileSecret + "\n", "FORGEJO_USER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ReadCredentialFile(writeCredentialFile(t, tc.body, 0o600))
			if err == nil || !strings.Contains(err.Error(), tc.wantKey+" value") {
				t.Fatalf("error = %v, want one naming %s", err, tc.wantKey)
			}
			if strings.Contains(err.Error(), credentialFileSecret) {
				t.Errorf("the error carries the token: %v", err)
			}
		})
	}
}

// A file that passes the permission check and then fails to read is refused
// with the read error, and the error does not carry the token.
func TestReadCredentialFileRefusesAFileThatFailsToRead(t *testing.T) {
	t.Parallel()
	path := writeCredentialFile(t, "FORGEJO_USER=bot-landing\nFORGEJO_TOKEN="+credentialFileSecret+"\n", 0o600)
	_, err := readPrivateFileWith(path, func(string) ([]byte, error) { return nil, errors.New("input/output error") })
	if err == nil || !strings.Contains(err.Error(), "read token file") {
		t.Fatalf("readPrivateFileWith(failing read) error = %v; want a read error", err)
	}
	if strings.Contains(err.Error(), credentialFileSecret) {
		t.Errorf("the error carries the token: %v", err)
	}
}
