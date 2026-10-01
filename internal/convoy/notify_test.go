package convoy

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
)

// opLog is the order a notice test's store and gt calls happened in:
// "close", "update", "export:<path>" and "mail".
type opLog struct {
	ops []string
}

// opStore is a fake town database that logs its close, update and export
// calls to log, and fails the n-th export (from 1) when exportErr(n).
type opStore struct {
	*beadsfake.Fake
	log       *opLog
	exportErr func(n int) bool
	exports   int
}

func (s *opStore) CloseWithReason(reason string, ids ...string) error {
	s.log.ops = append(s.log.ops, "close")
	return s.Fake.CloseWithReason(reason, ids...)
}

func (s *opStore) Update(id string, opts beads.UpdateOptions) error {
	s.log.ops = append(s.log.ops, "update")
	return s.Fake.Update(id, opts)
}

func (s *opStore) Export(path string) error {
	s.exports++
	s.log.ops = append(s.log.ops, "export:"+path)
	if s.exportErr != nil && s.exportErr(s.exports) {
		return errors.New("export failed")
	}
	return s.Fake.Export(path)
}

// store is a town database holding convoy, logging to l.
func (l *opLog) store(convoy beads.Issue, exportErr func(n int) bool) *opStore {
	db := townDB()
	db.Seed(convoy)
	return &opStore{Fake: db, log: l, exportErr: exportErr}
}

// notices is a noticeScript that logs each mail send as "mail".
func (l *opLog) notices() *noticeScript {
	return &noticeScript{onMail: func() { l.ops = append(l.ops, "mail") }}
}

func (l *opLog) String() string { return strings.Join(l.ops, "\n") }

func TestNotifyConvoyCompletion_StampsAndSkipsDuplicate(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")

	var log opLog
	db := log.store(beads.Issue{ID: "hq-cv-dup", Type: "convoy", Labels: []string{ConvoyLabel},
		Description: "Owner: gastown/crew/alice\nnudge_watchers: gastown/crew/bob", CreatedAt: "2026-05-25T02:00:00Z"}, nil)
	notices := &noticeScript{}
	settings := config.NewTownSettings()
	settings.Convoy = &config.ConvoyConfig{NotifyOnComplete: true}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("save town settings: %v", err)
	}

	town := testTown(townRoot, db, notices)
	town.Env = []string{"GT_ROLE=overseer"}
	town.NotifyCompletion("hq-cv-dup", "Duplicate Guard")
	town.NotifyCompletion("hq-cv-dup", "Duplicate Guard")

	mails := notices.mailsSent()
	if len(mails) != 2 {
		t.Fatalf("mail sends = %d, want 2: %+v", len(mails), mails)
	}
	for _, m := range mails {
		if m.From != "convoy/hq-cv-dup" || !m.SuppressNotify {
			t.Errorf("mail send from %q (suppress %v) lacks the convoy sender or --no-notify",
				m.From, m.SuppressNotify)
		}
	}
	nudges := notices.nudgesSent()
	if len(nudges) != 2 {
		t.Fatalf("nudges = %d, want 2: %+v", len(nudges), nudges)
	}
	for _, n := range nudges {
		if n.Sender != "convoy/hq-cv-dup" {
			t.Errorf("nudge sender = %q, want convoy/hq-cv-dup", n.Sender)
		}
	}
	if got, _ := db.Show("hq-cv-dup"); !strings.Contains(got.Description, "completion_notified_at:") {
		t.Fatalf("completion notification state was not recorded: %q", got.Description)
	}
	if want := "update\nexport:" + filepath.Join(townRoot, ".beads", "issues.jsonl"); log.String() != want {
		t.Fatalf("store calls:\n%s\nwant one stamp and one export of the town issues.jsonl:\n%s", log.String(), want)
	}
}

func TestCloseConvoyIfComplete_ExportsJSONLBeforeNotification(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, "")
	var log opLog
	town := testTown(townRoot, log.store(beads.Issue{ID: "hq-cv-done", Type: "convoy", Description: "Owner: mayor/", CreatedAt: "2026-05-25T02:00:00Z"}, nil), log.notices())

	closed, err := town.closeIfComplete("hq-cv-done", "Done Convoy", []TrackedIssue{
		{ID: "gt-done", Status: "closed"},
	}, false)
	if err != nil {
		t.Fatalf("closeConvoyIfComplete returned error: %v", err)
	}
	if !closed {
		t.Fatal("closeConvoyIfComplete returned closed=false, want true")
	}

	export := "export:" + filepath.Join(townRoot, ".beads", "issues.jsonl")
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
	town := testTown(townRoot, log.store(beads.Issue{ID: "hq-cv-export-fail", Type: "convoy", Description: "Owner: mayor/", CreatedAt: "2026-05-25T02:00:00Z"}, failAll), log.notices())

	town.NotifyCompletion("hq-cv-export-fail", "Export Failure")

	export := "export:" + filepath.Join(townRoot, ".beads", "issues.jsonl")
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
	town := testTown(townRoot, log.store(beads.Issue{ID: "hq-cv-close-export-fail", Type: "convoy", Description: "Owner: mayor/", CreatedAt: "2026-05-25T02:00:00Z"}, failFirst), log.notices())

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

	export := "export:" + filepath.Join(townRoot, ".beads", "issues.jsonl")
	want := strings.Join([]string{"close", export, export, "mail", "update", export}, "\n")
	if got := log.String(); got != want {
		t.Fatalf("operation order mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSendCloseNotification_MailUsesConvoyFromAndNoNotify(t *testing.T) {
	t.Parallel()
	notices := &noticeScript{}

	testTown(t.TempDir(), nil, notices).NotifyClosed("gastown/crew/alice", "hq-cv-close", "Close Guard", "done")

	mails := notices.mailsSent()
	if len(mails) != 1 {
		t.Fatalf("mails = %+v, want one mail send", mails)
	}
	if mails[0].To != "gastown/crew/alice" {
		t.Errorf("mail recipient = %q, want gastown/crew/alice", mails[0].To)
	}
	if mails[0].From != "convoy/hq-cv-close" {
		t.Errorf("mail send missing convoy sender: %q", mails[0].From)
	}
	if !mails[0].SuppressNotify {
		t.Error("mail send missing --no-notify")
	}
}
