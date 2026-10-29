package notify

import (
	"errors"
	"fmt"
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
		{"the human owns it: the whole route runs", attention.OwnerOperator, route},
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
	want := "imessage|Unhandled for 31 min: hq-polecat revert-guard ruling|"
	if len(pushed) != 1 || pushed[0] != want {
		t.Fatalf("pushed %v, want %q", pushed, want)
	}
}

// One pass is bounded: it pushes at most the cap, and every escalation it does
// not reach waits for a later pass unmarked. The following passes drain the
// rest in cap-sized chunks, so none is pushed twice and none is lost (gt-8db90).
func TestUnhandledRunBoundsOnePass(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	issues := make(map[string]*beads.Issue, 7)
	for i := range 7 {
		id := fmt.Sprintf("hq-%d", i)
		issues[id] = escalationBead(id, "daemon", now.Add(-time.Hour))
	}
	var pushed, marked []string
	run := UnhandledRun{
		List: func() ([]*beads.Issue, error) {
			out := make([]*beads.Issue, 0, len(issues))
			for i := range 7 {
				out = append(out, issues[fmt.Sprintf("hq-%d", i)])
			}
			return out, nil
		},
		Push: func(_, title, _ string) error { pushed = append(pushed, title); return nil },
		// The mark is the durable record the next pass reads: it is the label.
		Mark: func(id string) error {
			marked = append(marked, id)
			issues[id].Labels = append(issues[id].Labels, beads.EscalationPushedLabel)
			return nil
		},
		Name: "imessage", Window: config.DefaultEscalationUnhandledAfter,
		Max: 3,
	}
	pass := func() []string {
		ids, err := run.Run(now)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return ids
	}

	// Seven escalations, a cap of three: the first pass pushes three and leaves
	// the other four open and unmarked.
	if ids := pass(); strings.Join(ids, ",") != "hq-0,hq-1,hq-2" {
		t.Fatalf("first pass pushed %v, want the first three", ids)
	}
	if len(pushed) != 3 {
		t.Fatalf("pushed %d texts, want three", len(pushed))
	}
	// The mark is what carries the once-only, so the ones behind the cap must
	// not have one: a second text would be the human's price for the cap.
	if strings.Join(marked, ",") != "hq-0,hq-1,hq-2" {
		t.Errorf("marked %v, want the same three the pass pushed", marked)
	}

	// The rest drain over the passes that follow, in cap-sized chunks, and end
	// with nothing left to push.
	var drained []string
	for range 3 {
		drained = append(drained, pass()...)
	}
	if strings.Join(drained, ",") != "hq-3,hq-4,hq-5,hq-6" {
		t.Errorf("later passes pushed %v, want the remaining four", drained)
	}
	if len(pushed) != 7 || strings.Join(marked, ",") != "hq-0,hq-1,hq-2,hq-3,hq-4,hq-5,hq-6" {
		t.Errorf("after draining: %d texts, marked %v, want one of each", len(pushed), marked)
	}
	if ids := pass(); len(ids) != 0 {
		t.Errorf("a pass on a drained town pushed %v, want none", ids)
	}
}

// The cap bounds the time a pass spends, so it counts the pushes that fail too:
// a notify command that hangs costs its timeout per push, and a pass over more
// escalations than the cap must not pay it once per escalation (gt-8db90).
func TestUnhandledRunBoundsAPassOfFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var issues []*beads.Issue
	for i := range 7 {
		issues = append(issues, escalationBead(fmt.Sprintf("hq-%d", i), "daemon", now.Add(-time.Hour)))
	}
	tries := 0
	run := UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return issues, nil },
		Push:   func(string, string, string) error { tries++; return errors.New("timed out") },
		Mark:   func(string) error { t.Error("a failed push was recorded"); return nil },
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
		Max:    3,
	}
	ids, err := run.Run(now)
	if ids != nil {
		t.Errorf("Run pushed %v, want none recorded as pushed", ids)
	}
	if err == nil {
		t.Fatal("Run = nil, want the failures it had")
	}
	// Every push the pass made is named in the error; the escalations behind
	// the cap were not pushed and so have nothing to report.
	for _, id := range []string{"hq-0", "hq-1", "hq-2"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("Run error %v does not name %s", err, id)
		}
	}
	if tries != 3 {
		t.Errorf("pushed %d times, want the cap of three", tries)
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

// One escalation's failure does not stop the ones behind it: every escalation
// that has waited out the window is still owed its one push, and the pass
// reports the failure it had rather than abandoning the rest (gt-71714).
func TestUnhandledRunPushesTheRestWhenOneFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	first := escalationBead("hq-first", "daemon", now.Add(-time.Hour))
	second := escalationBead("hq-second", "daemon", now.Add(-time.Hour))
	var tried []string
	run := UnhandledRun{
		List: func() ([]*beads.Issue, error) { return []*beads.Issue{first, second}, nil },
		Push: func(_, title, _ string) error {
			tried = append(tried, title)
			if strings.Contains(title, first.ID) {
				return errors.New("timed out")
			}
			return nil
		},
		Mark: func(id string) error {
			if id == first.ID {
				t.Error("a failed push was recorded")
			}
			return nil
		},
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
	}
	ids, err := run.Run(now)
	if err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("Run = %v, want an error naming %s", err, first.ID)
	}
	if strings.Join(ids, ",") != second.ID {
		t.Errorf("pushed %v, want %s alone", ids, second.ID)
	}
	if len(tried) != 2 {
		t.Errorf("tried %v, want a push for both escalations", tried)
	}
}

