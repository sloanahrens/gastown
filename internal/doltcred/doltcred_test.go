package doltcred

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// writeCredential writes body as a 0600 credential file in a temp dir and
// returns its path.
func writeCredential(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dolt-credential")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestReadReadsCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Credential
	}{
		{
			name: "user and password",
			body: "user=gt\npassword=hunter2\n",
			want: Credential{User: "gt", Password: "hunter2"},
		},
		{
			name: "comments and blank lines",
			body: "# the dolt user\n\nuser=gt\n\n  # an indented comment\npassword=p=a=s=s\n",
			want: Credential{User: "gt", Password: "p=a=s=s"},
		},
		{
			name: "value keeps spaces up to the line end",
			body: "user=gt\npassword=  two  spaces  \n",
			want: Credential{User: "gt", Password: "  two  spaces  "},
		},
		{
			name: "CRLF line ends",
			body: "user=gt\r\npassword=secret\r\n",
			want: Credential{User: "gt", Password: "secret"},
		},
		{
			name: "no trailing newline",
			body: "user=gt\npassword=secret",
			want: Credential{User: "gt", Password: "secret"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Read(writeCredential(t, tt.body))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Read = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadWithoutAFileIsNoCredential(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent")
	for _, path := range []string{"", absent} {
		got, err := Read(path)
		if err != nil {
			t.Errorf("Read(%q): %v, want no error", path, err)
			continue
		}
		if got != (Credential{}) {
			t.Errorf("Read(%q) = %+v, want the zero Credential", path, got)
		}
	}
}

func TestReadRefusals(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	tests := []struct {
		name string
		body string
		mode os.FileMode
		rule string
		// values are the file's values the error must not print.
		values []string
	}{
		{
			name:   "group readable",
			body:   "user=gt\npassword=" + secret + "\n",
			mode:   0o640,
			rule:   "group or other",
			values: []string{secret},
		},
		{
			name:   "other readable",
			body:   "user=gt\npassword=" + secret + "\n",
			mode:   0o604,
			rule:   "group or other",
			values: []string{secret},
		},
		{
			name:   "group and other readable",
			body:   "user=gt\npassword=" + secret + "\n",
			mode:   0o644,
			rule:   "group or other",
			values: []string{secret},
		},
		{
			name:   "unknown key",
			body:   "user=gt\npassword=" + secret + "\ncolour=blue-value\n",
			mode:   0o600,
			rule:   "unknown key",
			values: []string{secret, "blue-value"},
		},
		{
			name:   "duplicate user",
			body:   "user=first-user\nuser=second-user\npassword=" + secret + "\n",
			mode:   0o600,
			rule:   "twice",
			values: []string{"first-user", "second-user", secret},
		},
		{
			name:   "duplicate password",
			body:   "user=gt\npassword=" + secret + "\npassword=again-value\n",
			mode:   0o600,
			rule:   "twice",
			values: []string{secret, "again-value"},
		},
		{
			name:   "line without equals",
			body:   "user=gt\npassword=" + secret + "\njust-a-line\n",
			mode:   0o600,
			rule:   "missing '='",
			values: []string{secret},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeCredential(t, tt.body)
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			_, err := Read(path)
			if err == nil {
				t.Fatalf("Read accepted a file it must refuse (%s)", tt.name)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error %q does not name the path %q", msg, path)
			}
			if !strings.Contains(msg, tt.rule) {
				t.Errorf("error %q does not name the rule %q", msg, tt.rule)
			}
			for _, value := range tt.values {
				if strings.Contains(msg, value) {
					t.Errorf("error %q holds the value %q", msg, value)
				}
			}
		})
	}
}

func TestReadRefusesAFileItCannotOpen(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	path := writeCredential(t, "user=gt\npassword="+secret+"\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	got, err := Read(path)
	if os.Geteuid() == 0 {
		// Root bypasses the permission bits, so the file opens and reads: the
		// refusal below is what a non-root reader must get.
		if err != nil {
			t.Fatalf("Read failed on a file root can read: %v", err)
		}
		if got.Password != secret {
			t.Fatalf("Read = %+v, want the password root can read", got)
		}
		return
	}
	if err == nil {
		t.Fatalf("Read returned the credential of a file it cannot open: %+v", got)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err.Error(), path)
	}
}

func TestReadRefusesADirectory(t *testing.T) {
	t.Parallel()
	// The directory is 0700, so only the regular-file rule can refuse it.
	dir := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := Read(dir)
	if err == nil {
		t.Fatal("Read accepted a directory")
	}
	if msg := err.Error(); !strings.Contains(msg, dir) || !strings.Contains(msg, "regular file") {
		t.Fatalf("error %q does not name the path and the rule", msg)
	}
}

func TestReadRefusesAPathItCannotStat(t *testing.T) {
	t.Parallel()
	// A path below a regular file stats with ENOTDIR, which is an error and not
	// the "no such file" that means no credential.
	file := writeCredential(t, "user=gt\npassword=secret\n")
	path := filepath.Join(file, "dolt-credential")
	_, err := Read(path)
	if err == nil {
		t.Fatalf("Read accepted the path %q, whose parent is a file", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err.Error(), path)
	}
}

func TestReadRefusesAnOverlongLine(t *testing.T) {
	t.Parallel()
	// bufio.Scanner's token limit is 64 KiB, and a line past it is an unread
	// file rather than a credential with a missing key.
	path := writeCredential(t, "user=gt\npassword="+strings.Repeat("x", 1<<17)+"\n")
	_, err := Read(path)
	if err == nil {
		t.Fatal("Read accepted a file on a line past the scanner's limit")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err.Error(), path)
	}
}

func TestCredentialFormattingHidesPassword(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	c := Credential{User: "gt", Password: secret}
	for _, format := range []string{"%v", "%+v", "%s"} {
		if out := fmt.Sprintf(format, c); strings.Contains(out, secret) {
			t.Errorf("%s printed the password: %q", format, out)
		}
	}
}

func TestCredentialLogValueHidesPassword(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	rec := slog.NewRecord(time.Unix(0, 0), slog.LevelInfo, "connecting", 0)
	rec.AddAttrs(slog.Any("credential", Credential{User: "gt", Password: secret}))
	if err := log.Handler().Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the log line printed the password: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "gt") {
		t.Errorf("the log line dropped the user: %s", buf.String())
	}
}
