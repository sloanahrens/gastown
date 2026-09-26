package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// escalationStub is a shell stub for bd that answers the handful of reads and
// writes the alert lifecycle uses, recording every mutating call so a test can
// assert what was touched. Responses for `show` and `list` come from files, so
// the fixtures stay valid JSON instead of being embedded in shell quoting.
type escalationStub struct {
	dir      string
	logPath  string
	stdinLog string
}

func newEscalationStub(t *testing.T) *escalationStub {
	t.Helper()
	s := &escalationStub{
		dir:      t.TempDir(),
		logPath:  filepath.Join(t.TempDir(), "calls.log"),
		stdinLog: filepath.Join(t.TempDir(), "stdin.log"),
	}
	showPath := filepath.Join(s.dir, "show.json")
	listPath := filepath.Join(s.dir, "list.json")
	// Defaults: nothing found. Tests overwrite these with writeJSON.
	s.writeFile(showPath, "[]")
	s.writeFile(listPath, "[]")

	script := `#!/bin/sh
case "$1" in
  --allow-stale)
    exit 1
    ;;
  show)
    cat "` + showPath + `"
    exit 0
    ;;
  list)
    cat "` + listPath + `"
    exit 0
    ;;
  update)
    printf '%s\n' "$*" >> "` + s.logPath + `"
    cat >> "` + s.stdinLog + `"
    printf '\n---\n' >> "` + s.stdinLog + `"
    exit 0
    ;;
  close)
    printf '%s\n' "$*" >> "` + s.logPath + `"
    exit 0
    ;;
esac
echo '{}'
exit 0
`
	stubPath := filepath.Join(s.dir, "bd")
	s.writeFile(stubPath, script)
	if err := os.Chmod(stubPath, 0o755); err != nil {
		t.Fatalf("chmod bd stub: %v", err)
	}
	t.Setenv("PATH", s.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetBdAllowStaleCacheForTest()
	return s
}

func (s *escalationStub) writeFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
}

// setShowJSON makes `bd show` answer with the given issues.
func (s *escalationStub) setShowJSON(t *testing.T, issues ...*Issue) {
	t.Helper()
	raw, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal show fixture: %v", err)
	}
	s.writeFile(filepath.Join(s.dir, "show.json"), string(raw))
}

// setListJSON makes `bd list` answer with the given issues.
func (s *escalationStub) setListJSON(t *testing.T, issues ...*Issue) {
	t.Helper()
	raw, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal list fixture: %v", err)
	}
	s.writeFile(filepath.Join(s.dir, "list.json"), string(raw))
}

// calls returns the recorded mutating invocations, one per line.
func (s *escalationStub) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read call log: %v", err)
	}
	return string(data)
}

// stdin returns the descriptions written to bd via stdin, one per update.
func (s *escalationStub) stdin(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.stdinLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read stdin log: %v", err)
	}
	return strings.Split(string(data), "\n---\n")
}

func escalationIssueForTest(id, fingerprint string, occurrences int) *Issue {
	fields := &EscalationFields{
		Severity:    "high",
		Reason:      "first firing",
		Source:      "main_branch_test",
		EscalatedBy: "daemon",
		EscalatedAt: "2026-09-21T00:00:00Z",
		Fingerprint: fingerprint,
		Occurrences: occurrences,
	}
	return &Issue{
		ID:          id,
		Title:       "main_branch_test: main branch test failures:",
		Status:      string(StatusOpen),
		Priority:    1,
		Type:        "task",
		Labels:      []string{"gt:escalation", fingerprint},
		Description: FormatEscalationDescription("main_branch_test: main branch test failures:", fields),
	}
}

