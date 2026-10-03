package beads_test

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// The escalation helpers are free functions over beads.Client, so they run
// against beadsfake the way they run against *Beads. These tests drive the
// fake: they cover the Client path (what any non-*Beads store executes), while
// beads_escalation_test.go and beads_escalation_alert_test.go keep the
// bd-backed path's argv and routing pinned with a bd stub.

// escalationOnFake files one escalation through the Client path on a fake and
// returns the fake and the bead.
func escalationOnFake(t *testing.T, fingerprint string) (*beadsfake.Fake, *beads.Issue) {
	t.Helper()
	f := beadsfake.New(beadsfake.WithActor("daemon"))
	issue := fileEscalationOnFake(t, f, fingerprint, "high")
	return f, issue
}

func fileEscalationOnFake(t *testing.T, f *beadsfake.Fake, fingerprint, severity string) *beads.Issue {
	t.Helper()
	issue, err := beads.CreateEscalationBead(f, "main_branch_test: failures", &beads.EscalationFields{
		Severity:    severity,
		Reason:      "main branch tests red",
		Source:      "main_branch_test",
		EscalatedBy: "daemon",
		EscalatedAt: "2026-10-02T00:00:00Z",
		Fingerprint: fingerprint,
	})
	if err != nil {
		t.Fatalf("CreateEscalationBead: %v", err)
	}
	return issue
}

func escalationIDs(issues []*beads.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func escalationContains(issues []*beads.Issue, id string) bool {
	for _, issue := range issues {
		if issue.ID == id {
			return true
		}
	}
	return false
}

// TestEscalationHelpersOverClientCreateFilesAWispWithItsLabels covers the
// create any Client runs: the family label, the severity label, the
// fingerprint label and the formatted multi-line description all land on one
// bead, and the listings find it.
func TestEscalationHelpersOverClientCreateFilesAWispWithItsLabels(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	for _, label := range []string{"gt:escalation", "severity:high", "escalation-fp:abc123"} {
		if !beads.HasLabel(issue, label) {
			t.Errorf("created escalation is missing label %q; got %v", label, issue.Labels)
		}
	}
	fields := beads.ParseEscalationFields(issue.Description)
	if fields.Severity != "high" || fields.Reason != "main branch tests red" || fields.Fingerprint != "escalation-fp:abc123" {
		t.Errorf("description round-trips to %+v, want the fields the create was given", fields)
	}

	open, err := beads.ListEscalations(f)
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if !escalationContains(open, issue.ID) {
		t.Errorf("ListEscalations = %v, want it to hold the escalation it just created", escalationIDs(open))
	}

	byKey, err := beads.ListEscalationsByFingerprint(f, "escalation-fp:abc123")
	if err != nil {
		t.Fatalf("ListEscalationsByFingerprint: %v", err)
	}
	if len(byKey) != 1 || byKey[0].ID != issue.ID {
		t.Errorf("ListEscalationsByFingerprint = %v, want just %s", escalationIDs(byKey), issue.ID)
	}

	high, err := beads.ListEscalationsBySeverity(f, "high")
	if err != nil {
		t.Fatalf("ListEscalationsBySeverity(high): %v", err)
	}
	if len(high) != 1 {
		t.Errorf("ListEscalationsBySeverity(high) = %v, want the high escalation", escalationIDs(high))
	}
	low, err := beads.ListEscalationsBySeverity(f, "low")
	if err != nil {
		t.Fatalf("ListEscalationsBySeverity(low): %v", err)
	}
	if len(low) != 0 {
		t.Errorf("ListEscalationsBySeverity(low) = %v, want none", escalationIDs(low))
	}
}

// TestEscalationHelpersOverClientListSkipsMailCarriers keeps the gt:message
// carriers a routed escalation leaves behind out of the open-only view, which
// is what makes `gt escalate list` and the re-escalation flow agree on how
// many escalations there are (gt-9k2bx).
func TestEscalationHelpersOverClientListSkipsMailCarriers(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	if _, err := f.Create(beads.CreateOptions{
		Title:    "escalation mail",
		Labels:   []string{"gt:escalation", "gt:message"},
		Priority: -1,
	}); err != nil {
		t.Fatalf("Create carrier: %v", err)
	}

	open, err := beads.ListEscalations(f)
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if len(open) != 1 || open[0].ID != issue.ID {
		t.Errorf("ListEscalations = %v, want only the escalation %s", escalationIDs(open), issue.ID)
	}

	all, err := beads.ListAllEscalationsAcrossRigs(f)
	if err != nil {
		t.Fatalf("ListAllEscalationsAcrossRigs: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListAllEscalationsAcrossRigs = %v, want the escalation and its carrier", escalationIDs(all))
	}
}

// TestEscalationHelpersOverClientBumpRecordsRepeatOnTheOneBead is the gt-vwry
// property over the Client path: a second firing of the same alert updates the
// bead that already represents it instead of minting another.
func TestEscalationHelpersOverClientBumpRecordsRepeatOnTheOneBead(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	occurrences, renotify, err := beads.BumpEscalation(f, issue.ID, "high", "still failing", "main_branch_test", time.Hour)
	if err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if occurrences != 2 {
		t.Errorf("occurrences = %d, want 2 (the create counts as the first firing)", occurrences)
	}
	if !renotify {
		t.Error("a bead whose escalated_at is long past its last notification must renotify")
	}

	open, err := beads.ListEscalations(f)
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("ListEscalations = %v, want one bead — the repeat must not mint a second", escalationIDs(open))
	}
	fields := beads.ParseEscalationFields(open[0].Description)
	if fields.Occurrences != 2 || fields.Reason != "still failing" {
		t.Errorf("bumped fields = %+v, want the recurrence and the latest reason on the bead", fields)
	}
}

