package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestEscalateStaleReescalationCarriesThreadID: a re-escalation mail must
// carry the escalation id as its ThreadID, as the original escalation's mail
// does. Without it the router mints a random thread:<id> label, and
// `gt escalate close` — which closes the open delivery beads labelled
// thread:<escalation id> — never closes this one.
func TestEscalateStaleReescalationCarriesThreadID(t *testing.T) {
	t.Parallel()
	result := &beads.ReescalationResult{
		ID: "hq-kl7", Title: "disk full", OldSeverity: "medium",
		NewSeverity: "high", ReescalationNum: 1,
	}
	msg := reescalationMail(result, "gastown/witness", "gastown/crew/operator")
	if msg.ThreadID != result.ID {
		t.Fatalf("re-escalation ThreadID = %q, want the escalation id %q", msg.ThreadID, result.ID)
	}

	// The router labels the delivery bead thread:<ThreadID>; escalate close
	// closes exactly the beads carrying that label.
	db := beadsfake.New(beadsfake.WithPrefix("hq"))
	db.Seed(beads.Issue{
		ID: "hq-deliv", Title: "re-escalation", Status: "open",
		Labels: []string{"gt:message", "thread:" + msg.ThreadID},
	})
	n, err := closeEscalationDeliveryBeads(db, result.ID, "gastown/witness")
	if err != nil || n != 1 {
		t.Fatalf("closeEscalationDeliveryBeads = (%d, %v), want (1, nil)", n, err)
	}
	issue, err := db.Show("hq-deliv")
	if err != nil {
		t.Fatalf("show delivery bead: %v", err)
	}
	if issue.Status != "closed" {
		t.Errorf("delivery bead status = %q, want closed", issue.Status)
	}
}

// TestEscalateListJSONEmptyIsArray: with no live escalations the JSON list is
// [] rather than null, so a script can index it without a nil check.
func TestEscalateListJSONEmptyIsArray(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := listEscalations(&out, &errOut, beadsfake.New(beadsfake.WithPrefix("hq")), false, true); err != nil {
		t.Fatalf("listEscalations: %v", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if entries == nil {
		t.Fatalf("empty list JSON = %s, want []", strings.TrimSpace(out.String()))
	}
	if len(entries) != 0 {
		t.Fatalf("empty list JSON has %d entries: %s", len(entries), out.String())
	}
}

// TestEscalateStaleDryRunJSON: `gt escalate stale --dry-run --json` emits the
// JSON plan even though the dry run never reaches the real-run JSON branch.
func TestEscalateStaleDryRunJSON(t *testing.T) {
	t.Parallel()
	fields := &beads.EscalationFields{Severity: "medium", Reason: "disk full", EscalatedBy: "gastown/witness"}
	stale := []*beads.Issue{{
		ID:          "hq-esc1",
		Title:       "disk full",
		Description: beads.FormatEscalationDescription("disk full", fields),
	}}

	var out bytes.Buffer
	if err := writeEscalateStaleDryRun(&out, stale, "4h0m0s", 3, true); err != nil {
		t.Fatalf("writeEscalateStaleDryRun: %v", err)
	}
	var plan []beads.ReescalationResult
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("dry-run --json is not JSON: %v\n%s", err, out.String())
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d entries, want 1: %s", len(plan), out.String())
	}
	if plan[0].Skipped || plan[0].OldSeverity != "medium" || plan[0].NewSeverity != "high" {
		t.Errorf("plan = %+v, want an unskipped medium→high entry", plan[0])
	}
}
