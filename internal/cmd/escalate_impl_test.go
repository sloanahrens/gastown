package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/notify"
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
// Skipped entries stay in the plan with the reason they were skipped: a script
// reading the dry run needs to see the escalations it would leave alone, not
// only the ones it would bump.
func TestEscalateStaleDryRunJSON(t *testing.T) {
	t.Parallel()
	escalation := func(id, title, severity string, reescalations int) *beads.Issue {
		return &beads.Issue{
			ID:    id,
			Title: title,
			Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
				Severity:          severity,
				Reason:            "disk full",
				EscalatedBy:       "gastown/witness",
				ReescalationCount: reescalations,
			}),
		}
	}
	stale := []*beads.Issue{
		escalation("hq-esc1", "disk full", "medium", 0),
		escalation("hq-esc2", "already critical", "critical", 0),
		escalation("hq-esc3", "flapping", "high", 3),
	}

	var out bytes.Buffer
	if err := writeEscalateStaleDryRun(&out, stale, "4h0m0s", 3, true); err != nil {
		t.Fatalf("writeEscalateStaleDryRun: %v", err)
	}
	var plan []beads.ReescalationResult
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("dry-run --json is not JSON: %v\n%s", err, out.String())
	}
	want := []beads.ReescalationResult{
		{ID: "hq-esc1", Title: "disk full", OldSeverity: "medium", NewSeverity: "high", ReescalationNum: 1},
		{ID: "hq-esc2", Title: "already critical", OldSeverity: "critical", NewSeverity: "critical",
			Skipped: true, SkipReason: "already at critical severity"},
		{ID: "hq-esc3", Title: "flapping", OldSeverity: "high",
			Skipped: true, SkipReason: "already at max reescalations (3)"},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d entries, want %d: %s", len(plan), len(want), out.String())
	}
	for i, got := range plan {
		if got != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestPrintCreatedEscalationRoutedTo: the text "Routed to:" line lists every
// route action, not only the mail targets, so a HIGH escalation routed to a
// bead and email:human does not read as if nobody was told. An empty route
// prints "none".
func TestPrintCreatedEscalationRoutedTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		actions []string
		want    string
	}{
		{"bead and email", []string{"bead", "email:human"}, "  Routed to: bead, email:human"},
		{"empty route", nil, "  Routed to: none"},
		{"mail target kept", []string{"bead", "mail:gastown/witness", "email:human"},
			"  Routed to: bead, mail:gastown/witness, email:human"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := escalateRun{out: &out}
			res := &notify.RaiseResult{ID: "hq-esc1", Actions: tc.actions, Targets: notify.MailTargets(tc.actions)}
			r.printCreatedEscalation(res, "high")
			if !strings.Contains(out.String(), tc.want+"\n") {
				t.Errorf("output has no line %q:\n%s", tc.want, out.String())
			}
		})
	}
}
