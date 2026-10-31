package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/tracker/api"
)

// bdHandshakeTimeout bounds the handshake's retries while the store is not
// answering.
const bdHandshakeTimeout = 30 * time.Second

// BeadsBinaryCheck reports the startup handshake of the town's store. For a
// Dolt store the bd on PATH must be a beads fork build whose JSON contract gt
// knows and whose schema level equals the database migration level. For a
// store whose own route names the tracker there is no bd to ask: the tracker
// must answer ready at the schema level this gt speaks. It has no auto-fix:
// gastown never installs bd.
type BeadsBinaryCheck struct {
	BaseCheck
	// store answers the handshake's two reads; nil is the town root's store.
	store deps.Store
	// lookPath finds bd for the report; nil is exec.LookPath.
	lookPath func(string) (string, error)
	// handshakeTimeout budgets the handshake; 0 is bdHandshakeTimeout. A test
	// shrinks it so a Dolt that never answers spends the budget, not the test.
	handshakeTimeout time.Duration
	// trackerHTTP answers the tracker's /readyz; nil is a client bounded by
	// readyzTimeout. A test answers in process, so no test opens a socket.
	trackerHTTP *http.Client
}

// NewBeadsBinaryCheck creates the bd handshake check.
func NewBeadsBinaryCheck() *BeadsBinaryCheck {
	return &BeadsBinaryCheck{
		BaseCheck: BaseCheck{
			CheckName:        "beads-binary",
			CheckDescription: "Check that bd is a known fork build at the database's schema level",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run performs the handshake and reports what it found against what the
// town needs. A town whose own route names the tracker has no bd version or
// Dolt migration level to read, so its store is asked the tracker's question
// instead. An unreadable route table does not say the store is Dolt: bd must
// pass, and a town that configures a tracker must pass the tracker too, as the
// start gate requires (gt-p85pw.110).
func (c *BeadsBinaryCheck) Run(ctx *CheckContext) *CheckResult {
	onTracker, routesErr := townStoreOnTracker(ctx.TownRoot)
	switch {
	case onTracker:
		return c.runTracker(ctx.TownRoot)
	case routesErr != nil && trackerConfigured(ctx.TownRoot):
		if res := c.runBD(ctx); res.Status != StatusOK {
			return res
		}
		return c.runTracker(ctx.TownRoot)
	}
	return c.runBD(ctx)
}

// runBD is the bd handshake for a Dolt store.
func (c *BeadsBinaryCheck) runBD(ctx *CheckContext) *CheckResult {
	lookPath := c.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	path, _ := lookPath("bd")

	timeout := c.handshakeTimeout
	if timeout == 0 {
		timeout = bdHandshakeTimeout
	}
	hctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	store := c.store
	if store == nil {
		// The town's own database, with the bd target variables stripped from
		// the inherited environment, and this check's budget as the parent of
		// its bd calls (deps.NewHandshakeStore).
		store = deps.NewHandshakeStore(hctx, ctx.TownRoot)
	}
	hs, err := deps.CheckBDHandshake(hctx, path, store)
	if err == nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: hs.String(),
		}
	}
	return refusalResult(c.Name(), err, deps.ErrBDHandshake, deps.BDInstallHint)
}

// runTracker is the handshake for a store on the tracker: the tracker's
// /readyz must answer ready at the schema level this gt speaks. The endpoint is
// the town's own, as the start gate reads it.
func (c *BeadsBinaryCheck) runTracker(townRoot string) *CheckResult {
	endpoint, err := config.ResolveTrackerEndpoint(townRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "the town's tracker endpoint cannot be read, so the tracker cannot be asked",
			Details: []string{err.Error()},
			FixHint: "fix the tracker section of mayor/town.json",
		}
	}
	tr := readyzTracker{url: trackerEndpoint(endpoint.URL()), client: c.trackerHTTP}
	if tr.client == nil {
		tr.client = &http.Client{Timeout: readyzTimeout}
	}
	actx, cancel := context.WithTimeout(context.Background(), readyzTimeout)
	defer cancel()
	if err := deps.CheckTrackerHandshake(actx, tr, api.SchemaLevel); err != nil {
		return refusalResult(c.Name(), err, deps.ErrTrackerHandshake, "check the tracker with `"+deps.TrackerStatusCommand+"`")
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("the tracker at %s is ready (schema level %d)", tr.url, api.SchemaLevel),
	}
}

// refusalResult reports a handshake refusal: its first line is the message,
// its fix line the remedy (fixHint when the refusal carries none), and the
// rest the details. A refusal that is not the sentinel's own error keeps its
// whole text as a detail.
func refusalResult(name string, err error, sentinel error, fixHint string) *CheckResult {
	msg := err.Error()
	lines := strings.Split(msg, "\n")
	result := &CheckResult{
		Name:    name,
		Status:  StatusError,
		Message: strings.TrimPrefix(lines[0], sentinel.Error()+": "),
		// Only a refusal with no fix line falls back to this.
		FixHint: fixHint,
	}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// The refusal carries the remedy for itself: the bd install for a bd
		// problem, the Dolt commands for a Dolt that did not answer (gt-j572r).
		if fix, ok := strings.CutPrefix(l, "fix:"); ok {
			result.FixHint = strings.TrimSpace(fix)
			continue
		}
		result.Details = append(result.Details, l)
	}
	if !errors.Is(err, sentinel) {
		result.Details = append(result.Details, msg)
	}
	return result
}
