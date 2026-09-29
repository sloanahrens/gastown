package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/deps"
)

// bdHandshakeTimeout bounds the two bd calls the check makes.
const bdHandshakeTimeout = 30 * time.Second

// BeadsBinaryCheck reports the bd startup handshake: the bd on PATH must be a
// beads fork build whose JSON contract gt knows and whose schema level
// equals the database migration level. It has no auto-fix: gastown never
// installs bd.
type BeadsBinaryCheck struct {
	BaseCheck
	// run answers bd calls; nil runs the bd on PATH in the town root.
	run deps.BDRunner
	// lookPath finds bd for the report; nil is exec.LookPath.
	lookPath func(string) (string, error)
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
	run := c.run
	if run == nil {
		run = deps.NewBDProcessRunner(ctx.TownRoot)
	}
	path, _ := lookPath("bd")

	hctx, cancel := context.WithTimeout(context.Background(), bdHandshakeTimeout)
	defer cancel()
	hs, err := deps.CheckBDHandshake(hctx, path, run)
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
		FixHint: deps.BDInstallHint,
	}
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "fix:") {
			result.Details = append(result.Details, l)
		}
	}
	if !errors.Is(err, deps.ErrBDHandshake) {
		result.Details = append(result.Details, msg)
	}
	return result
}
