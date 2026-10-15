package notify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// escalationBead is an open escalation as the unhandled pass reads one: the
// fields live in the description, as CreateEscalationBead writes them.
func escalationBead(id, escalatedBy string, raised time.Time, labels ...string) *beads.Issue {
	fields := &beads.EscalationFields{
		Severity:    "high",
		EscalatedBy: escalatedBy,
		EscalatedAt: raised.Format(time.RFC3339),
	}
	description := beads.FormatEscalationDescription("revert-guard ruling", fields)
	return &beads.Issue{
		ID: id, Title: "revert-guard ruling", Description: description,
		Status: "open", Labels: append([]string{"gt:escalation", "severity:high"}, labels...),
		CreatedAt: raised.Format(time.RFC3339),
	}
}

// A push that reaches the human is about what the human must act on: the route
// actions for the agent channels run whatever the owner is, and the ones that
// reach a person run only when the escalation is the human's (gt-71714).
func TestPushActionsReachTheHumanOnlyForTheHumansEscalation(t *testing.T) {
	t.Parallel()
	route := []string{"bead", "mail:gastown/overseer", "exec:imessage", "email:human", "sms:human", "slack", "log"}
	cases := []struct {
		name  string
		owner attention.Owner
		want  []string
	}{
		{"the human owns it: the whole route runs", attention.OwnerSloan, route},
		{"the overseer owns it: nothing reaches a person", attention.OwnerOverseer,
			[]string{"bead", "mail:gastown/overseer", "log"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PushActions(route, tc.owner); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("PushActions(%v, %s) = %v, want %v", route, tc.owner, got, tc.want)
			}
		})
	}
}

// A polecat's escalation to the overseer pushes nothing when it is filed, and
// one still open past the window is pushed once, naming its id and title and
// never its body (gt-71714).
func TestUnhandledRunPushesOnceAfterTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	issues := []*beads.Issue{
		escalationBead("hq-polecat", "gastown/polecats/rictus", now.Add(-31*time.Minute)),
		// Still inside the window: the overseer may yet act on it.
		escalationBead("hq-fresh", "daemon", now.Add(-29*time.Minute)),
		// Already pushed once: a restart must not say it again.
		escalationBead("hq-pushed", "daemon", now.Add(-2*time.Hour), beads.EscalationPushedLabel),
		// Acknowledged by an agent: someone is on it.
		escalationBead("hq-acked", "daemon", now.Add(-2*time.Hour), "acked"),
		// The human raised it, so it pushed when it was filed.
		escalationBead("hq-human", attention.HumanAddress, now.Add(-2*time.Hour)),
	}
	var pushed, marked []string
	run := UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return issues, nil },
		Push:   func(name, title, body string) error { pushed = append(pushed, name+"|"+title+"|"+body); return nil },
		Mark:   func(id string) error { marked = append(marked, id); return nil },
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
	}
	ids, err := run.Run(now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(ids, ",") != "hq-polecat" || strings.Join(marked, ",") != "hq-polecat" {
		t.Fatalf("pushed %v, marked %v, want hq-polecat alone", ids, marked)
	}
	want := "imessage|Unhandled for 30 min: hq-polecat revert-guard ruling|"
	if len(pushed) != 1 || pushed[0] != want {
		t.Fatalf("pushed %v, want %q", pushed, want)
	}
}

// A second pass over the same bead pushes nothing: the first push is recorded
// on the bead, which is what carries the once-only across a daemon restart.
func TestUnhandledRunDoesNotPushTheSameEscalationTwice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	bead := escalationBead("hq-polecat", "gastown/polecats/rictus", now.Add(-time.Hour))
	run := UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return []*beads.Issue{bead}, nil },
		Push:   func(string, string, string) error { return nil },
		Mark:   func(string) error { bead.Labels = append(bead.Labels, beads.EscalationPushedLabel); return nil },
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
	}
	if ids, err := run.Run(now); err != nil || len(ids) != 1 {
		t.Fatalf("first pass = %v, %v, want one push", ids, err)
	}
	if ids, err := run.Run(now); err != nil || len(ids) != 0 {
		t.Fatalf("second pass = %v, %v, want none", ids, err)
	}
}

// A town with no notify command pushes nothing, and a failed push is reported
// without being recorded, so the next pass tries it again.
func TestUnhandledRunWithoutACommandAndOnPushFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	bead := escalationBead("hq-polecat", "daemon", now.Add(-time.Hour))
	calls := 0
	run := UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return []*beads.Issue{bead}, nil },
		Push:   func(string, string, string) error { calls++; return errors.New("timed out") },
		Mark:   func(string) error { t.Error("a failed push was recorded"); return nil },
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
	}
	if _, err := run.Run(now); err == nil || !strings.Contains(err.Error(), "hq-polecat") {
		t.Fatalf("Run = %v, want an error naming the escalation", err)
	}
	if calls != 1 {
		t.Errorf("pushed %d times, want once", calls)
	}

	run.Name = ""
	run.Push = func(string, string, string) error { t.Error("pushed with no command configured"); return nil }
	if ids, err := run.Run(now); err != nil || len(ids) != 0 {
		t.Fatalf("Run with no command = %v, %v, want nothing", ids, err)
	}
}

// The same window spelled differently reaches the message: a push names how
// long the escalation waited.
func TestUnhandledLineNamesTheWait(t *testing.T) {
	t.Parallel()
	u := UnhandledPush{ID: "hq-abc", Title: "disk full"}
	if got, want := u.Line(90*time.Minute), "Unhandled for 90 min: hq-abc disk full"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	if got, want := u.Line(45*time.Second), "Unhandled for 45s: hq-abc disk full"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
}

// EscalationOwner is the rule the push gate reads: only the human's own session
// speaks for the human.
func TestEscalationOwner(t *testing.T) {
	t.Parallel()
	cases := map[string]attention.Owner{
		"overseer":                 attention.OwnerSloan,
		"gastown/overseer":         attention.OwnerSloan,
		"gastown/polecats/rictus":  attention.OwnerOverseer,
		"gastown/crew/sloan":       attention.OwnerOverseer,
		"daemon":                   attention.OwnerOverseer,
		"":                         attention.OwnerOverseer,
		"gastown/overseer/polecat": attention.OwnerOverseer,
		"  gastown/overseer  ":     attention.OwnerSloan,
	}
	for address, want := range cases {
		if got := attention.EscalationOwner(address); got != want {
			t.Errorf("EscalationOwner(%q) = %q, want %q", address, got, want)
		}
	}
}
