package ominstaller

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"time"
)

// Environment names the resolver reads. GT_TOWN_ROOT is the one GT_ name an
// install reads; the rest are the script's INSTALL_OM_ overrides.
const (
	envTownRoot  = "GT_TOWN_ROOT"
	envOMPath    = "INSTALL_OM_PATH"
	envSrcDir    = "INSTALL_OM_SRC_DIR"
	envSignDir   = "INSTALL_OM_SIGN_DIR"
	envLockWait  = "INSTALL_OM_LOCK_WAIT"
	envSignWait  = "INSTALL_OM_SIGN_TIMEOUT"
	envSmokeWait = "INSTALL_OM_SMOKE_TIMEOUT"
)

const (
	defaultLockWait  = 300 * time.Second
	defaultSignWait  = 30 * time.Second
	defaultSmokeWait = 30 * time.Second
)

// resolveConfig fills each unset path and wait in cfg from the environment, then
// from the default. getenv reads the environment and exists reports whether a
// file exists; both are seams so no test reads the host. cwd is where the town
// search starts. A value that cannot be resolved is an error, and a caller
// refuses the install on it.
func resolveConfig(cfg Config, cwd string, getenv func(string) string, exists func(string) bool) (Config, error) {
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
		cfg.TownRoot = townRootAbove(cwd, exists)
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
// mayor/town.json, or "" when none does. Outermost, as internal/workspace finds
// the town: a rig can carry its own mayor/.
func townRootAbove(dir string, exists func(string) bool) string {
	found := ""
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, "mayor", "town.json")) {
			found = d
		}
		if filepath.Dir(d) == d {
			return found
		}
	}
}

// resolveWait returns set when it is non-zero, else the whole seconds in the
// named variable, else def. An environment value that is not a whole, non-negative
// number of seconds is an error.
func resolveWait(set time.Duration, name string, def time.Duration, getenv func(string) string) (time.Duration, error) {
	if set != 0 {
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
	return time.Duration(secs) * time.Second, nil
}
