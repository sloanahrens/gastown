package jev

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnvKey is the name of the line that holds the API key in a key file.
const EnvKey = "TYPESAFE_API_KEY"

// KeyPaths returns the files ReadKey searches, in order: the role-neutral
// key file <config home>/gt/jev.env, where the config home is
// $XDG_CONFIG_HOME or ~/.config, then <town root>/settings/daemon.env when
// GT_TOWN_ROOT is set.
func KeyPaths() ([]string, error) {
	return keyPaths(os.Getenv, os.UserHomeDir)
}

// keyPaths is KeyPaths over injected environment and home lookups.
func keyPaths(getenv func(string) string, homeDir func() (string, error)) ([]string, error) {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := homeDir()
		if err != nil {
			return nil, fmt.Errorf("jev: resolve home for key file: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	paths := []string{filepath.Join(dir, "gt", "jev.env")}
	if root := getenv("GT_TOWN_ROOT"); root != "" {
		paths = append(paths, filepath.Join(root, "settings", "daemon.env"))
	}
	return paths, nil
}

// ReadKey returns the API key from the first file in KeyPaths that holds one.
// A file that does not exist is skipped; a file that exists but is readable
// by group or other is an error, so a key on disk stays private.
func ReadKey() (string, error) {
	paths, err := KeyPaths()
	if err != nil {
		return "", err
	}
	return readKeyFrom(ReadKeyFile, paths)
}

// readKeyFrom is ReadKey over an explicit search list and key-file reader.
// ReadKey passes ReadKeyFile; a test passes a fake and needs no file.
func readKeyFrom(read func(path string) (string, error), paths []string) (string, error) {
	for _, path := range paths {
		key, err := read(path)
		if errors.Is(err, errNoKey) || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if key == "" {
			// A reader that succeeds with no key would send an empty
			// Authorization header: keep looking, then refuse.
			continue
		}
		return key, nil
	}
	return "", fmt.Errorf("jev: no %s line in %s", EnvKey, strings.Join(paths, " or "))
}

// errNoKey marks a key file that exists but has no EnvKey line.
var errNoKey = errors.New("no key line")

// ReadKeyFile reads the API key from path and refuses a file group or other
// can read. Its errors name the path, never the key.
func ReadKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("jev: key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("jev: key file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("jev: key file %s is mode %o; it must be 600 so the key stays private", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("jev: read key file: %w", err)
	}
	key, ok := parseKey(string(data))
	if !ok {
		return "", fmt.Errorf("jev: key file %s has no usable %s line: %w", path, EnvKey, errNoKey)
	}
	return key, nil
}

// parseKey returns the value of the first non-empty EnvKey line, with a
// leading "export " and one pair of surrounding quotes stripped.
func parseKey(data string) (string, bool) {
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != EnvKey {
			continue
		}
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		if value != "" {
			return value, true
		}
	}
	return "", false
}
