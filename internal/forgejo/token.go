package forgejo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvKey is the key a role's token file holds. The file names the role
// (forgejo-<role>.env), so one key serves every role.
const EnvKey = "FORGEJO_TOKEN"

// UserKey is the key a role's token file holds for the bot's login, which git
// needs beside the token to authenticate a push over HTTP.
const UserKey = "FORGEJO_USER"

// TokenPath returns role's token file: <config home>/gt/forgejo-<role>.env,
// where the config home is $XDG_CONFIG_HOME or ~/.config.
func TokenPath(role string) (string, error) {
	return tokenPath(os.Getenv, os.UserHomeDir, role)
}

// tokenPath is TokenPath over injected environment and home lookups.
func tokenPath(getenv func(string) string, homeDir func() (string, error), role string) (string, error) {
	if role == "" || strings.ContainsAny(role, `/\`) {
		return "", fmt.Errorf("forgejo: role %q must be a plain name", role)
	}
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("forgejo: resolve home for token file: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gt", "forgejo-"+role+".env"), nil
}

// ReadToken reads role's token from its file.
func ReadToken(role string) (string, error) {
	path, err := TokenPath(role)
	if err != nil {
		return "", err
	}
	return ReadTokenFile(path)
}

// ReadTokenFile reads the token from path and refuses a file group or other
// can read, so a token on disk stays private.
func ReadTokenFile(path string) (string, error) {
	data, err := readPrivateFile(path)
	if err != nil {
		return "", err
	}
	token, ok := parseToken(data)
	if !ok {
		return "", fmt.Errorf("forgejo: token file %s has no %s line", path, EnvKey)
	}
	return token, nil
}

// ReadCredentialFile reads the bot's login and token from path, under the same
// rules as ReadTokenFile: a regular file with no group or other permission bit.
// Neither value appears in an error.
func ReadCredentialFile(path string) (user, token string, err error) {
	data, err := readPrivateFile(path)
	if err != nil {
		return "", "", err
	}
	token, ok := parseToken(data)
	if !ok {
		return "", "", fmt.Errorf("forgejo: token file %s has no %s line", path, EnvKey)
	}
	user, ok = parseKey(data, UserKey)
	if !ok || user == "" {
		return "", "", fmt.Errorf("forgejo: token file %s has no %s line", path, UserKey)
	}
	return user, token, nil
}

// readPrivateFile returns the content of a regular file that group and other
// cannot read, the only kind a token may live in.
func readPrivateFile(path string) (string, error) { return readPrivateFileWith(path, os.ReadFile) }

// readPrivateFileWith is readPrivateFile over an injected read, so a test can
// fail the read of a file that passed the permission check.
func readPrivateFileWith(path string, readFile func(string) ([]byte, error)) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("forgejo: token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("forgejo: token file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("forgejo: token file %s is mode %o; it must be 600 so the token stays private", path, perm)
	}
	data, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("forgejo: read token file: %w", err)
	}
	return string(data), nil
}

// parseToken returns the value of the FORGEJO_TOKEN line: its first
// KEY=value line, with a leading "export " and surrounding quotes stripped.
func parseToken(data string) (string, bool) { return parseKey(data, EnvKey) }

// parseKey returns the value of the first KEY=value line for key, with a
// leading "export " and surrounding quotes stripped.
func parseKey(data, wantKey string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != wantKey {
			continue
		}
		return unquote(strings.TrimSpace(value)), true
	}
	return "", false
}

// unquote strips one matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
