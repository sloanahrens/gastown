package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/client"
)

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
