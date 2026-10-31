package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/tracker/client"
)

// readyzBodyLimit bounds the readiness body read: the answer is a small JSON
// object, and a probe is never a way to pull a large body through the check.
const readyzBodyLimit = 64 << 10

// readyzTimeout bounds the wait for the readiness answer, so a tracker that
// does not answer reads as an error instead of stalling the doctor run.
const readyzTimeout = 5 * time.Second

// TrackerCheck reports whether the town tracker is ready. It asks /readyz,
// which answers ready only when the database answers a ping and accepts a
// write, so a dead or read-only tracker shows up beside the Dolt checks.
//
// The check is not applicable to a town with no rig on the tracker: it reports
// skipped then, never OK, so a town with no tracker does not read as a
// tracker that proved healthy. Report only; it has no auto-fix.
type TrackerCheck struct {
	BaseCheck

	// getenv reads TRACKER_URL and the token. Nil is os.Getenv; tests answer
	// from a map, so the check reads nothing from the host's environment.
	getenv func(string) string

	// http answers the probe. Nil is the client's own transport; tests answer
	// in process, so no test in this package opens a socket.
	http *http.Client
}

// NewTrackerCheck creates the tracker readiness check.
func NewTrackerCheck() *TrackerCheck {
	return &TrackerCheck{
		BaseCheck: BaseCheck{
			CheckName:        "tracker",
			CheckDescription: "Check the town tracker answers ready (database ping and write probe)",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run probes the tracker when a route puts a rig on it.
func (c *TrackerCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: c.Category()}
	rigs, err := trackerRoutedRigs(ctx.TownRoot)
	if err != nil {
		// An unreadable route table leaves the check unable to tell whether a
		// rig is on the tracker: it reports the read error rather than a
		// result it cannot support.
		res.Status = StatusError
		res.Message = "cannot read the town routes, so the tracker is not checked"
		res.Details = []string{err.Error()}
		return res
	}
	if len(rigs) == 0 {
		res.Status = StatusSkipped
		res.Message = "no rig is on the tracker; not applicable"
		return res
	}
	res.Details = rigs

	getenv := c.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// The tracker settings come the way internal/beads/backend reads them, so
	// the check fails on exactly the configuration that fails a rig open.
	cfg := client.FromEnv(getenv)
	if cfg.BaseURL == "" {
		res.Status = StatusError
		res.Message = "a rig is on the tracker but TRACKER_URL is not set, so the tracker cannot be reached"
		res.FixHint = "Set TRACKER_URL and TRACKER_TOKEN (or TRACKER_TOKEN_FILE) as the town's tracker settings"
		return res
	}
	cfg.Timeout = readyzTimeout
	cfg.HTTP = c.http
	tc, err := client.New(cfg)
	if err != nil {
		res.Status = StatusError
		res.Message = "a rig is on the tracker but the tracker client cannot be built from TRACKER_URL and the token settings"
		res.FixHint = "Set TRACKER_URL to an https endpoint and TRACKER_TOKEN (or TRACKER_TOKEN_FILE)"
		return res
	}

	endpoint := trackerEndpoint(cfg.BaseURL)
	actx, cancel := context.WithTimeout(context.Background(), readyzTimeout)
	defer cancel()
	status, err := tc.Readyz(actx)
	if err != nil {
		res.Status = StatusError
		res.Message = fmt.Sprintf("the tracker at %s is not ready: %v", endpoint, err)
		return res
	}
	if status.Status != "ready" {
		res.Status = StatusError
		res.Message = fmt.Sprintf("the tracker at %s answered %q, not ready", endpoint, status.Status)
		return res
	}
	res.Status = StatusOK
	res.Message = fmt.Sprintf("the tracker at %s is ready (schema level %d)", endpoint, status.SchemaLevel)
	return res
}

// trackerEndpoint is the configured tracker address with any credentials
// stripped, so a report can name the endpoint without carrying them.
func trackerEndpoint(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "(unparsable tracker URL)"
	}
	u.User = nil
	return u.String()
}

// trackerRoutedRigs returns the paths of the rigs the route table puts on the
// tracker. A route with no backend is Dolt.
func trackerRoutedRigs(townRoot string) ([]string, error) {
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(townRoot))
	if err != nil {
		return nil, err
	}
	var rigs []string
	for _, r := range routes {
		if r.BackendName() == beads.BackendTracker {
			rigs = append(rigs, r.Path)
		}
	}
	return rigs, nil
}

// townStoreOnTracker reports whether the route whose .beads directory is the
// town's own names the tracker backend. A table with no such route is Dolt, as
// the start gate reads it; an unreadable table is the error.
func townStoreOnTracker(townRoot string) (bool, error) {
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(townRoot))
	if err != nil {
		return false, err
	}
	townBeads := filepath.Clean(beads.GetTownBeadsPath(townRoot))
	for _, r := range routes {
		if filepath.Clean(filepath.Join(townRoot, r.Path, ".beads")) == townBeads {
			return r.BackendName() == beads.BackendTracker, nil
		}
	}
	return false, nil
}

// trackerConfigured reports whether the town's town.json configures a tracker.
// A town.json that does not read counts as configured, so an unreadable answer
// never reads as "no tracker".
func trackerConfigured(townRoot string) bool {
	town, err := config.LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json"))
	if errors.Is(err, config.ErrNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	return town.Tracker != nil
}

// readyzTracker is the tracker the handshake asks: its /readyz, which answers
// without a token. The start gate's probe in internal/cmd reads the same
// answer; doctor cannot import that package, so the read is repeated here.
type readyzTracker struct {
	url    string
	client *http.Client
}

// URL names the tracker in a refusal.
func (t readyzTracker) URL() string { return t.url }

// Readyz returns the schema level of a tracker that answers ready. Every other
// answer, and every failure to get one, is an error naming the tracker.
func (t readyzTracker) Readyz(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url+"/readyz", nil)
	if err != nil {
		return 0, fmt.Errorf("the tracker at %s cannot be asked: %w", t.url, err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("the tracker at %s did not answer: %w", t.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, readyzBodyLimit))
	if err != nil {
		return 0, fmt.Errorf("the tracker at %s answered, but the answer could not be read: %w", t.url, err)
	}
	var answer struct {
		Status      string `json:"status"`
		SchemaLevel int    `json:"schema_level"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, fmt.Errorf("the tracker at %s answered %d with a body that is not its readiness report", t.url, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if answer.Message != "" {
			return 0, fmt.Errorf("the tracker at %s is not ready: %s", t.url, answer.Message)
		}
		return 0, fmt.Errorf("the tracker at %s answered %d to its readiness probe", t.url, resp.StatusCode)
	}
	if answer.Status != "ready" {
		return 0, fmt.Errorf("the tracker at %s answered %q, not ready", t.url, answer.Status)
	}
	return answer.SchemaLevel, nil
}