// TestBumpEscalation_RecordsRepeatOnTheExistingBead is the gt-vwry acceptance
// criterion "firing the same alert twice yields one open bead with count 2":
// the second firing must not create anything, and must leave the recurrence
// visible on the bead that already represents the alert.
func TestBumpEscalation_RecordsRepeatOnTheExistingBead(t *testing.T) {
	stub := newEscalationStub(t)
	existing := escalationIssueForTest("hq-e1", "escalation-fp:abc123", 1)
	stub.setShowJSON(t, existing)

	b := New(t.TempDir())
	count, renotify, err := b.BumpEscalation("hq-e1", "high", "second firing", "main_branch_test", time.Hour)
	if err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if count != 2 {
		t.Errorf("occurrences = %d, want 2", count)
	}
	if !renotify {
		t.Error("a bead with no last_notified_at and an old escalated_at should renotify")
	}

	calls := stub.calls(t)
	if !strings.Contains(calls, "update hq-e1") {
		t.Errorf("expected an update of hq-e1, got calls:\n%s", calls)
	}
	if strings.Contains(calls, "create") {
		t.Errorf("BumpEscalation must not create a bead, got calls:\n%s", calls)
	}

	stdins := stub.stdin(t)
	if len(stdins) == 0 {
		t.Fatal("expected the new description to be written via stdin")
	}
	updated := ParseEscalationFields(stdins[0])
	if updated.Occurrences != 2 {
		t.Errorf("persisted occurrences = %d, want 2", updated.Occurrences)
	}
	if updated.LastSeenAt == "" {
		t.Error("persisted last_seen_at is empty; the repeat should record when it was seen")
	}
	if updated.Reason != "second firing" {
		t.Errorf("persisted reason = %q, want the latest firing's reason", updated.Reason)
	}
}

// TestBumpEscalation_SeedsOccurrencesForPreexistingBeads covers escalations
// created before occurrences were tracked: their fields carry no count, so the
// bump must count the bead's own creation as the first firing rather than
// restarting the tally at 1 and losing the recurrence.
func TestBumpEscalation_SeedsOccurrencesForPreexistingBeads(t *testing.T) {
	stub := newEscalationStub(t)
	stub.setShowJSON(t, escalationIssueForTest("hq-old", "escalation-fp:abc123", 0))

	b := New(t.TempDir())
	count, _, err := b.BumpEscalation("hq-old", "high", "second firing", "main_branch_test", time.Hour)
	if err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if count != 2 {
		t.Errorf("occurrences = %d, want 2 (creation counts as the first firing)", count)
	}
}

func TestBumpEscalation_RejectsNonEscalationBeads(t *testing.T) {
	stub := newEscalationStub(t)
	plain := escalationIssueForTest("gt-work", "", 1)
	plain.Labels = []string{"gt:task"}
	plain.Description = "ordinary work"
	stub.setShowJSON(t, plain)

	b := New(t.TempDir())
	if _, _, err := b.BumpEscalation("gt-work", "high", "reason", "source", time.Hour); err == nil {
		t.Fatal("expected BumpEscalation to refuse a bead that is not an escalation")
	}
}

// TestBumpEscalation_SuppressesWithinRenotifyWindow is the gt-9qg1 fix's core
// property: a repeat firing that arrives before the renotify window has
// elapsed since the last notification must bump the bead's occurrence count
// (so the recurrence stays visible) but must NOT ask the caller to re-send
// notifications, or a fast-firing condition would spam every channel on every
// cycle.
func TestBumpEscalation_SuppressesWithinRenotifyWindow(t *testing.T) {
	stub := newEscalationStub(t)
	existing := escalationIssueForTest("hq-e1", "escalation-fp:abc123", 1)
	fields := ParseEscalationFields(existing.Description)
	fields.LastNotifiedAt = time.Now().Format(time.RFC3339)
	existing.Description = FormatEscalationDescription(existing.Title, fields)
	stub.setShowJSON(t, existing)

	b := New(t.TempDir())
	count, renotify, err := b.BumpEscalation("hq-e1", "high", "second firing", "main_branch_test", time.Hour)
	if err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if count != 2 {
		t.Errorf("occurrences = %d, want 2", count)
	}
	if renotify {
		t.Error("a firing just after the last notification must not renotify")
	}

	stdins := stub.stdin(t)
	if len(stdins) == 0 {
		t.Fatal("expected the new description to be written via stdin")
	}
	updated := ParseEscalationFields(stdins[0])
	if updated.LastNotifiedAt != fields.LastNotifiedAt {
		t.Errorf("last_notified_at changed from %q to %q on a suppressed firing", fields.LastNotifiedAt, updated.LastNotifiedAt)
	}
}

