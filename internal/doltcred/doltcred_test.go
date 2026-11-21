package doltcred

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
			want: Credential{User: "gt", Password: NewSecret("hunter2")},
		},
		{
			name: "comments and blank lines",
			body: "# the dolt user\n\nuser=gt\n\n  # an indented comment\npassword=p=a=s=s\n",
			want: Credential{User: "gt", Password: NewSecret("p=a=s=s")},
		},
		{
			name: "value keeps spaces up to the line end",
			body: "user=gt\npassword=  two  spaces  \n",
			want: Credential{User: "gt", Password: NewSecret("  two  spaces  ")},
		},
		{
			name: "CRLF line ends",
			body: "user=gt\r\npassword=secret\r\n",
			want: Credential{User: "gt", Password: NewSecret("secret")},
		},
		{
			name: "no trailing newline",
			body: "user=gt\npassword=secret",
			want: Credential{User: "gt", Password: NewSecret("secret")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Read(writeCredential(t, tt.body))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.User != tt.want.User || got.Password.Reveal() != tt.want.Password.Reveal() {
				t.Fatalf("Read = %+v (password %q), want %+v (password %q)", got, got.Password.Reveal(), tt.want, tt.want.Password.Reveal())
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
		if got.Password.Reveal() != secret {
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

// leakSecret is distinctive enough that a substring match cannot be an accident.
const leakSecret = "pw-9f3c1d7a-leak-canary"

// credHolder and the types after it hold a Credential where fmt cannot call a
// method on it: an unexported field, which fmt reaches by reflection.
type credHolder struct{ c Credential }

type credPtrHolder struct{ c *Credential }

type credNestedHolder struct{ inner credHolder }

// TestCredentialFormattingHidesPassword formats a Credential with every verb a
// log line can reach: directly, through a pointer, as an exported field, in an
// unexported field (the case fmt cannot reach a method for), as a pointer in an
// unexported field, in a holder inside a holder, and in a slice and a map. It
// fails on any output that holds the password or its hex form, or drops the
// user.
func TestCredentialFormattingHidesPassword(t *testing.T) {
	t.Parallel()
	c := Credential{User: "gt-user", Password: NewSecret(leakSecret)}
	type holder struct{ Cred Credential }
	values := map[string]any{
		"value":                      c,
		"pointer":                    &c,
		"exported field":             holder{Cred: c},
		"unexported field":           credHolder{c: c},
		"pointer in unexported":      credPtrHolder{c: &c},
		"holder in a holder":         credNestedHolder{inner: credHolder{c: c}},
		"slice":                      []Credential{c},
		"slice of unexported fields": []credHolder{{c: c}},
		"map of unexported fields":   map[string]credHolder{"k": {c: c}},
	}
	user, secretHex := "gt-user", hex.EncodeToString([]byte(leakSecret))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%t"} {
		for name, v := range values {
			t.Run(verb+"/"+name, func(t *testing.T) {
				t.Parallel()
				out := fmt.Sprintf(verb, v)
				if strings.Contains(out, leakSecret) || strings.Contains(out, secretHex) {
					t.Errorf("Sprintf(%q) of the %s printed the password: %s", verb, name, out)
				}
				// fmt prints a pointer in an unexported field as an address, so
				// neither the user nor the password is in that output.
				if name != "pointer in unexported" && !strings.Contains(out, user) && !strings.Contains(out, hex.EncodeToString([]byte(user))) {
					t.Errorf("Sprintf(%q) of the %s dropped the user: %s", verb, name, out)
				}
			})
		}
	}
}

// TestCredentialRedactingMethodsHideThePassword calls the redacting methods
// directly: fmt prefers Format, so only a direct call reaches GoString and
// MarshalText.
func TestCredentialRedactingMethodsHideThePassword(t *testing.T) {
	t.Parallel()
	c := Credential{User: "gt-user", Password: NewSecret(leakSecret)}
	text, err := c.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	for name, out := range map[string]string{"String": c.String(), "GoString": c.GoString(), "MarshalText": string(text)} {
		if strings.Contains(out, leakSecret) {
			t.Errorf("%s printed the password: %s", name, out)
		}
		if !strings.Contains(out, "gt-user") {
			t.Errorf("%s dropped the user: %s", name, out)
		}
	}
}

func TestCredentialLogValueHidesPassword(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	rec := slog.NewRecord(time.Unix(0, 0), slog.LevelInfo, "connecting", 0)
	rec.AddAttrs(slog.Any("credential", Credential{User: "gt", Password: NewSecret(secret)}))
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
