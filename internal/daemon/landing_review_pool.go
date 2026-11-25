package daemon

import (
	"errors"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// wireReviewPool is where the daemon wires the review pool into a rig's
// landing worker. The pool is per-rig and off by default: a rig the config
// does not list leaves the Lander and the Worker exactly as they were, because
// the pool may only add a queue-time review and never change how a landing
// runs (gt-eduozs).
func (d *Daemon) wireReviewPool(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker, workRoot string) { //nolint:unparam // l, w and workRoot are the pool's wiring targets, used by the on path a later bead of gt-s47qp4 adds
	on, _, _, err := cfg.ReviewPoolFor(rig)
	if err != nil {
		// A bad review_pool block fails loud and leaves the pool off: the rig
		// keeps landing without one until the config is fixed, because the next
		// worker construction rereads the config and this function never retries
		// (gt-eduozs).
		d.logger.Printf("landing_worker: %s: review pool off: %v", rig, err)
		return
	}
	if !on {
		return
	}
	// On for this rig: wiring the pool into l, w and workRoot is a later bead
	// of the map, gt-s47qp4.
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
