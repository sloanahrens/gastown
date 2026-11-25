package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// reviewPoolEnv is the environment wireReviewPool reads om's config location
// through: os.LookupEnv and os.UserHomeDir in production, and a fake in the
// tests, which never read the host's environment (gt-aovgi3).
type reviewPoolEnv struct {
	// lookup reads an environment variable; nil is os.LookupEnv.
	lookup func(key string) (string, bool)
	// home returns the user's home directory; nil is os.UserHomeDir. Its
	// result is needed only when XDG_CONFIG_HOME names no directory.
	home func() (string, error)
}

// wireReviewPool is where the daemon wires the review pool into a rig's
// landing worker. The pool is per-rig and off by default: a rig the config
// does not list leaves the Lander and the Worker exactly as they were, because
// the pool may only add a queue-time review and never change how a landing
// runs (gt-eduozs). When the pool is on for the rig, the Lander is given the
// rig's review-ahead store, the configured reuse expiry, and the path of om's
// own config file, so a landing can reuse a review its queue-time twin made
// (gt-yqoha0, gt-aovgi3).
//
// The error is the on path's when om's config file cannot be located: the
// Lander is then left as it was, pool off, so a failure never reads as a
// wired pool (gt-aovgi3).
func (d *Daemon) wireReviewPool(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker, workRoot string, env reviewPoolEnv) error { //nolint:unparam // w and workRoot are the pool's wiring targets, used by the on path a later bead of gt-s47qp4 adds
	on, _, expiry, err := cfg.ReviewPoolFor(rig)
	if err != nil {
		// A bad review_pool block fails loud and leaves the pool off: the rig
		// keeps landing without one until the config is fixed, because the next
		// worker construction rereads the config and this function never retries
		// (gt-eduozs).
		d.logger.Printf("landing_worker: %s: review pool off: %v", rig, err)
		return nil
	}
	if !on {
		return nil
	}
	// The review key reads om's own config from this file, so it is resolved
	// before anything is wired: a path this function cannot name leaves the
	// pool off rather than wiring one whose key could not be trusted
	// (gt-aovgi3).
	omPath, err := reviewPoolOMConfigPath(env)
	if err != nil {
		return fmt.Errorf("landing_worker: %s: %w", rig, err)
	}
	// The store hangs off the town the Lander already lands for, so its
	// records sit beside the town's other landing state (gt-yqoha0).
	l.Ahead = land.TownAheadStore(l.TownRoot, rig)
	l.ReuseExpiry = expiry
	l.OMConfigPath = omPath
	// Wiring the Worker and workRoot into the pool is a later bead of the map,
	// gt-s47qp4.
	return nil
}

// reviewPoolOMConfigPath resolves om's config file the way om does
// (gt-3fr65j): $XDG_CONFIG_HOME/om/config.json when that variable names a
// directory, else <home>/.config/om/config.json. A home directory the process
// cannot name is not fatal when XDG_CONFIG_HOME already names the file, and is
// reported as the cause when the fallback needed it (gt-aovgi3).
func reviewPoolOMConfigPath(env reviewPoolEnv) (string, error) {
	lookup, homeFn := env.lookup, env.home
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if homeFn == nil {
		homeFn = os.UserHomeDir
	}
	home, homeErr := homeFn()
	path, err := omConfigPath(lookup, home)
	if err != nil {
		if homeErr != nil {
			return "", fmt.Errorf("om config: read the home directory: %w", homeErr)
		}
		return "", err
	}
	return path, nil
}

// omConfigPath returns om's config file as om resolves it in its own
// internal/config/config.go: $XDG_CONFIG_HOME/om/config.json when
// XDG_CONFIG_HOME holds a non-empty value, else <home>/.config/om/config.json.
// An empty home with no XDG value is an error, because the fallback then names
// no directory. os.UserConfigDir is not this rule on macOS. No file is read
// here (gt-3fr65j).
func omConfigPath(lookup func(string) (string, bool), home string) (string, error) {
	if dir, ok := lookup("XDG_CONFIG_HOME"); ok && dir != "" {
		return filepath.Join(dir, "om", "config.json"), nil
	}
	if home == "" {
		return "", errors.New("om config: no non-empty XDG_CONFIG_HOME and no home directory")
	}
	return filepath.Join(home, ".config", "om", "config.json"), nil
}
