package doctor

import (
	"github.com/steveyegge/gastown/internal/daemon"
)

// DeaconSelfProbeCheck verifies the deacon patrol reacts to a known event
// on the doctor-dog cadence (glossary "Self-probe", "Probe-path rule").
// The actual probe send happens once per doctor-dog cycle from
// daemon.SendDeaconSelfProbe (called by Daemon.runDeaconSelfProbe) — this
// check only reads back the verdict via daemon.EvaluateDeaconSelfProbe, so
// running `gt doctor` never itself sends a probe (preserving "one probe
// per doctor-dog run").
type DeaconSelfProbeCheck struct {
	BaseCheck

	// evaluate is nil in production, where Run calls
	// daemon.EvaluateDeaconSelfProbe. Tests override it directly.
	evaluate func(townRoot string) daemon.DeaconSelfProbeVerdict
}

// NewDeaconSelfProbeCheck creates a new deacon-self-probe check.
func NewDeaconSelfProbeCheck() *DeaconSelfProbeCheck {
	return &DeaconSelfProbeCheck{
		BaseCheck: BaseCheck{
			CheckName:        "deacon-self-probe",
			CheckDescription: "Verify the deacon patrol acknowledges an injected probe mail within its ping-timeout budget",
			CheckCategory:    CategoryPatrol,
		},
	}
}

// Run reports the outcome of the most recent doctor-dog probe cycle.
func (c *DeaconSelfProbeCheck) Run(ctx *CheckContext) *CheckResult {
	evaluate := c.evaluate
	if evaluate == nil {
		evaluate = daemon.EvaluateDeaconSelfProbe
	}

	verdict := evaluate(ctx.TownRoot)

	status := StatusSkipped
	switch verdict.Verdict {
	case "ok":
		status = StatusOK
	case "error":
		status = StatusError
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  status,
		Message: verdict.Message,
	}
}
