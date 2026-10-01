package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify"
)

// escalateFixture is an escalateRun over a temp town whose filing and
// clearing are recorded instead of reaching bd. The keyed upsert itself
// (one open bead per alert key, occurrences bumped on a repeat, clear closes
// only matching keys) is pinned in internal/beads' beads_escalation_alert_test.go
// and the key derivation in internal/notify's TestAlertKey.
type escalateFixture struct {
	r        escalateRun
	out      *strings.Builder
	raised   []notify.EscalationRequest
	cleared  [][]string
	raiseRes *notify.RaiseResult
	closed   []string
}

func newEscalateFixture(t *testing.T) *escalateFixture {
	t.Helper()
	town := t.TempDir()
	fx := &escalateFixture{out: &strings.Builder{}, raiseRes: &notify.RaiseResult{ID: "hq-created"}}
	fx.r = escalateRun{
		severity: "high",
		in:       strings.NewReader(""),
		out:      fx.out,
		townRoot: func() (string, error) { return town, nil },
		sender:   func() string { return "daemon" },
		raise: func(req notify.EscalationRequest, _ *config.EscalationConfig) (*notify.RaiseResult, error) {
			fx.raised = append(fx.raised, req)
			return fx.raiseRes, nil
		},
		clear: func(_ string, keys []string, _, _ string) ([]string, error) {
			fx.cleared = append(fx.cleared, keys)
			return fx.closed, nil
		},
	}
	return fx
}

// TestRunEscalate_FirstFiringCreatesOneKeyedBead: a firing hands notify the
// description, severity and sender it was given, so the alert lands under
// the key notify derives from them (gt-vwry).
func TestRunEscalate_FirstFiringCreatesOneKeyedBead(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.raiseRes.FingerprintLabel = notify.FingerprintLabel("main branch test failures:")

	if err := fx.r.escalate([]string{"main", "branch", "test", "failures:"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(fx.raised) != 1 {
		t.Fatalf("raised %d escalations, want 1", len(fx.raised))
	}
	req := fx.raised[0]
	if req.Description != "main branch test failures:" || req.Severity != "high" || req.EscalatedBy != "daemon" || req.Fingerprint != "" {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(fx.out.String(), "Escalation created: hq-created") {
		t.Errorf("output = %q", fx.out.String())
	}
}

// TestRunEscalate_RepeatFiringBumpsInsteadOfCreating: a firing notify recorded
// on the open escalation is reported as a repeat with its occurrence count.
func TestRunEscalate_RepeatFiringBumpsInsteadOfCreating(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.raiseRes = &notify.RaiseResult{ID: "hq-e1", Duplicate: true, Occurrences: 2, AlertKey: "main branch test failures:"}

	if err := fx.r.escalate([]string{"main branch test failures:"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	out := fx.out.String()
	if !strings.Contains(out, "Repeat escalation recorded on hq-e1 (occurrence 2)") || strings.Contains(out, "Escalation created") {
		t.Errorf("output = %q, want the repeat on hq-e1", out)
	}
}

// TestRunEscalate_StdinReadsTheReason: --stdin carries the reason, so it
// need not survive shell quoting.
func TestRunEscalate_StdinReadsTheReason(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.stdin = true
	fx.r.in = strings.NewReader("multi\nline reason\n")

	if err := fx.r.escalate([]string{"x"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got := fx.raised[0].Reason; got != "multi\nline reason" {
		t.Errorf("reason = %q", got)
	}
}

// TestRunEscalateClear_ClosesTheKeyAndNothingElse: --fingerprint keys go to
// notify verbatim, with nothing derived beside them.
func TestRunEscalateClear_ClosesTheKeyAndNothingElse(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.clearKeys = []string{"main_branch_test:failures"}
	fx.closed = []string{"hq-mine"}

	if err := fx.r.clearAlerts(nil); err != nil {
		t.Fatalf("clearAlerts: %v", err)
	}
	if len(fx.cleared) != 1 || strings.Join(fx.cleared[0], "|") != "main_branch_test:failures" {
		t.Errorf("cleared keys = %v", fx.cleared)
	}
	if !strings.Contains(fx.out.String(), "Cleared 1 escalation(s)") || !strings.Contains(fx.out.String(), "hq-mine") {
		t.Errorf("output = %q", fx.out.String())
	}
}

// TestRunEscalateClear_NothingToClearIsSuccess covers the healthy cycle. A
// producer clears its key on every pass, so a key that matches nothing is the
// ordinary case — if it were an error, a clean patrol would report itself as
// failed and the daemon would log noise on every tick.
func TestRunEscalateClear_NothingToClearIsSuccess(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.clearKeys = []string{"jsonl_git_backup:spike"}

	if err := fx.r.clearAlerts(nil); err != nil {
		t.Fatalf("clearing an absent key must succeed: %v", err)
	}
	if !strings.Contains(fx.out.String(), "Nothing to clear") {
		t.Errorf("output = %q", fx.out.String())
	}
}

// TestRunEscalateClear_DerivesKeyFromSourceAndDescription keeps the two halves
// of a keyed alert symmetrical: a producer that raised an alert with a source
// and a description must be able to clear it the same way, with no bookkeeping
// of the hashed label on its side.
func TestRunEscalateClear_DerivesKeyFromSourceAndDescription(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.source = "main_branch_test"

	if err := fx.r.clearAlerts([]string{"main", "branch", "test", "failures:"}); err != nil {
		t.Fatalf("clearAlerts: %v", err)
	}
	if len(fx.cleared) != 1 || strings.Join(fx.cleared[0], "|") != "main_branch_test: main branch test failures:" {
		t.Errorf("cleared keys = %v, want the key raise derives", fx.cleared)
	}
}

// TestRunEscalateClear_RequiresAKey guards against a caller clearing "the"
// escalation by accident: with neither a key nor a description there is nothing
// to match, and clearing everything open would be far worse than an error.
func TestRunEscalateClear_RequiresAKey(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	if err := fx.r.clearAlerts(nil); err == nil {
		t.Fatal("expected an error when no key or description is given")
	}
	if len(fx.cleared) != 0 {
		t.Errorf("clear reached notify: %v", fx.cleared)
	}
}

// TestRunEscalateClear_ReportsAClearFailure: a clear notify could not make is
// the command's error.
func TestRunEscalateClear_ReportsAClearFailure(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.clearKeys = []string{"k"}
	fx.r.clear = func(string, []string, string, string) ([]string, error) { return nil, errors.New("dolt down") }
	if err := fx.r.clearAlerts(nil); err == nil || !strings.Contains(err.Error(), "dolt down") {
		t.Fatalf("clearAlerts = %v, want the clear failure", err)
	}
}