// TestBumpEscalation_RenotifiesAfterWindowElapses is the flip side: once the
// renotify window has elapsed since the last notification, the next firing
// must ask the caller to re-send and must record the new last_notified_at, or
// a persisting condition would go silent forever after its first alert.
func TestBumpEscalation_RenotifiesAfterWindowElapses(t *testing.T) {
	stub := newEscalationStub(t)
	existing := escalationIssueForTest("hq-e1", "escalation-fp:abc123", 1)
	fields := ParseEscalationFields(existing.Description)
	fields.LastNotifiedAt = time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	existing.Description = FormatEscalationDescription(existing.Title, fields)
	stub.setShowJSON(t, existing)

	b := New(t.TempDir())
	_, renotify, err := b.BumpEscalation("hq-e1", "high", "still failing", "main_branch_test", time.Hour)
	if err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if !renotify {
		t.Error("a firing an hour after the last notification must renotify")
	}

	stdins := stub.stdin(t)
	if len(stdins) == 0 {
		t.Fatal("expected the new description to be written via stdin")
	}
	updated := ParseEscalationFields(stdins[0])
	if updated.LastNotifiedAt == fields.LastNotifiedAt || updated.LastNotifiedAt == "" {
		t.Errorf("expected last_notified_at to be refreshed, got %q (was %q)", updated.LastNotifiedAt, fields.LastNotifiedAt)
	}
}

