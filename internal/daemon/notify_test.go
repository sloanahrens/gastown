package daemon

import (
	"context"
	"errors"
	"slices"
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
