package cmd

import (
	"bytes"
	"io"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

func TestRunPatrolReportForRejectsNonPatrolRole(t *testing.T) {
	err := runPatrolReportFor(io.Discard, RoleInfo{Role: RoleCrew, Rig: "gastown", TownRoot: t.TempDir()}, "x", "", true)
	if err == nil {
		t.Fatal("runPatrolReportFor accepted a crew role; only deacon, witness and refinery patrol")
	}
}

// TestBuildStepAuditBareIDsRejected: a bare step id (no ":STATUS") must fail
// the CLI audit instead of being silently recorded as SKIP (gt-gvo8m). A deacon
// that ran a full 28/28 patrol but passed bare ids must not produce a ledger
// entry reading "0/28".
func TestBuildStepAuditBareIDsRejected(t *testing.T) {
	var out bytes.Buffer
	_, err := buildStepAudit(&out, constants.MolDeaconPatrol, "heartbeat,ack-probes,inbox-check")
	if err == nil {
		t.Fatal("buildStepAudit accepted bare step ids; the audit must reject a missing status, not default it to SKIP")
	}
}

// TestBuildStepAuditBareIDWithEmptyStatusRejected: "step:" parses to an empty
// status, which is the same missing-status defect as a bare id.
func TestBuildStepAuditBareIDWithEmptyStatusRejected(t *testing.T) {
	var out bytes.Buffer
	_, err := buildStepAudit(&out, constants.MolDeaconPatrol, "heartbeat:")
	if err == nil {
		t.Fatal("buildStepAudit accepted an empty status; \"heartbeat:\" has no status")
	}
}

// TestBuildStepAuditValidInputsUnchanged: well-formed step:STATUS pairs keep
// the pre-gt-gvo8m behavior, including the SKIP default for steps the agent
// omitted from the report.
func TestBuildStepAuditValidInputsUnchanged(t *testing.T) {
	var out bytes.Buffer
	audit, err := buildStepAudit(&out, constants.MolDeaconPatrol, "heartbeat:OK,ack-probes:OK,inbox-check:SKIP")
	if err != nil {
		t.Fatalf("buildStepAudit rejected valid input: %v", err)
	}
	for _, want := range []string{"heartbeat OK", "ack-probes OK", "inbox-check SKIP"} {
		if !contains(audit, want) {
			t.Errorf("audit missing %q: %s", want, audit)
		}
	}
	// Every reported step is SKIP/OK except the three reported; the deacon
	// formula has 28 steps, so a step the agent did not mention defaults to
	// SKIP and the OK count is exactly 2.
	if !contains(audit, "(2/28)") {
		t.Errorf("audit OK count = %s; want (2/28)", audit)
	}
	if contains(audit, "heartbeat SKIP") {
		t.Errorf("reported OK step read as SKIP: %s", audit)
	}
}

// TestBuildStepAuditUnknownStepIDWarns: an entry naming a step that is not in
// the formula is ignored for the audit line but must be warned about, so a
// typo cannot hide a skipped step.
func TestBuildStepAuditUnknownStepIDWarns(t *testing.T) {
	var out bytes.Buffer
	_, err := buildStepAudit(&out, constants.MolDeaconPatrol, "heartbeat:OK,heartbet:OK")
	if err != nil {
		t.Fatalf("buildStepAudit rejected a valid report containing an unknown step: %v", err)
	}
	if !contains(out.String(), "heartbet") {
		t.Errorf("no warning for unknown step %q: %s", "heartbet", out.String())
	}
}

// TestBuildStepAuditNoFormulaStillPrintsRaw: without a resolvable formula the
// audit prints the raw flag unvalidated; a bare id is unparseable, so it is an
// error in the CLI path, not a silent pass-through.
func TestBuildStepAuditNoFormulaStillPrintsRaw(t *testing.T) {
	var out bytes.Buffer

	audit, err := buildStepAudit(&out, "no-such-formula", "heartbeat:OK")
	if err != nil {
		t.Fatalf("buildStepAudit returned error for unvalidated steps: %v", err)
	}
	if !contains(audit, "heartbeat:OK") || !contains(audit, "unvalidated") {
		t.Errorf("unvalidated audit should print the raw flag: %s", audit)
	}

	_, err = buildStepAudit(&out, "no-such-formula", "heartbeat")
	if err == nil {
		t.Fatal("buildStepAudit accepted a bare id for an unvalidated formula; a missing status must not be invented")
	}
}

// TestBuildStepAuditEmptyFlagUnchanged: no --steps flag keeps the
// NOT REPORTED line for both the CLI and the unit cycle.
func TestBuildStepAuditEmptyFlagUnchanged(t *testing.T) {
	var out bytes.Buffer
	audit, err := buildStepAudit(&out, constants.MolDeaconPatrol, "")
	if err != nil {
		t.Fatalf("buildStepAudit returned error for empty flag: %v", err)
	}
	if !contains(audit, "NOT REPORTED (?/28)") {
		t.Errorf("empty flag should read NOT REPORTED (?/28): %s", audit)
	}
}