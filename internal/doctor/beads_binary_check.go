package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
)

// bdHandshakeTimeout bounds the handshake's retries while the store is not
// answering.
const bdHandshakeTimeout = 30 * time.Second

// BeadsBinaryCheck reports the bd startup handshake: the bd on PATH must be a
// beads fork build whose JSON contract gt knows and whose schema level
// equals the database migration level. It has no auto-fix: gastown never
// installs bd.
type BeadsBinaryCheck struct {
	BaseCheck
	// store answers the handshake's two reads; nil is the town root's store.
	store deps.Store
	// lookPath finds bd for the report; nil is exec.LookPath.
	lookPath func(string) (string, error)
	// handshakeTimeout budgets the handshake; 0 is bdHandshakeTimeout. A test
	// shrinks it so a Dolt that never answers spends the budget, not the test.
	handshakeTimeout time.Duration
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
// town needs.
func (c *BeadsBinaryCheck) Run(ctx *CheckContext) *CheckResult {
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
		// the inherited environment: a stale BEADS_DIR or BEADS_DOLT_* must
		// not steer the diagnostic to another database. The store carries the
		// handshake's context, so this check's budget bounds the bd calls.
		store = beads.New(ctx.TownRoot,
			beads.WithEnv(beads.StripBDTargetEnv(os.Environ())),
			beads.WithCallContext(hctx))
	}
	hs, err := deps.CheckBDHandshake(hctx, path, store)
	if err == nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: hs.String(),
		}
	}
	msg := err.Error()
	lines := strings.Split(msg, "\n")
	result := &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: strings.TrimPrefix(lines[0], deps.ErrBDHandshake.Error()+": "),
		// Only a refusal with no fix line falls back to this.
		FixHint: deps.BDInstallHint,
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
	if !errors.Is(err, deps.ErrBDHandshake) {
		result.Details = append(result.Details, msg)
	}
	return result
}
