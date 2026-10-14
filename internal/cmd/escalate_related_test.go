package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/patrolscan"
)

// polecatEscalateFixture is an escalate fixture inside furiosa's polecat
// session in the gastown rig, with hooked as the bead the lookup finds.
func polecatEscalateFixture(t *testing.T, hooked string) (*escalateFixture, *strings.Builder) {
	t.Helper()
	fx := newEscalateFixture(t)
	warned := &strings.Builder{}
	fx.r.rig, fx.r.polecat = "gastown", "furiosa"
	fx.r.sender = func() string { return "gastown/polecats/furiosa" }
	fx.r.errOut = warned
	fx.r.hookedBead = func(_, seat string) string {
		if seat != "gastown/polecats/furiosa" {
			t.Errorf("hooked bead looked up for %q, want gastown/polecats/furiosa", seat)
		}
		return hooked
	}
	return fx, warned
}

// TestEscalate_PolecatEscalationNamesHookedBead: a polecat's free-text
// escalation carries the hooked bead as related_bead, so the supervisor's
// verdict holds the seat while it is open and releases it once it is closed.
func TestEscalate_PolecatEscalationNamesHookedBead(t *testing.T) {
	t.Parallel()
	fx, warned := polecatEscalateFixture(t, "gt-x9cav")

	if err := fx.r.escalate([]string{"need", "a", "decision"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(fx.raised) != 1 {
		t.Fatalf("raised %d escalations, want 1", len(fx.raised))
	}
	req := fx.raised[0]
	if req.RelatedBead != "gt-x9cav" {
		t.Fatalf("related bead = %q, want gt-x9cav", req.RelatedBead)
	}
	if warned.Len() != 0 {
		t.Errorf("unexpected warning: %q", warned.String())
	}

	filed := func(open bool) []patrolscan.Escalation {
		return []patrolscan.Escalation{{
			ID: "hq-esc1", EscalatedBy: req.EscalatedBy, Title: req.Description,
			RelatedBead: req.RelatedBead, Open: open,
		}}
	}
	if got := patrolscan.SeatEscalationVerdict("gastown", "furiosa", "gt-x9cav", filed(true)); got.Verdict != patrolscan.EscalationPending {
		t.Errorf("open escalation verdict = %q, want %q", got.Verdict, patrolscan.EscalationPending)
	}
	if got := patrolscan.SeatEscalationVerdict("gastown", "furiosa", "gt-x9cav", filed(false)); got.Verdict != patrolscan.EscalationResolved {
		t.Errorf("closed escalation verdict = %q, want %q", got.Verdict, patrolscan.EscalationResolved)
	}
}

func TestEscalate_ExplicitRelatedWins(t *testing.T) {
	t.Parallel()
	fx, _ := polecatEscalateFixture(t, "gt-x9cav")
	fx.r.relatedBead = "gt-other"
	fx.r.hookedBead = func(_, _ string) string {
		t.Error("hooked bead looked up although --related was given")
		return "gt-x9cav"
	}

	if err := fx.r.escalate([]string{"need", "a", "decision"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got := fx.raised[0].RelatedBead; got != "gt-other" {
		t.Errorf("related bead = %q, want gt-other", got)
	}
}

func TestEscalate_NoPolecatContextLeavesRelatedEmpty(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.hookedBead = func(_, _ string) string {
		t.Error("hooked bead looked up outside a polecat session")
		return "gt-x9cav"
	}

	if err := fx.r.escalate([]string{"need", "a", "decision"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got := fx.raised[0].RelatedBead; got != "" {
		t.Errorf("related bead = %q, want empty", got)
	}
}

// TestEscalate_HookLookupFailureStillRaises: a polecat with no findable hook
// still files the escalation, unnamed, with a one-line warning on stderr.
func TestEscalate_HookLookupFailureStillRaises(t *testing.T) {
	t.Parallel()
	fx, warned := polecatEscalateFixture(t, "")

	if err := fx.r.escalate([]string{"need", "a", "decision"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(fx.raised) != 1 || fx.raised[0].RelatedBead != "" {
		t.Fatalf("raised = %+v, want one escalation with no related bead", fx.raised)
	}
	if got := warned.String(); !strings.HasPrefix(got, "Warning: ") || strings.Count(got, "\n") != 1 {
		t.Errorf("warning = %q, want one Warning: line", got)
	}
}

func TestEscalate_DryRunShowsHookedBead(t *testing.T) {
	t.Parallel()
	fx, _ := polecatEscalateFixture(t, "gt-x9cav")
	fx.r.dryRun = true

	if err := fx.r.escalate([]string{"need", "a", "decision"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(fx.raised) != 0 {
		t.Errorf("dry run raised %d escalations", len(fx.raised))
	}
	if !strings.Contains(fx.out.String(), "Related: gt-x9cav") {
		t.Errorf("dry run output = %q, want the hooked bead", fx.out.String())
	}
}
