package promote

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
)

// keyFileLogged is the set of configured key paths whose substitution has
// been logged. A rig's promotion runs once per verdict and sweep tick, so
// the substitution is logged once per process rather than once per call.
var (
	keyFileLoggedMu sync.Mutex
	keyFileLogged   = map[string]bool{}
)

// ResolveKeyFile returns the deploy key path a promotion hands ssh for the
// configured promote_key_file. A path that exists is returned unchanged. A
// path that does not exist resolves to the same basename under
// $XDG_CONFIG_HOME/gt, where the pod mounts the key, when that file exists.
// Anything else returns the configured path so ssh's own error names what is
// missing, and an empty path stays empty.
//
// Only a missing path is substituted: a path that exists but cannot be stat'ed
// (permission denied) is returned unchanged rather than swapped for another
// file. The log line names the two paths and never the key's contents.
func ResolveKeyFile(path string, getenv func(string) string, stat func(string) error, logf func(string, ...any)) string {
	if path == "" {
		return ""
	}
	if err := stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return path
	}
	configHome := getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		return path
	}
	resolved := filepath.Join(configHome, "gt", filepath.Base(path))
	if stat(resolved) != nil {
		return path
	}
	logKeyFileSubstitution(path, resolved, logf)
	return resolved
}

// logKeyFileSubstitution logs the substitution the first time path is
// resolved in this process, and not again. A nil logf logs nothing.
func logKeyFileSubstitution(path, resolved string, logf func(string, ...any)) {
	keyFileLoggedMu.Lock()
	first := !keyFileLogged[path]
	keyFileLogged[path] = true
	keyFileLoggedMu.Unlock()
	if first && logf != nil {
		logf("promote: key file %s not found, using %s from the config home", path, resolved)
	}
}
