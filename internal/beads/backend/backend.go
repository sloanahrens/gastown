// Package backend opens a rig's beads on the store its route names: Dolt
// through bd, or the town tracker. It is the one place that chooses, so the
// rest of gt holds a beads.Client and does not know which it has.
//
// A rig on the tracker never falls back to Dolt. If the tracker cannot be
// configured, opening the rig fails and says why: two stores that can both
// take writes for one rig is the failure this exists to prevent.
package backend

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/tracker/client"
)

// Store is what a rig's beads offer whatever holds them.
type Store interface {
	beads.Client
	beads.Admin
}

var (
	_ Store = (*beads.Beads)(nil)
	_ Store = (*trackerclient.Client)(nil)
)

// Config says how to reach the tracker. It comes from the environment (the
// token never from a flag or a file in the repository): TRACKER_URL and either
// TRACKER_TOKEN or TRACKER_TOKEN_FILE.
type Config struct{ Tracker client.Config }

// FromEnv reads the tracker settings from getenv.
func FromEnv(getenv func(string) string) Config { return Config{Tracker: client.FromEnv(getenv)} }

// TownConfig is the tracker config of a town-side process, built on the town's admin credential.
func TownConfig(townRoot string, getenv func(string) string) (Config, error) {
	// The daemon's export and the reaper's purge call admin-only routes (POST
	// /v1/export, POST /v1/wisps:purge), so their token is the admin credential
	// in settings/daemon.env. The endpoint still comes from TRACKER_URL.
	// TRACKER_TOKEN and TRACKER_TOKEN_FILE are a seat's token and are never read
	// here, so a seat token in the daemon's environment cannot be the one these
	// calls use.
	return townConfig(townRoot, getenv, config.LoadTrackerAdminToken)
}

// townConfig is TownConfig reading the admin credential through load, so a
// test can drive a loader that yields no credential without a settings file.
func townConfig(townRoot string, getenv func(string) string, load func(string) (config.TrackerAdminToken, error)) (Config, error) {
	baseURL := getenv("TRACKER_URL")
	if baseURL == "" {
		return Config{}, errors.New("TRACKER_URL is not set")
	}
	if townRoot == "" {
		return Config{}, errors.New("no town root, so the tracker admin credential cannot be read")
	}
	admin, err := load(townRoot)
	if err != nil {
		return Config{}, err
	}
	secret, ok := admin.Reveal()
	if !ok {
		return Config{}, errors.New("the tracker admin credential is not valid")
	}
	return Config{Tracker: client.Config{
		BaseURL:       baseURL,
		AllowHTTPHost: getenv("TRACKER_ALLOW_HTTP_HOST"),
		Token:         secret,
	}}, nil
}

// Open opens the rig whose beads directory is beadsDir. townRoot holds the
// route table (townRoot/.beads/routes.jsonl). A rig the table does not name,
// or names with no backend, is on Dolt.
func Open(townRoot, beadsDir string, cfg Config) (Store, error) {
	route, found, err := routeFor(townRoot, beadsDir)
	if err != nil {
		return nil, err
	}
	switch name := routeBackend(route, found); name {
	case beads.BackendDolt:
		return beads.NewPinned(beadsDir), nil
	case beads.BackendTracker:
		return openTracker(route, cfg)
	default:
		return nil, fmt.Errorf("backend: route %s names the backend %q, which is neither %q nor %q", route.Prefix, name, beads.BackendDolt, beads.BackendTracker)
	}
}

func routeBackend(r beads.Route, found bool) string {
	if !found {
		return beads.BackendDolt
	}
	return r.BackendName()
}

// routeFor finds the route whose .beads directory is beadsDir.
func routeFor(townRoot, beadsDir string) (beads.Route, bool, error) {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return beads.Route{}, false, fmt.Errorf("backend: reading the route table: %w", err)
	}
	want := filepath.Clean(beadsDir)
	for _, r := range routes {
		if filepath.Clean(filepath.Join(townRoot, r.Path, ".beads")) == want {
			return r, true, nil
		}
	}
	return beads.Route{}, false, nil
}

func openTracker(r beads.Route, cfg Config) (Store, error) {
	if cfg.Tracker.BaseURL == "" {
		return nil, errors.New("backend: rig " + r.Prefix + " is on the tracker but TRACKER_URL is not set")
	}
	if cfg.Tracker.Token == "" && cfg.Tracker.TokenFile == "" {
		return nil, errors.New("backend: rig " + r.Prefix + " is on the tracker but neither TRACKER_TOKEN nor TRACKER_TOKEN_FILE is set")
	}
	tc, err := client.New(cfg.Tracker)
	if err != nil {
		// client.New never quotes the token; its message names the problem.
		return nil, fmt.Errorf("backend: rig %s: %w", r.Prefix, err)
	}
	return trackerclient.New(tc, strings.TrimSuffix(r.Prefix, "-")), nil
}
