package convoy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// opLog is the order a notice test's bd and gt calls happened in: "close",
// "update", "export:<args>" and "mail".
type opLog struct {
	ops []string
}

// bd is a bdScript that logs close, update and export, answers show with
// showJSON, and fails export with exportErr(n) for the n-th export (from 1).
func (l *opLog) bd(showJSON string, exportErr func(n int) bool) *bdScript {
	exports := 0
	return &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		pos := positional(c.Args)
		switch pos[0] {
		case "version":
			return "", "", 0
		case "close", "update":
			l.ops = append(l.ops, pos[0])
			return "", "", 0
		case "export":
			exports++
			l.ops = append(l.ops, "export:"+strings.Join(c.Args, " "))
			if exportErr != nil && exportErr(exports) {
				return "", "export failed", 1
			}
			return "", "", 0
		case "show":
			return showJSON, "", 0
		case "sql":
			return "[]", "", 0
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}
}

// gt is a gtRunner that logs each mail send as "mail".
func (l *opLog) gt() gtRunner {
	return func(_ string, _ []string, args ...string) error {
		if len(args) > 1 && args[0] == "mail" && args[1] == "send" {
			l.ops = append(l.ops, "mail")
		}
		return nil
	}
}

func (l *opLog) String() string { return strings.Join(l.ops, "\n") }

func TestNotifyConvoyCompletion_StampsAndSkipsDuplicate(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")

	// show reports the completion stamp once bd update has written it.
	stamped := false
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		switch positional(c.Args)[0] {
		case "show":
			if stamped {
				return `[{"id":"hq-cv-dup","description":"Owner: gastown/crew/alice\nnudge_watchers: gastown/crew/bob\ncompletion_notified_at: 2026-05-25T02:30:00Z","created_at":"2026-05-25T02:00:00Z"}]`, "", 0
			}
			return `[{"id":"hq-cv-dup","description":"Owner: gastown/crew/alice\nnudge_watchers: gastown/crew/bob","created_at":"2026-05-25T02:00:00Z"}]`, "", 0
		case "update":
			stamped = true
		case "sql":
			return "[]", "", 0
		}
		return "", "", 0
	}}
	gt := &gtScript{}
	settings := config.NewTownSettings()
	settings.Convoy = &config.ConvoyConfig{NotifyOnComplete: true}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("save town settings: %v", err)
	}

	town := testTown(townRoot, bd, gt)
	town.Env = []string{"GT_ROLE=overseer"}
	town.NotifyCompletion("hq-cv-dup", "Duplicate Guard")
	town.NotifyCompletion("hq-cv-dup", "Duplicate Guard")

	var mails, nudges []gtCall
	for _, c := range gt.recorded() {
		switch c.Args[0] {
		case "mail":
			mails = append(mails, c)
		case "nudge":
			nudges = append(nudges, c)
		}
	}
	if len(mails) != 2 {
		t.Fatalf("mail sends = %d, want 2: %+v", len(mails), mails)
	}
	for _, m := range mails {
		line := strings.Join(m.Args, " ")
		if !strings.Contains(line, "--from convoy/hq-cv-dup") || !strings.Contains(line, "--no-notify") {
			t.Errorf("mail send %q lacks the convoy sender or --no-notify", line)
		}
	}
	if len(nudges) != 2 {
		t.Fatalf("nudges = %d, want 2: %+v", len(nudges), nudges)
	}
	for _, n := range nudges {
		if got := envValue(n.Env, "GT_ROLE"); got != "convoy/hq-cv-dup" {
			t.Errorf("nudge GT_ROLE = %q, want convoy/hq-cv-dup", got)
		}
	}
	if !stamped {
		t.Fatal("completion notification state was not recorded")
	}
	exports := bd.ran("export")
	if len(exports) != 1 {
		t.Fatalf("bd export calls = %d, want 1: %q", len(exports), bd.argvs())
	}
	if !strings.Contains(strings.Join(exports[0].Args, " "), filepath.Join(townRoot, ".beads", "issues.jsonl")) {
		t.Fatalf("bd export did not target town issues.jsonl: %q", exports[0].Args)
	}
}

