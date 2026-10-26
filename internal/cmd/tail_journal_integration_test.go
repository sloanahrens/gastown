//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeBDScript answers the calls a viewer's journal makes and logs every start
// to $BD_LOG, so this test counts processes rather than stub calls. `events
// tail` answers the record the store's own read serves: a viewer that went back
// to a process per poll would print the same stream, and the count is what says
// so.
const fakeBDScript = `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
case " $* " in
  *" events tail "*) printf '{"seq":1,"ts":"2026-09-30T13:59:00Z","op":"create","issue_id":"gt-1","issue":{"status":"open"}}\n' ;;
  *" config get "*) printf '{"key":"events-journal","location":"config.yaml","value":"true"}\n' ;;
  *"version"*) printf 'bd version 999.0.0\n' ;;
esac
exit 0
`

// A viewer left open for a minute starts a handful of bd processes — the
// events-journal config read its client makes once — where it used to start one
// per poll (gt-jw9sr). The count is of real starts, which the unit tier cannot
// make: its harness refuses every bd start, so the client's reads are counted
// there instead (TestTailJournalStore_MinuteOfPollsRunsNoBDPerPoll).
func TestIntegrationTailJournalSpawnsNoBDPerPoll(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBDScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "bd.log")
	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	beadsDir := t.TempDir()
	journal := newTailJournalStore(beads.NewWithBeadsDir(t.TempDir(), beadsDir), func() (tailJournalRead, error) {
		return &stubJournalRead{records: []beads.EventRecord{
			{Seq: 1, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-1", Status: "open"},
		}}, nil
	})
	defer func() { _ = journal.Close() }()
	s := &eventsSource{rig: "gastown", journal: journal, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events create gt-1 status=open seq=1"}) {
		t.Fatalf("first poll = %q", got)
	}
	for i := 1; i < tailPollCount; i++ {
		if got := texts(s.Poll()); len(got) != 0 {
			t.Errorf("poll %d reprinted: %q", i, got)
		}
	}
	out, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("no bd ran at all, so this test counts nothing: %v", err)
	}
	starts := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, argv := range starts {
		if strings.Contains(argv, "events tail") {
			t.Errorf("a poll of the minute started a bd journal read: %q", argv)
		}
	}
	if len(starts) > 3 {
		t.Errorf("%d bd processes over %d polls (%q), want the config read alone", len(starts), tailPollCount, starts)
	}
}
