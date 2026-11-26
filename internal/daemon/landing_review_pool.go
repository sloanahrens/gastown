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
// through: os.LookupEnv, os.UserHomeDir and os.Stat in production, and injected
// fakes in the tests (gt-aovgi3, gt-lfykjq).
type reviewPoolEnv struct {
	// lookup reads an environment variable; nil is os.LookupEnv.
	lookup func(key string) (string, bool)
	// home returns the user's home directory; nil is os.UserHomeDir. Its
	// result is needed only when XDG_CONFIG_HOME names no directory.
	home func() (string, error)
	// stat reports om's config file; nil is os.Stat. An error means the review
	// key's config file cannot be used (gt-lfykjq).
	stat func(name string) (os.FileInfo, error)
}

// wireReviewPool is where the daemon wires the review pool into a rig's
// landing worker. The pool is per-rig and off by default: a rig the config
// does not list leaves the Lander and the Worker exactly as they were, because
// the pool may only add a queue-time review and never change how a landing
// runs (gt-eduozs). When the pool is on for the rig, the Lander is given the
// rig's review-ahead store, the configured reuse expiry, and the path of om's
// own config file, so a landing can reuse a review its queue-time twin made
// (gt-yqoha0, gt-aovgi3); the Worker is given that store and the pool that
// fills it (gt-sywmws).
//
// The error is the on path's when om's config file cannot be named or does not
// exist: the wiring is undone and the pool stays off, so a failure never reads
// as a wired pool (gt-aovgi3, gt-lfykjq).
func (d *Daemon) wireReviewPool(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker, workRoot string, env reviewPoolEnv) error { //nolint:unparam // workRoot is a later bead's wiring target
	on, max, expiry, err := cfg.ReviewPoolFor(rig)
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
	// The store hangs off the town the Lander already lands for, so its
	// records sit beside the town's other landing state (gt-yqoha0).
	l.Ahead = land.TownAheadStore(l.TownRoot, rig)
	l.ReuseExpiry = expiry
	// The review key is computed from om's own config file, so a pool wired
	// without it could never produce a verdict the landing could trust: a
	// missing config file or a path this function cannot name undoes the
	// wiring above rather than leaving the pool on (gt-lfykjq).
	stat := env.stat
	if stat == nil {
		stat = os.Stat
	}
	omPath, err := reviewPoolOMConfigPath(env)
	if err == nil {
		if _, statErr := stat(omPath); statErr != nil {
			err = fmt.Errorf("om config: %w", statErr)
		}
	}
	if err != nil {
		l.Ahead, l.ReuseExpiry, l.OMConfigPath = nil, 0, ""
		d.logger.Printf("landing_worker: %s: review pool off: %v", rig, err)
		return fmt.Errorf("landing_worker: %s: %w", rig, err)
	}
	l.OMConfigPath = omPath
	// The Worker gets the pool that fills the store and the store itself as its
	// hints, so a review that finishes while the bead queues is the verdict its
	// landing finds. Its lines go to the log the landing writes to (gt-sywmws).
	// Begin is the daemon's landing-pass gate, so an ahead review counts as a
	// landing pass: while a hold or drain is on Begin answers false and the pool
	// starts nothing (gt-zajcg2).
	w.Stored = l.Ahead
	w.Pool = &landworker.ReviewPool{
		Reviewer: l,
		Hints:    l.Ahead,
		Max:      max,
		Begin:    d.beginLandingPass,
		Logf:     w.Logf,
	}
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
