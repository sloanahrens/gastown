package notify

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// recordingRunner is a commandRunner that records each call and answers with
// err(argv), so no unit test starts a process.
type recordingRunner struct {
	mu    sync.Mutex
	argvs [][]string
	stdin []string
	err   func(argv []string) error
}

func (r *recordingRunner) run(argv []string, stdin string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argvs = append(r.argvs, argv)
	r.stdin = append(r.stdin, stdin)
	if r.err == nil {
		return nil
	}
	return r.err(argv)
}

func notifyConfig(commands map[string][]string) *config.EscalationConfig {
	return &config.EscalationConfig{Notify: config.NotifyConfig{Commands: commands}}
}

func TestPushMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, title, body, want string
	}{
		{"title only", "gastown notify test", "", "gastown notify test\n"},
		{"title and body", "Disk full", "rig gastown\nfree: 1G", "Disk full\nrig gastown\nfree: 1G\n"},
		{"title is one line", "Disk\nfull\t now ", "body", "Disk full now\nbody\n"},
		{"trailing newlines of the body collapse", "T", "body\n\n\n", "T\nbody\n"},
	}
	for _, tc := range tests {
		if got := pushMessage(tc.title, tc.body); got != tc.want {
			t.Errorf("%s: pushMessage(%q, %q) = %q, want %q", tc.name, tc.title, tc.body, got, tc.want)
		}
	}
}

func TestPushThroughRunsTheConfiguredArgvWithTheMessageOnStdin(t *testing.T) {
	t.Parallel()
	argv := []string{"curl", "-fsS", "--data-binary", "@-", "https://ntfy.sh/REPLACE_WITH_PRIVATE_TOPIC"}
	cfg := notifyConfig(map[string][]string{"ntfy": argv, "other": {"true"}})
	rr := &recordingRunner{}

	if err := pushThrough(rr.run, cfg, "ntfy", "Title here", "Body here"); err != nil {
		t.Fatalf("pushThrough: %v", err)
	}
	if len(rr.argvs) != 1 || !reflect.DeepEqual(rr.argvs[0], argv) {
		t.Errorf("ran %v, want exactly %v", rr.argvs, argv)
	}
	if want := "Title here\nBody here\n"; rr.stdin[0] != want {
		t.Errorf("stdin = %q, want %q", rr.stdin[0], want)
	}
	for _, a := range rr.argvs[0] {
		if strings.Contains(a, "Title here") || strings.Contains(a, "Body here") {
			t.Errorf("message text reached argv: %v", rr.argvs[0])
		}
	}
}

func TestPushThroughUnknownNameRunsNothingAndNamesIt(t *testing.T) {
	t.Parallel()
	rr := &recordingRunner{}
	err := pushThrough(rr.run, notifyConfig(nil), "ntfy", "t", "b")
	if err == nil || !strings.Contains(err.Error(), "ntfy") {
		t.Fatalf("pushThrough = %v, want an error naming ntfy", err)
	}
	if len(rr.argvs) != 0 {
		t.Errorf("ran %v with no command configured", rr.argvs)
	}
}

func TestPushThroughReturnsTheRunnersError(t *testing.T) {
	t.Parallel()
	want := errors.New("timed out after 10s")
	rr := &recordingRunner{err: func([]string) error { return want }}
	err := pushThrough(rr.run, notifyConfig(map[string][]string{"ntfy": {"curl"}}), "ntfy", "t", "b")
	if !errors.Is(err, want) {
		t.Errorf("pushThrough = %v, want %v", err, want)
	}
}

