// Package doltcred reads the one credential file a Dolt client authenticates
// with, so the gateway, the daemon and every other client share one format and
// one set of safety rules.
package doltcred

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// Credential is a Dolt user and password read from a credential file.
type Credential struct {
	User     string
	Password string
}

// redacted stands in for the password in every rendering of a Credential.
const redacted = "<redacted>"

// String implements fmt.Stringer: %v and %+v name the user and never print the
// password.
func (c Credential) String() string {
	return fmt.Sprintf("doltcred.Credential{User:%q, Password:%s}", c.User, redacted)
}

// LogValue implements slog.LogValuer, so a structured log line redacts the
// password as String does.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user", c.User),
		slog.String("password", redacted),
	)
}

var (
	_ fmt.Stringer   = Credential{}
	_ slog.LogValuer = Credential{}
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
			c.Password = value
		default:
			return Credential{}, fmt.Errorf("%s:%d: a credential line uses an unknown key, and only user and password are read", path, n)
		}
	}
	if err := sc.Err(); err != nil {
		return Credential{}, fmt.Errorf("%s: cannot read the credential file: %w", path, err)
	}
	return c, nil
}