// A push that reached the human and then could not be recorded is not sent
// again: the label is the record the next pass reads, so without a memory of
// the send a database that refused the mark would re-text the human every
// heartbeat (gt-71714).
func TestUnhandledRunRemembersAPushItCouldNotRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	unrecorded := escalationBead("hq-unrecorded", "daemon", now.Add(-time.Hour))
	recorded := escalationBead("hq-recorded", "daemon", now.Add(-time.Hour))
	var pushes []string
	run := UnhandledRun{
		List: func() ([]*beads.Issue, error) { return []*beads.Issue{unrecorded, recorded}, nil },
		Push: func(_, title, _ string) error { pushes = append(pushes, title); return nil },
		Mark: func(id string) error {
			if id == unrecorded.ID {
				return errors.New("connection refused")
			}
			recorded.Labels = append(recorded.Labels, beads.EscalationPushedLabel)
			return nil
		},
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
		memory: newPushMemory(),
	}
	ids, err := run.Run(now)
	if err == nil || !strings.Contains(err.Error(), unrecorded.ID) {
		t.Fatalf("Run = %v, want an error naming the record that failed", err)
	}
	// The text went out, so both count as pushed, and the failed record did not
	// stop the escalation behind it.
	if strings.Join(ids, ",") != "hq-unrecorded,hq-recorded" {
		t.Errorf("pushed %v, want both escalations", ids)
	}
	if len(pushes) != 2 {
		t.Fatalf("pushed %v, want a text for both escalations", pushes)
	}

	// The next pass reads the beads again: the recorded one is skipped by its
	// label, the unrecorded one by the memory, so nobody is texted twice.
	ids, err = run.Run(now)
	if err != nil || len(ids) != 0 || len(pushes) != 2 {
		t.Fatalf("second pass = %v, %v, %v, want nothing pushed again", ids, err, pushes)
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

// A town that configures no push command has nowhere to send, so the pass
// stops before it reads the escalation config or opens the beads store: a
// heartbeat pays nothing until the town has somewhere to push to (gt-71714).
func TestPushUnhandledDoesNothingWithoutAPushCommand(t *testing.T) {
	t.Parallel()
	ids, err := PushUnhandled(t.TempDir(), time.Now())
	if err != nil || len(ids) != 0 {
		t.Fatalf("PushUnhandled = %v, %v, want no ids and no error", ids, err)
	}
}

// A push names how long the escalation has waited, spelled by the size of the
// wait.
func TestUnhandledLineNamesTheWait(t *testing.T) {
	t.Parallel()
	cases := []struct {
		age  time.Duration
		want string
	}{
		{45 * time.Second, "Unhandled for 45s: hq-abc disk full"},
		{90 * time.Second, "Unhandled for 1 min: hq-abc disk full"},
		{31 * time.Minute, "Unhandled for 31 min: hq-abc disk full"},
		{59*time.Minute + 59*time.Second, "Unhandled for 59 min: hq-abc disk full"},
		{time.Hour, "Unhandled for 1h: hq-abc disk full"},
		{90 * time.Minute, "Unhandled for 1h: hq-abc disk full"},
		{119 * time.Minute, "Unhandled for 1h: hq-abc disk full"},
		{2 * time.Hour, "Unhandled for 2h: hq-abc disk full"},
		{5 * time.Hour, "Unhandled for 5h: hq-abc disk full"},
		{72 * time.Hour, "Unhandled for 3d: hq-abc disk full"},
	}
	for _, tc := range cases {
		u := UnhandledPush{ID: "hq-abc", Title: "disk full", Age: tc.age}
		if got := u.Line(); got != tc.want {
			t.Errorf("Line at age %v = %q, want %q", tc.age, got, tc.want)
		}
	}
}

// An escalation open for 3 days reads as 3 days, not as the 30 minute window
// that made it owed (gt-fasij).
func TestUnhandledRunNamesTheEscalationsAgeNotTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	issues := []*beads.Issue{escalationBead("hq-old", "daemon", now.Add(-72*time.Hour))}
	var pushed []string
	run := UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return issues, nil },
		Push:   func(name, title, body string) error { pushed = append(pushed, title); return nil },
		Name:   "imessage",
		Window: config.DefaultEscalationUnhandledAfter,
	}
	if _, err := run.Run(now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "Unhandled for 3d: hq-old revert-guard ruling"
	if len(pushed) != 1 || pushed[0] != want {
		t.Fatalf("pushed %v, want %q", pushed, want)
	}
}

// EscalationOwner is the rule the push gate reads: only the human's own session
// speaks for the human.
func TestEscalationOwner(t *testing.T) {
	t.Parallel()
	cases := map[string]attention.Owner{
		"overseer":                 attention.OwnerOperator,
		"gastown/overseer":         attention.OwnerOperator,
		"gastown/polecats/rictus":  attention.OwnerOverseer,
		"gastown/crew/sloan":       attention.OwnerOverseer,
		"daemon":                   attention.OwnerOverseer,
		"":                         attention.OwnerOverseer,
		"gastown/overseer/polecat": attention.OwnerOverseer,
		"  gastown/overseer  ":     attention.OwnerOperator,
	}
	for address, want := range cases {
		if got := attention.EscalationOwner(address); got != want {
			t.Errorf("EscalationOwner(%q) = %q, want %q", address, got, want)
		}
	}
}
