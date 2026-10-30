package ominstaller

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"
)

// Environment names the resolver reads. GT_TOWN_ROOT is the only GT_ name read;
// the rest are INSTALL_OM_ overrides.
const (
	envTownRoot  = "GT_TOWN_ROOT"
	envOMPath    = "INSTALL_OM_PATH"
	envSrcDir    = "INSTALL_OM_SRC_DIR"
	envSignDir   = "INSTALL_OM_SIGN_DIR"
	envSignIdent = "INSTALL_OM_SIGN_IDENTITY"
	envLockWait  = "INSTALL_OM_LOCK_WAIT"
	envSignWait  = "INSTALL_OM_SIGN_TIMEOUT"
	envSmokeWait = "INSTALL_OM_SMOKE_TIMEOUT"
)

const (
	defaultLockWait  = 300 * time.Second
	defaultSignWait  = 30 * time.Second
	defaultSmokeWait = 30 * time.Second
	// defaultSignIdentity is the keychain identity the gt installer signs gt with.
	defaultSignIdentity = "gastown-dev-unattended"
)

// resolveConfig fills each unset path and wait in cfg from the environment, then
// from the default. getenv reads the environment, exists reports whether a file
// exists and abs makes a path absolute; all three are seams so no test reads the
// host. cwd is where the town search starts. A value that cannot be resolved is
// an error, and a caller refuses the install on it.
func resolveConfig(cfg Config, cwd string, getenv func(string) string, exists func(string) bool, abs func(string) (string, error)) (Config, error) {
	if cfg.OMPath == "" {
		cfg.OMPath = getenv(envOMPath)
	}
	if cfg.OMPath == "" {
		home := getenv("HOME")
		if home == "" {
			return Config{}, errors.New("no HOME to find the default om path (set INSTALL_OM_PATH)")
		}
		cfg.OMPath = filepath.Join(home, "go", "bin", "om")
	}

	if cfg.TownRoot == "" {
		cfg.TownRoot = getenv(envTownRoot)
	}
	if cfg.TownRoot == "" {
		town, err := townRootAbove(cwd, abs, exists)
		if err != nil {
			return Config{}, err
		}
		cfg.TownRoot = town
	}
	if cfg.TownRoot == "" {
		return Config{}, errors.New("no town found (set GT_TOWN_ROOT)")
	}

	if cfg.SrcDir == "" {
		cfg.SrcDir = getenv(envSrcDir)
	}
	if cfg.SrcDir == "" {
		cfg.SrcDir = filepath.Join(cfg.TownRoot, ".runtime", "om-src")
	}
	if cfg.SignDir == "" {
		cfg.SignDir = getenv(envSignDir)
	}
	if cfg.SignDir == "" {
		cfg.SignDir = filepath.Join(cfg.TownRoot, ".runtime", "signing")
	}
	if cfg.SignIdentity == "" {
		cfg.SignIdentity = getenv(envSignIdent)
	}
	if cfg.SignIdentity == "" {
		cfg.SignIdentity = defaultSignIdentity
	}

	var err error
	if cfg.LockWait, err = resolveWait(cfg.LockWait, envLockWait, defaultLockWait, getenv); err != nil {
		return Config{}, err
	}
	if cfg.SignWait, err = resolveWait(cfg.SignWait, envSignWait, defaultSignWait, getenv); err != nil {
		return Config{}, err
	}
	if cfg.SmokeWait, err = resolveWait(cfg.SmokeWait, envSmokeWait, defaultSmokeWait, getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// townRootAbove returns the outermost directory at or above dir that holds
// mayor/town.json, or "" when none does. A relative dir is made absolute first,
// so the walk reaches the filesystem root and never returns a relative path.
// Outermost, as internal/workspace finds the town: a rig can carry its own mayor/.
func townRootAbove(dir string, abs func(string) (string, error), exists func(string) bool) (string, error) {
	start, err := abs(dir)
	if err != nil {
		return "", fmt.Errorf("cannot make %s absolute to search for the town: %w", dir, err)
	}
	found := ""
	for d := filepath.Clean(start); ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, "mayor", "town.json")) {
			found = d
		}
		if filepath.Dir(d) == d {
			return found, nil
		}
	}
}

// resolveWait returns set when it is positive, else the whole seconds in the named
// variable when that is positive, else def. Zero is unset, as it is in Config, so
// a zero environment value also takes the default. A negative Config wait, and an
// environment value that is not a whole, non-negative number of seconds, is an error.
func resolveWait(set time.Duration, name string, def time.Duration, getenv func(string) string) (time.Duration, error) {
	if set < 0 {
		return 0, fmt.Errorf("a negative wait (%s) is refused", set)
	}
	if set > 0 {
		return set, nil
	}
	raw := getenv(name)
	if raw == "" {
		return def, nil
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs < 0 || secs > math.MaxInt64/int64(time.Second) {
		return 0, errors.New(name + " must be a whole number of seconds")
	}
	if secs == 0 {
		return def, nil
	}
	return time.Duration(secs) * time.Second, nil
}