func TestCloseConvoyIfComplete_ExportsJSONLBeforeNotification(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")
	var log opLog
	town := testTown(townRoot, log.bd(`[{"id":"hq-cv-done","description":"Owner: mayor/","created_at":"2026-05-25T02:00:00Z"}]`, nil), nil)
	town.gtRun = log.gt()

	closed, err := town.closeIfComplete("hq-cv-done", "Done Convoy", []TrackedIssue{
		{ID: "gt-done", Status: "closed"},
	}, false)
	if err != nil {
		t.Fatalf("closeConvoyIfComplete returned error: %v", err)
	}
	if !closed {
		t.Fatal("closeConvoyIfComplete returned closed=false, want true")
	}

	export := "export:export -o " + filepath.Join(townRoot, ".beads", "issues.jsonl")
	want := strings.Join([]string{"close", export, "mail", "update", export}, "\n")
	if got := log.String(); got != want {
		t.Fatalf("operation order mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNotifyConvoyCompletion_ExportFailureDoesNotPreventMail(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")
	var log opLog
	failAll := func(int) bool { return true }
	town := testTown(townRoot, log.bd(`[{"id":"hq-cv-export-fail","description":"Owner: mayor/","created_at":"2026-05-25T02:00:00Z"}]`, failAll), nil)
	town.gtRun = log.gt()

	town.NotifyCompletion("hq-cv-export-fail", "Export Failure")

	export := "export:export -o " + filepath.Join(townRoot, ".beads", "issues.jsonl")
	want := strings.Join([]string{"mail", "update", export}, "\n")
	if got := log.String(); got != want {
		t.Fatalf("operation order mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCloseConvoyIfComplete_CloseExportFailureRequiresDurableRetryBeforeNotification(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")
	var log opLog
	failFirst := func(n int) bool { return n == 1 }
	town := testTown(townRoot, log.bd(`[{"id":"hq-cv-close-export-fail","description":"Owner: mayor/","created_at":"2026-05-25T02:00:00Z"}]`, failFirst), nil)
	town.gtRun = log.gt()

	closed, err := town.closeIfComplete("hq-cv-close-export-fail", "Close Export Failure", []TrackedIssue{
		{ID: "gt-done", Status: "closed"},
	}, false)
	if err == nil {
		t.Fatal("closeConvoyIfComplete returned nil error after close JSONL export failure")
	}
	if closed {
		t.Fatal("closeConvoyIfComplete returned closed=true after close JSONL export failure")
	}
	if err := town.PersistAndNotify("hq-cv-close-export-fail", "Close Export Failure"); err != nil {
		t.Fatalf("persistAndNotifyConvoyCompletion returned error: %v", err)
	}

	export := "export:export -o " + filepath.Join(townRoot, ".beads", "issues.jsonl")
	want := strings.Join([]string{"close", export, export, "mail", "update", export}, "\n")
	if got := log.String(); got != want {
		t.Fatalf("operation order mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSendCloseNotification_MailUsesConvoyFromAndNoNotify(t *testing.T) {
	t.Parallel()
	gt := &gtScript{}

	testTown(t.TempDir(), nil, gt).NotifyClosed("gastown/crew/alice", "hq-cv-close", "Close Guard", "done")

	calls := gt.recorded()
	if len(calls) != 1 {
		t.Fatalf("gt calls = %+v, want one mail send", calls)
	}
	line := strings.Join(calls[0].Args, " ")
	if !strings.HasPrefix(line, "mail send ") {
		t.Fatalf("gt call %q is not a mail send", line)
	}
	if !strings.Contains(line, "--from convoy/hq-cv-close") {
		t.Fatalf("mail send missing convoy sender: %s", line)
	}
	if !strings.Contains(line, "--no-notify") {
		t.Fatalf("mail send missing --no-notify: %s", line)
	}
}