func TestShouldRenotify(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		fields *EscalationFields
		window time.Duration
		want   bool
	}{
		{
			name:   "nil fields always renotifies",
			fields: nil,
			window: time.Hour,
			want:   true,
		},
		{
			name:   "no timestamps at all always renotifies",
			fields: &EscalationFields{},
			window: time.Hour,
			want:   true,
		},
		{
			name:   "recent last_notified_at suppresses",
			fields: &EscalationFields{LastNotifiedAt: now.Add(-time.Minute).Format(time.RFC3339)},
			window: time.Hour,
			want:   false,
		},
		{
			name:   "old last_notified_at renotifies",
			fields: &EscalationFields{LastNotifiedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
			window: time.Hour,
			want:   true,
		},
		{
			name:   "falls back to escalated_at when never notified",
			fields: &EscalationFields{EscalatedAt: now.Add(-time.Minute).Format(time.RFC3339)},
			window: time.Hour,
			want:   false,
		},
		{
			name:   "zero window always renotifies",
			fields: &EscalationFields{LastNotifiedAt: now.Format(time.RFC3339)},
			window: 0,
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldRenotify(tt.fields, tt.window); got != tt.want {
				t.Errorf("ShouldRenotify() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCloseEscalationsByFingerprints_ClosesOnlyMatchingKeys is the gt-vwry
// acceptance criterion "the next patrol after the condition clears closes it".
// A producer clearing its own key must not reach another condition's alert.
func TestCloseEscalationsByFingerprints_ClosesOnlyMatchingKeys(t *testing.T) {
	stub := newEscalationStub(t)
	mine := escalationIssueForTest("hq-mine", "escalation-fp:aaa111", 1)
	other := escalationIssueForTest("hq-other", "escalation-fp:bbb222", 1)
	stub.setListJSON(t, mine, other)
	// CloseEscalation re-reads the bead before mutating it.
	stub.setShowJSON(t, mine)

	b := New(t.TempDir())
	closed, err := b.CloseEscalationsByFingerprints([]string{"escalation-fp:aaa111"}, "daemon", "main branch tests green")
	if err != nil {
		t.Fatalf("CloseEscalationsByFingerprints: %v", err)
	}
	if len(closed) != 1 || closed[0] != "hq-mine" {
		t.Fatalf("closed = %v, want just [hq-mine]", closed)
	}

	calls := stub.calls(t)
	if !strings.Contains(calls, "close hq-mine") {
		t.Errorf("expected hq-mine to be closed, got calls:\n%s", calls)
	}
	if strings.Contains(calls, "close hq-other") {
		t.Errorf("hq-other's key was not cleared and must stay open, got calls:\n%s", calls)
	}
}

// TestCloseEscalationsByFingerprints_ClearWithNothingToClear covers the healthy
// cycle: a producer re-checks every pass, so the ordinary case is a key that
// matches nothing. That must not be an error, or a clean patrol would report
// itself as failed.
func TestCloseEscalationsByFingerprints_ClearWithNothingToClear(t *testing.T) {
	stub := newEscalationStub(t)
	stub.setListJSON(t)

	b := New(t.TempDir())
	closed, err := b.CloseEscalationsByFingerprints([]string{"escalation-fp:absent"}, "daemon", "condition cleared")
	if err != nil {
		t.Fatalf("clearing an absent key must not error: %v", err)
	}
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none", closed)
	}
	if calls := stub.calls(t); strings.Contains(calls, "close") {
		t.Errorf("nothing matched, so nothing should have been closed, got calls:\n%s", calls)
	}
}

func TestCloseEscalationsByFingerprints_NoKeysIsANoOp(t *testing.T) {
	stub := newEscalationStub(t)
	b := New(t.TempDir())
	closed, err := b.CloseEscalationsByFingerprints(nil, "daemon", "reason")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none", closed)
	}
	if calls := stub.calls(t); calls != "" {
		t.Errorf("expected no bd calls at all, got:\n%s", calls)
	}
}

// TestEscalationFieldsOccurrenceRoundTrip pins the on-bead encoding: the
// occurrence count and last-seen stamp have to survive Format -> Parse, since
// BumpEscalation reads what a previous firing wrote.
func TestEscalationFieldsOccurrenceRoundTrip(t *testing.T) {
	fields := &EscalationFields{
		Severity:    "high",
		Reason:      "spike",
		EscalatedBy: "daemon",
		EscalatedAt: "2026-09-21T00:00:00Z",
		Occurrences: 7,
		LastSeenAt:  "2026-09-21T06:00:00Z",
	}
	got := ParseEscalationFields(FormatEscalationDescription("jsonl spike", fields))
	if got.Occurrences != 7 {
		t.Errorf("Occurrences = %d, want 7", got.Occurrences)
	}
	if got.LastSeenAt != "2026-09-21T06:00:00Z" {
		t.Errorf("LastSeenAt = %q, want the round-tripped timestamp", got.LastSeenAt)
	}
}

// TestEscalationFieldsWithoutOccurrencesParsesAsZero keeps the pre-gt-vwry
// shape readable: descriptions written before occurrences existed have no such
// line, and must parse as "never bumped" rather than as a parse failure that
// would make every bump restart the tally.
func TestEscalationFieldsWithoutOccurrencesParsesAsZero(t *testing.T) {
	legacy := "jsonl spike\n\nseverity: high\nreason: spike\nsource: jsonl_git_backup\n" +
		"escalated_by: daemon\nescalated_at: 2026-09-21T00:00:00Z\nfingerprint: escalation-fp:abc123\n"
	fields := ParseEscalationFields(legacy)
	if fields.Occurrences != 0 {
		t.Errorf("Occurrences = %d, want 0 for a description with no occurrences line", fields.Occurrences)
	}
	if fields.LastSeenAt != "" {
		t.Errorf("LastSeenAt = %q, want empty", fields.LastSeenAt)
	}
}
