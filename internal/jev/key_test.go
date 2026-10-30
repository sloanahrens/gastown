package jev

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeKeyFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{"plain", "TYPESAFE_API_KEY=abc\n", "abc", true},
		{"export and double quotes", "export TYPESAFE_API_KEY=\"abc\"\n", "abc", true},
		{"single quotes", "TYPESAFE_API_KEY='abc'", "abc", true},
		{"after comment and other keys", "# note\nOTHER=x\n\nTYPESAFE_API_KEY=abc\n", "abc", true},
		{"spaces around", "  TYPESAFE_API_KEY = abc  \n", "abc", true},
		{"empty value skipped for a later line", "TYPESAFE_API_KEY=\nTYPESAFE_API_KEY=abc\n", "abc", true},
		{"value keeps inner equals", "TYPESAFE_API_KEY=a=b\n", "a=b", true},
		{"other key only", "TYPESAFE_API_KEYS=abc\n", "", false},
		{"empty", "", "", false},
		{"empty value", "TYPESAFE_API_KEY=\n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseKey(tt.data)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestKeyPaths(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"home config", nil, []string{"/home/u/.config/gt/jev.env"}},
		{"xdg config", map[string]string{"XDG_CONFIG_HOME": "/x"}, []string{"/x/gt/jev.env"}},
		{"town settings follow", map[string]string{"GT_TOWN_ROOT": "/town"},
			[]string{"/home/u/.config/gt/jev.env", "/town/settings/daemon.env"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keyPaths(env(tt.env), home)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadKeyFrom(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "jev.env")
	second := filepath.Join(dir, "daemon.env")
	missing := filepath.Join(dir, "absent.env")

	t.Run("first file with a key wins", func(t *testing.T) {
		writeKeyFile(t, first, "TYPESAFE_API_KEY=one\n", 0o600)
		writeKeyFile(t, second, "TYPESAFE_API_KEY=two\n", 0o600)
		got, err := readKeyFrom(ReadKeyFile, []string{first, second})
		require.NoError(t, err)
		assert.Equal(t, "one", got)
	})

	t.Run("missing file is skipped", func(t *testing.T) {
		got, err := readKeyFrom(ReadKeyFile, []string{missing, second})
		require.NoError(t, err)
		assert.Equal(t, "two", got)
	})

	t.Run("file without the line is skipped", func(t *testing.T) {
		writeKeyFile(t, first, "OTHER=x\n", 0o600)
		got, err := readKeyFrom(ReadKeyFile, []string{first, second})
		require.NoError(t, err)
		assert.Equal(t, "two", got)
	})

	t.Run("no key anywhere names the files", func(t *testing.T) {
		_, err := readKeyFrom(ReadKeyFile, []string{missing, first})
		require.Error(t, err)
		assert.Contains(t, err.Error(), missing)
		assert.Contains(t, err.Error(), first)
	})
}

func TestReadKeyFile_RefusesOpenPermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "jev.env")
	writeKeyFile(t, path, "TYPESAFE_API_KEY=secret-value\n", 0o644)

	_, err := ReadKeyFile(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "600")
	assert.NotContains(t, err.Error(), "secret-value")
}

func TestReadKeyFile_RefusesDirectory(t *testing.T) {
	t.Parallel()
	_, err := ReadKeyFile(t.TempDir())
	require.Error(t, err)
}

func TestReadKeyFrom_OpenPermissionsAbortSearch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := filepath.Join(dir, "jev.env")
	later := filepath.Join(dir, "daemon.env")
	writeKeyFile(t, open, "TYPESAFE_API_KEY=one\n", 0o640)
	writeKeyFile(t, later, "TYPESAFE_API_KEY=two\n", 0o600)

	_, err := readKeyFrom(ReadKeyFile, []string{open, later})

	require.Error(t, err, "a key file others can read is an error, not a reason to try the next file")
}

// TestReadKeyFrom_UsesTheReaderItWasGiven: the search asks the reader it was
// handed, not the key files on disk. A fake that holds a key answers with it; a
// fake that holds none, or that succeeds with an empty key, refuses with an
// error naming every path. A loader that read the real files instead, or that
// took an empty key as an answer, fails here.
func TestReadKeyFrom_UsesTheReaderItWasGiven(t *testing.T) {
	t.Parallel()
	keys := map[string]string{"fake-one": "key-from-the-reader", "fake-empty": ""}
	read := func(path string) (string, error) {
		if key, ok := keys[path]; ok {
			return key, nil
		}
		return "", fmt.Errorf("fake reader: %s: %w", path, fs.ErrNotExist)
	}

	got, err := readKeyFrom(read, []string{"fake-missing", "fake-one"})
	require.NoError(t, err)
	assert.Equal(t, "key-from-the-reader", got)

	for _, tt := range []struct {
		name  string
		paths []string
	}{
		{name: "a fake that holds no key", paths: []string{"fake-missing", "fake-two"}},
		{name: "a fake that succeeds with an empty key", paths: []string{"fake-missing", "fake-empty"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := readKeyFrom(read, tt.paths)
			require.Error(t, err, "an empty or absent key is refused")
			for _, path := range tt.paths {
				assert.Contains(t, err.Error(), path)
			}
		})
	}
}
