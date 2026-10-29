package doctor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/bdshim"
	"github.com/steveyegge/gastown/internal/bdshim/flagrecord"
)

// BdShimFlagsCheck reports when the bd shim's recorded flag table no longer
// matches the help of the installed bd. The shim routes by which flags take a
// value, so a value flag bd gained reads as a bead id and sends the command to
// the wrong store (gt-w8j7v).
type BdShimFlagsCheck struct {
	BaseCheck
	// lookPath finds bd; nil is exec.LookPath.
	lookPath func(string) (string, error)
	// run answers bd's help; nil runs the bd lookPath finds.
	run flagrecord.Runner
	// recorded is the table bd's help is compared with; nil is the copy
	// compiled into this binary, so a check needs no checkout.
	recorded []byte
}

// NewBdShimFlagsCheck creates the check that compares the shim's flag table
// with the installed bd's help.
func NewBdShimFlagsCheck() *BdShimFlagsCheck {
	return &BdShimFlagsCheck{
		BaseCheck: BaseCheck{
			CheckName:        "bd-shim-flags",
			CheckDescription: "Compare the bd shim's recorded flag table with the installed bd's help",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run walks the installed bd's help and compares it with the recorded table.
// It reads only: the refresh is the operator's, since the recorded file and
// internal/bdshim/flags.go have to change together.
func (c *BdShimFlagsCheck) Run(ctx *CheckContext) *CheckResult {
	recorded := c.recorded
	if recorded == nil {
		recorded = bdshim.RecordedFlagFile
	}
	run := c.run
	if run == nil {
		lookPath := c.lookPath
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		path, err := lookPath("bd")
		if err != nil {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: "bd is not on PATH, so its help cannot be compared with the shim's flag table",
			}
		}
		run = flagrecord.ExecRunner(path)
	}
	installed, err := flagrecord.RecordFlagFile(run)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "bd's help could not be read, so the shim's flag table is unverified",
			Details: []string{err.Error()},
		}
	}
	if bytes.Equal(recorded, installed) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("the shim's flag table matches bd %s's help", flagrecord.Version(installed)),
		}
	}
	return c.drift(recorded, installed)
}

// drift reports how the recorded table differs from what bd's help records
// now: the first differing command and flag, and the refresh steps the
// recorded file itself documents.
func (c *BdShimFlagsCheck) drift(recorded, installed []byte) *CheckResult {
	recordedVersion, installedVersion := flagrecord.Version(recorded), flagrecord.Version(installed)
	result := &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("the shim's recorded flag table does not match bd %s's help", installedVersion),
		FixHint: "Refresh the table after the upgrade: go run ./cmd/bdshim-record -flags, then update internal/bdshim/flags.go to match",
	}
	drift, err := flagrecord.Compare(recorded, installed)
	if err != nil {
		// A file this check cannot read is unverified, never a pass: say what
		// it could not read rather than what differs.
		result.Status = StatusSkipped
		result.Message = "the shim's flag table could not be compared with bd's help"
		result.Details = []string{err.Error()}
		return result
	}
	if drift != nil {
		result.Message = fmt.Sprintf("%s: %s", result.Message, drift)
		result.Details = append(result.Details, fmt.Sprintf("recorded: %s", driftRow(drift.Recorded)),
			fmt.Sprintf("bd's help: %s", driftRow(drift.Installed)))
	}
	if recordedVersion != installedVersion {
		result.Details = append(result.Details, fmt.Sprintf(
			"the recorded table was recorded from bd %s, and bd %s is installed", recordedVersion, installedVersion))
	}
	result.Details = append(result.Details, "Report the drift without writing: go run ./cmd/bdshim-record -flags -check")
	result.Details = append(result.Details, flagrecord.Header(recorded)...)
	return result
}

// driftRow is one row's tokens as a report shows it, with a row the other side
// lacks written as none.
func driftRow(tokens []string) string {
	if len(tokens) == 0 {
		return "none"
	}
	return strings.Join(tokens, " ")
}
