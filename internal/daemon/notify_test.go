package daemon

import (
	"context"
	"errors"
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
}

// TestDaemonEscalationsGoOutAsTheDaemon: every automated alert the daemon
// raises must travel through the notifier that names the daemon — BD_ACTOR
// with no other identity variable (daemonGTEnv, gt-kyik6). gt escalate
// derives the sender of the escalation it files from that environment, so a
// notifier built any other way attributes the alert to the human operator:
// that is how a daemon-raised escalation read "Escalated by: overseer"
// (gt-bw6ai). The alert path must also use that notifier, not one of its own.
func TestDaemonEscalationsGoOutAsTheDaemon(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: t.TempDir()}, gtPath: "gt"}
	cli, ok := d.notify().(*notify.CLI)
	if !ok || cli.Env == nil {
		t.Fatalf("daemon notifier = %+v, want a notify.CLI with an explicit env", d.notify())
	}
	assertDaemonIdentity(t, "escalation notifier", cli.Env())

	rec := notifyfake.New()
	d.notifier = rec
	if err := d.escalateAlertSeverity("low", "landing:slow", "landing_slow", "landing took 12m"); err != nil {
		t.Fatalf("escalateAlertSeverity: %v", err)
	}
	if got := rec.Escalations(); len(got) != 1 || got[0].Escalation.Severity != "low" || got[0].Escalation.Fingerprint != "landing:slow" {
		t.Fatalf("escalations = %+v, want the one low alert under its key", got)
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
// always used: gt from PATH, run from the town root, as the daemon
// (TestDaemonNotifiersRunGtAsTheDaemon checks the env).
func TestDoltServerManagerNotifierDefaultsToGtFromTheTownRoot(t *testing.T) {
	t.Parallel()
	m := &DoltServerManager{townRoot: "/town"}
	cli, ok := m.notify().(*notify.CLI)
	if !ok {
		t.Fatalf("notify() = %T, want *notify.CLI", m.notify())
	}
	if cli.Dir != "/town" || cli.Bin != "" {
		t.Fatalf("CLI = %+v, want gt from PATH run from /town", cli)
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

func TestRaiseDoltAlertBoundsTheSend(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	n := deadlineSpy{onEscalate: func(ctx context.Context) { deadline, _ = ctx.Deadline() }}
	raiseDoltAlert(n, "high", "dolt:crash-loop", "s", "b", func(string, ...interface{}) {})
	if deadline.IsZero() {
		t.Fatal("alert escalation raised without a deadline")
	}
}

// deadlineSpy reports the context each escalation ran under.
type deadlineSpy struct {
	notify.Notifier
	onEscalate func(ctx context.Context)
}

func (s deadlineSpy) Escalate(ctx context.Context, _ notify.Escalation) error {
	s.onEscalate(ctx)
	return nil
}
