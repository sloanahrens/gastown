package refinery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// recorderOf returns the notifyfake.Recorder newTestEngineer installed on e.
func recorderOf(t *testing.T, e *Engineer) *notifyfake.Recorder {
	t.Helper()
	rec, ok := e.notifier.(*notifyfake.Recorder)
	if !ok {
		t.Fatalf("engineer notifier is %T, want *notifyfake.Recorder", e.notifier)
	}
	return rec
}

// sentLog renders every send e made, one per line, in the argv shape the gt
// stub these tests used to log: "nudge <target> <message>", "escalate -s
// <severity> <description> ..." and "mail send <to> ...".
func sentLog(t *testing.T, e *Engineer) string {
	t.Helper()
	var b strings.Builder
	for _, c := range recorderOf(t, e).Calls() {
		switch c.Kind {
		case notifyfake.KindNudge:
			fmt.Fprintf(&b, "nudge %s %s\n", c.Target, c.Message)
		case notifyfake.KindEscalate:
			x := c.Escalation
			fmt.Fprintf(&b, "escalate -s %s --reason %s %s\n", x.Severity, x.Reason, x.Description)
		case notifyfake.KindMail:
			fmt.Fprintf(&b, "mail send %s -s %s -m %s\n", c.To, c.Subject, c.Body)
		case notifyfake.KindClear:
			fmt.Fprintf(&b, "escalate clear %s\n", strings.Join(c.Fingerprints, " "))
		}
	}
	return b.String()
}

// witnessNudges returns the messages e nudged to its rig's witness.
func witnessNudges(t *testing.T, e *Engineer) []string {
	t.Helper()
	var msgs []string
	for _, n := range recorderOf(t, e).Nudges() {
		if n.Target == e.rig.Name+"/witness" {
			msgs = append(msgs, n.Message)
		}
	}
	return msgs
}

func TestManagerNotifyWorkerRejectedNudgesThePolecat(t *testing.T) {
	t.Parallel()
	rec := notifyfake.New()
	m := &Manager{rig: &rig.Rig{Name: "gastown"}, notifier: rec}

	m.notifyWorkerRejected(&MergeRequest{Worker: "polecats/nux", Branch: "polecat/nux/gt-1", IssueID: "gt-1"}, "tests failed")

	nudges := rec.Nudges()
	if len(nudges) != 1 || nudges[0].Target != "gastown/nux" {
		t.Fatalf("nudges = %+v, want one to gastown/nux", nudges)
	}
	want := "MR rejected: branch=polecat/nux/gt-1 issue=gt-1 reason=tests failed — review feedback and resubmit with 'gt done'"
	if nudges[0].Message != want {
		t.Errorf("message = %q\nwant      %q", nudges[0].Message, want)
	}
}

func TestEngineerNotifierDefaultsToGtFromTheGivenDir(t *testing.T) {
	t.Parallel()
	e := &Engineer{}
	cli, ok := e.notify("/town/gastown/refinery/rig").(*notify.CLI)
	if !ok || cli.Dir != "/town/gastown/refinery/rig" || cli.Bin != "" {
		t.Fatalf("notify() = %#v, want gt run from the given dir", e.notify("/x"))
	}
}
