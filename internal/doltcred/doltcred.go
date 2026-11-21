// Package doltcred reads the one credential file a Dolt client authenticates
// with, so the gateway, the daemon and every other client share one format and
// one set of safety rules.
package doltcred

import (
	"bufio"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// Credential is a Dolt user and password read from a credential file. The
// password is a Secret, so a Credential held in an unexported struct field,
// where fmt cannot call a method, still prints an address and never the bytes;
// read the password with Password.Reveal() at the point of use and never keep
// it in a field or a log line. A dumper that follows pointers by reflection
// (go-spew style) would still read the bytes: fmt, the encoders and slog do not.
type Credential struct {
	User     string
	Password Secret
}

// redacted stands in for the password in every rendering of a Credential.
const redacted = "<redacted>"

// String implements fmt.Stringer and names the user and never the password.
func (c Credential) String() string {
	return fmt.Sprintf("doltcred.Credential{User:%q, Password:%s}", c.User, redacted)
}

// Format implements fmt.Formatter and writes the same redaction for every verb.
// Without it fmt would walk the fields for %#v, %d and any other verb that does
// not call String, so every verb prints the same redaction, directly, through a
// pointer, as an exported struct field or in a slice.
func (c Credential) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String())
}

// GoString implements fmt.GoStringer, so the %#v form is the redaction too.
func (c Credential) GoString() string { return c.String() }

// MarshalJSON implements json.Marshaler and encodes the user with the
// password replaced by the redaction.
func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}{c.User, redacted})
}

// MarshalText implements encoding.TextMarshaler and writes the redaction, so a
// text encoder never reaches the password either.
func (c Credential) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// LogValue implements slog.LogValuer, so a structured log line redacts the
// password as String does.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user", c.User),
		slog.String("password", redacted),
	)
}

var (
	_ fmt.Stringer           = Credential{}
	_ fmt.Formatter          = Credential{}
	_ fmt.GoStringer         = Credential{}
	_ json.Marshaler         = Credential{}
	_ encoding.TextMarshaler = Credential{}
	_ slog.LogValuer         = Credential{}
)

// Read returns the credential the file at path holds. An empty path, or a path
// that does not exist, means there is no credential: Read returns the zero
// Credential and a nil error.
//
// Read refuses a path that is not a regular file, a file that group or other
// can read, an unknown key, a repeated key and a line without '=', and it
// returns a credential only with a nil error. Every error names the path and
// the rule it broke; none holds a value from the file.
func Read(path string) (Credential, error) {
	if path == "" {
		return Credential{}, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credential{}, nil
	}
	if err != nil {
		return Credential{}, fmt.Errorf("%s: cannot read the credential file's metadata: %w", path, err)
	}
	if err := checkFile(path, fi); err != nil {
		return Credential{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: cannot open the credential file: %w", path, err)
	}
	defer f.Close()
	return parse(path, f)
}

// checkFile refuses a path that is not a regular file, because reading a FIFO
// or a device can wait forever, and a file group or other can read.
func checkFile(path string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: the credential path must be a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s: the credential file is mode %04o and group or other can read it, so it must be 0600", path, perm)
	}
	return nil
}

// parse reads the user= and password= lines. A value is the rest of its line,
// so a password keeps its spaces and any further '='; blank lines and lines
// whose first non-space character is '#' are skipped.
func parse(path string, r io.Reader) (Credential, error) {
	var c Credential
	var seenUser, seenPassword bool
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Credential{}, fmt.Errorf("%s:%d: a credential line is missing '=', and only user=<name> and password=<value> lines are read", path, n)
		}
		switch key {
		case "user":
			if seenUser {
				return Credential{}, fmt.Errorf("%s:%d: the credential file sets \"user\" twice", path, n)
			}
			seenUser = true
			c.User = value
		case "password":
			if seenPassword {
				return Credential{}, fmt.Errorf("%s:%d: the credential file sets \"password\" twice", path, n)
			}
			seenPassword = true
			c.Password = NewSecret(value)
		default:
			return Credential{}, fmt.Errorf("%s:%d: a credential line uses an unknown key, and only user and password are read", path, n)
		}
	}
	if err := sc.Err(); err != nil {
		return Credential{}, fmt.Errorf("%s: cannot read the credential file: %w", path, err)
	}
	return c, nil
}
