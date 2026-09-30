package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

func TestDaemonNotifierDefaultsToGtAsDaemon(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: "/town"}, gtPath: "/usr/local/bin/gt"}
	cli, ok := d.notify().(*notify.CLI)
	if !ok {
		t.Fatalf("notify() = %T, want *notify.CLI", d.notify())
	}
	if cli.Bin != "/usr/local/bin/gt" || cli.Dir != "/town" {
		t.Errorf("CLI = %+v, want the resolved gt run from the town root", cli)
	}
	if env := cli.Env(); !slices.Contains(env, "BD_ACTOR=daemon") {
		t.Errorf("env lacks BD_ACTOR=daemon")
	}
}

func TestDoltServerManagerNotifierKeepsAnInjectedOne(t *testing.T) {
	t.Parallel()
	rec := notifyfake.New()
	m := &DoltServerManager{townRoot: "/town", notifier: rec}
	if got := m.notify(); got != rec {
		t.Fatalf("notify() = %T, want the injected recorder", got)
	}
}

// TestDoltServerManagerNotifierDefaultsToGtFromTheTownRoot pins the
// invocation the Dolt crash, crash-loop, unhealthy and read-only alert mails
// always used: gt from PATH, run from the town root, with the daemon's own
// environment.
func TestDoltServerManagerNotifierDefaultsToGtFromTheTownRoot(t *testing.T) {
	t.Parallel()
	m := &DoltServerManager{townRoot: "/town"}
	cli, ok := m.notify().(*notify.CLI)
	if !ok {
		t.Fatalf("notify() = %T, want *notify.CLI", m.notify())
	}
	if cli.Dir != "/town" || cli.Bin != "" || cli.Env != nil {
		t.Fatalf("CLI = %+v, want gt from PATH run from /town with the inherited env", cli)
	}
}

func TestNotifyWitnessMailsGoToTheRigWitness(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	d.notifyWitnessOfCrashedPolecat("gastown", "ruby", "gt-hook3")

	mails := rec.Mails()
	if len(mails) != 1 {
		t.Fatalf("mails = %+v, want 1", mails)
	}
	wantSubjects := []string{
		"CRASHED_POLECAT: gastown/ruby detected",
	}
	for i, m := range mails {
		if m.To != "gastown/witness" || m.Subject != wantSubjects[i] {
			t.Errorf("mail %d = to %q subject %q, want gastown/witness %q", i, m.To, m.Subject, wantSubjects[i])
		}
		if !strings.Contains(m.Body, "hook_bead: gt-hook") {
			t.Errorf("mail %d body lacks the hook bead: %q", i, m.Body)
		}
	}
}

func TestNudgeMayorGoesToTheMayor(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	d.ctx = t.Context()

	if err := d.nudgeMayor("2 free seats, 3 ready beads"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Nudges(); len(got) != 1 || got[0].Target != "mayor" || got[0].Message != "2 free seats, 3 ready beads" {
		t.Fatalf("nudges = %+v", got)
	}
}

func TestNudgeMayorReportsDeliveryFailure(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	d.ctx = t.Context()
	boom := errors.New("gt nudge: exit status 1 (Error: session not found)")
	rec.Fail(notifyfake.KindNudge, boom)

	if err := d.nudgeMayor("hello"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the delivery failure", err)
	}
}

func TestNudgeStalePatrolAddressesTheRole(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	d.ctx = t.Context()

	d.nudgeStalePatrol(patrolWatchdogTarget{Rig: "gastown", Role: "witness"})
	d.nudgeStalePatrol(patrolWatchdogTarget{Role: "deacon"})

	var targets []string
	for _, n := range rec.Nudges() {
		targets = append(targets, n.Target)
		if !strings.HasPrefix(n.Message, "resume patrol:") {
			t.Errorf("nudge message = %q", n.Message)
		}
	}
	if !slices.Equal(targets, []string{"gastown/witness", "deacon/"}) {
		t.Fatalf("targets = %q", targets)
	}
}

func TestSendDoltAlertToWitnessesMailsEveryRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"rigs":{"gastown":{},"beads":{}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := notifyfake.New()

	sendDoltAlertToWitnesses(rec, townRoot, "ALERT: Dolt server crashed", "body", func(string, ...interface{}) {})

	var to []string
	for _, m := range rec.Mails() {
		to = append(to, m.To)
		if m.Subject != "ALERT: Dolt server crashed" {
			t.Errorf("subject = %q", m.Subject)
		}
	}
	slices.Sort(to)
	if !slices.Equal(to, []string{"beads/witness", "gastown/witness"}) {
		t.Fatalf("mailed %q, want each rig's witness", to)
	}
}

func TestSendDoltAlertMailBoundsTheSend(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	n := deadlineSpy{onMail: func(ctx context.Context) { deadline, _ = ctx.Deadline() }}
	sendDoltAlertMail(n, "mayor/", "s", "b", func(string, ...interface{}) {})
	if deadline.IsZero() {
		t.Fatal("alert mail sent without a deadline")
	}
}

// deadlineSpy reports the context each mail send ran under.
type deadlineSpy struct {
	notify.Notifier
	onMail func(ctx context.Context)
}

func (s deadlineSpy) MailSend(ctx context.Context, _, _, _ string, _ ...notify.MailOption) error {
	s.onMail(ctx)
	return nil
}