// TestEscalationHelpersOverClientAckThenClose walks the ack-then-close
// lifecycle the way `gt escalate ack` and `gt escalate close` drive it.
func TestEscalationHelpersOverClientAckThenClose(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	if err := beads.AckEscalation(f, issue.ID, "gastown/crew/sloan"); err != nil {
		t.Fatalf("AckEscalation: %v", err)
	}
	got, fields, err := beads.GetEscalationBead(f, issue.ID)
	if err != nil {
		t.Fatalf("GetEscalationBead: %v", err)
	}
	if fields.AckedBy != "gastown/crew/sloan" || fields.AckedAt == "" {
		t.Errorf("acked fields = %+v, want the acking agent and a timestamp", fields)
	}
	if !beads.HasLabel(got, "acked") {
		t.Errorf("acked escalation lacks the acked label; got %v", got.Labels)
	}

	if err := beads.CloseEscalation(f, issue.ID, "gastown/crew/sloan", "tests are green"); err != nil {
		t.Fatalf("CloseEscalation: %v", err)
	}
	closed, fields, err := beads.GetEscalationBead(f, issue.ID)
	if err != nil {
		t.Fatalf("GetEscalationBead after close: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("status = %q, want closed", closed.Status)
	}
	if fields.ClosedBy != "gastown/crew/sloan" || fields.ClosedReason != "tests are green" {
		t.Errorf("closed fields = %+v, want the closer and the reason", fields)
	}
	if fields.AckedBy != "gastown/crew/sloan" {
		t.Errorf("close dropped the ack: %+v", fields)
	}

	open, err := beads.ListEscalations(f)
	if err != nil {
		t.Fatalf("ListEscalations after close: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("ListEscalations = %v, want none once the escalation is closed", escalationIDs(open))
	}
}

// TestEscalationHelpersOverClientClearClosesOnlyTheClearedKey is the gt-vwry
// clear half: a producer clearing its own key must not reach another
// condition's alert.
func TestEscalationHelpersOverClientClearClosesOnlyTheClearedKey(t *testing.T) {
	f := beadsfake.New(beadsfake.WithActor("daemon"))
	mine := fileEscalationOnFake(t, f, "escalation-fp:aaa111", "high")
	other := fileEscalationOnFake(t, f, "escalation-fp:bbb222", "high")

	closed, err := beads.CloseEscalationsByFingerprint(f, "escalation-fp:aaa111", "daemon", "main branch tests green")
	if err != nil {
		t.Fatalf("CloseEscalationsByFingerprint: %v", err)
	}
	if len(closed) != 1 || closed[0] != mine.ID {
		t.Fatalf("closed = %v, want just [%s]", closed, mine.ID)
	}

	open, err := beads.ListEscalations(f)
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if escalationContains(open, mine.ID) {
		t.Errorf("cleared escalation %s is still open", mine.ID)
	}
	if !escalationContains(open, other.ID) {
		t.Errorf("clearing one key closed %s, whose key was not cleared", other.ID)
	}
}

// TestEscalationHelpersOverClientReescalateBumpsSeverity covers the
// threshold-driven bump `gt escalate stale` runs.
func TestEscalationHelpersOverClientReescalateBumpsSeverity(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	result, err := beads.ReescalateEscalation(f, issue.ID, "deacon", 3)
	if err != nil {
		t.Fatalf("ReescalateEscalation: %v", err)
	}
	if result.OldSeverity != "high" || result.NewSeverity != "critical" || result.Skipped {
		t.Fatalf("result = %+v, want high -> critical", result)
	}

	got, fields, err := beads.GetEscalationBead(f, issue.ID)
	if err != nil {
		t.Fatalf("GetEscalationBead: %v", err)
	}
	if fields.Severity != "critical" || fields.ReescalationCount != 1 {
		t.Errorf("fields = %+v, want severity critical at reescalation 1", fields)
	}
	if fields.OriginalSeverity != "high" {
		t.Errorf("original_severity = %q, want the severity before the bump", fields.OriginalSeverity)
	}
	if !beads.HasLabel(got, "reescalated") || !beads.HasLabel(got, "severity:critical") {
		t.Errorf("labels = %v, want reescalated and severity:critical", got.Labels)
	}
}

// TestEscalationHelpersOverClientListStaleEscalations keeps the stale
// threshold — the input to re-escalation and its mail — working over any
// Client, and keeps an acknowledged escalation out of it: an ack means a human
// is already on it, so re-escalating it would page them twice.
func TestEscalationHelpersOverClientListStaleEscalations(t *testing.T) {
	f, issue := escalationOnFake(t, "escalation-fp:abc123")

	// The fake's clock starts at its epoch, well before now, so an escalation
	// filed there is past any positive threshold.
	stale, err := beads.ListStaleEscalations(f, time.Hour)
	if err != nil {
		t.Fatalf("ListStaleEscalations: %v", err)
	}
	if len(stale) != 1 || stale[0].ID != issue.ID {
		t.Fatalf("stale = %v, want %s", escalationIDs(stale), issue.ID)
	}

	if err := beads.AckEscalation(f, issue.ID, "gastown/crew/sloan"); err != nil {
		t.Fatalf("AckEscalation: %v", err)
	}
	stale, err = beads.ListStaleEscalations(f, time.Hour)
	if err != nil {
		t.Fatalf("ListStaleEscalations after ack: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("an acked escalation must not be stale, got %v", escalationIDs(stale))
	}
}

// TestEscalationHelpersOverClientRefuseNonEscalationBeads pins the guard both
// mutating paths share: a bead without the family label is not something these
// helpers will touch.
func TestEscalationHelpersOverClientRefuseNonEscalationBeads(t *testing.T) {
	f := beadsfake.New()
	plain, err := f.Create(beads.CreateOptions{Title: "ordinary work", Labels: []string{"gt:task"}, Priority: -1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, _, err := beads.GetEscalationBead(f, plain.ID); err == nil {
		t.Error("GetEscalationBead accepted a bead that is not an escalation")
	}
	if err := beads.AckEscalation(f, plain.ID, "someone"); err == nil {
		t.Error("AckEscalation accepted a bead that is not an escalation")
	}
	if err := beads.CloseEscalation(f, plain.ID, "someone", "reason"); err == nil {
		t.Error("CloseEscalation accepted a bead that is not an escalation")
	}
	if _, _, err := beads.BumpEscalation(f, plain.ID, "high", "reason", "source", time.Hour); err == nil {
		t.Error("BumpEscalation accepted a bead that is not an escalation")
	}

	if _, _, err := beads.GetEscalationBead(f, "gt-absent"); err != nil {
		t.Errorf("a missing escalation must read as absent, not as an error: %v", err)
	} else if _, err := f.Show("gt-absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("fixture: gt-absent should not exist, got %v", err)
	}
}

// TestEscalationHelpersOverClientRefuseFlagLikeTitles keeps the gt-e0kx5 guard
// on the free function, not only on *Beads.
func TestEscalationHelpersOverClientRefuseFlagLikeTitles(t *testing.T) {
	f := beadsfake.New()
	if _, err := beads.CreateEscalationBead(f, "--help", &beads.EscalationFields{Severity: "low"}); err == nil {
		t.Fatal("CreateEscalationBead accepted a flag-like title")
	}
}