func TestExecuteExternalActionsExecPushesTheEscalation(t *testing.T) {
	t.Parallel()
	cfg := notifyConfig(map[string][]string{"ntfy": {"curl", "-d", "@-"}})
	rr := &recordingRunner{}
	senders := externalSenders{command: rr.run}

	statuses := executeExternalActions([]string{"bead", "exec:ntfy"}, cfg, "hq-e1", "critical", "db down", t.TempDir(), senders)

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one exec status", statuses)
	}
	got := statuses[0]
	if got.Channel != "exec" || got.Target != "ntfy" || got.Severity != "critical" || !got.RuntimeNotified || got.Error != "" {
		t.Errorf("status = %+v", got)
	}
	if len(rr.stdin) != 1 {
		t.Fatalf("ran %d commands, want 1", len(rr.stdin))
	}
	title, body, _ := strings.Cut(rr.stdin[0], "\n")
	if title != "[Gas Town CRITICAL] hq-e1: db down" {
		t.Errorf("title line = %q, want it to name the bead", title)
	}
	for _, want := range []string{"hq-e1", "CRITICAL", "gt escalate ack hq-e1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
}

// TestExecuteExternalActionsExecFailureDoesNotBlockTheRoute: a command that
// fails is counted in its own status, and the actions around it still run.
func TestExecuteExternalActionsExecFailureDoesNotBlockTheRoute(t *testing.T) {
	t.Parallel()
	cfg := notifyConfig(map[string][]string{
		"flaky": {"flaky-cmd"},
		"good":  {"good-cmd"},
	})
	rr := &recordingRunner{err: func(argv []string) error {
		if argv[0] == "flaky-cmd" {
			return errors.New("timed out after 10s")
		}
		return nil
	}}
	rec := &recordingSenders{}
	senders := rec.senders()
	senders.command = rr.run
	cfg.Contacts = config.EscalationContacts{HumanEmail: "h@example.com", SMTPHost: "smtp.invalid"}
	townRoot := t.TempDir()

	statuses := executeExternalActions(
		[]string{"exec:flaky", "email:human", "exec:good", "log"},
		cfg, "hq-e2", "high", "disk", townRoot, senders)

	if len(statuses) != 4 {
		t.Fatalf("statuses = %+v, want one per action", statuses)
	}
	if s := statuses[0]; s.Channel != "exec" || s.Target != "flaky" || s.Error != "timed out after 10s" || s.RuntimeNotified {
		t.Errorf("flaky status = %+v", s)
	}
	if s := statuses[1]; s.Channel != "email" || !s.RuntimeNotified {
		t.Errorf("email after the failure = %+v", s)
	}
	if s := statuses[2]; s.Channel != "exec" || s.Target != "good" || !s.RuntimeNotified {
		t.Errorf("good status = %+v", s)
	}
	if s := statuses[3]; s.Channel != "log" || !s.RuntimeNotified {
		t.Errorf("log status = %+v", s)
	}
	if len(rr.argvs) != 2 {
		t.Errorf("ran %v, want both commands", rr.argvs)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "logs", "escalations.log")); err != nil {
		t.Errorf("the log action after a failed exec did not run: %v", err)
	}
}

// TestExecuteExternalActionsExecUnconfiguredNameIsReported: a config built in
// code can skip load validation; delivery reports the name and runs nothing.
func TestExecuteExternalActionsExecUnconfiguredNameIsReported(t *testing.T) {
	t.Parallel()
	rr := &recordingRunner{}
	statuses := executeExternalActions([]string{"exec:missing"}, notifyConfig(nil), "hq-e3", "high", "x", t.TempDir(), externalSenders{command: rr.run})

	if len(statuses) != 1 || !strings.Contains(statuses[0].Error, "missing") {
		t.Fatalf("statuses = %+v, want an error naming missing", statuses)
	}
	if len(rr.argvs) != 0 {
		t.Errorf("ran %v", rr.argvs)
	}
}

func TestDefaultSendersCarryTheRealCommandRunner(t *testing.T) {
	t.Parallel()
	if defaultSenders().command == nil {
		t.Error("defaultSenders().command is nil: an exec: action would panic in production")
	}
}

// PushIn reads the escalation config under the town root it is handed, with no
// reliance on the working directory.
func TestPushInResolvesTheCommandFromTheGivenTown(t *testing.T) {
	t.Parallel()
	err := PushIn(t.TempDir(), "ntfy", "t", "b")
	if err == nil || !strings.Contains(err.Error(), `"ntfy"`) {
		t.Fatalf("PushIn on a town with no commands = %v, want an error naming ntfy", err)
	}
}
