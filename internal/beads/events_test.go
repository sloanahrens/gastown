package beads

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const tailEnvelope = `{"schema_version":1,"contract_version":1,"data":{"records":[
{"seq":6,"ts":"2026-09-29T00:00:00Z","op":"close","issue_id":"gt-a","actor":"x","issue":{"id":"gt-a","status":"closed"}},
{"seq":7,"ts":"2026-09-29T00:00:01Z","op":"delete","issue_id":"gt-b","issue":null}],"next_since":7},
"pagination":{"returned":2,"truncated":true,"limit":2,"next_cursor":"7"},"error":null}`

func TestEventsTail_ReadsMachineEnvelope(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: tailEnvelope} })
	page, err := newRecordedBeads(t.TempDir(), rec).EventsTail(5, 2)
	if err != nil {
		t.Fatalf("EventsTail: %v", err)
	}
	if len(page.Records) != 2 || page.NextSince != 7 || !page.More {
		t.Fatalf("page = %+v, want 2 records, next 7, more", page)
	}
	if r := page.Records[0]; r.Seq != 6 || r.Op != "close" || r.IssueID != "gt-a" || r.Status != "closed" || r.Actor != "x" {
		t.Errorf("record 0 = %+v", r)
	}
	if r := page.Records[1]; r.Op != "delete" || r.Status != "" {
		t.Errorf("record 1 = %+v, want a delete with no status", r)
	}
	calls := rec.calls()
	if len(calls) != 1 || strings.Join(calls[0].args, " ") != "events tail --since 5 --limit 2" {
		t.Fatalf("calls = %q, want one events tail --since 5 --limit 2", rec.argvs())
	}
	if v, _ := lastEnvValue(calls[0].env, "BD_MACHINE"); v != "1" {
		t.Errorf("BD_MACHINE = %q, want 1", v)
	}
}

func TestEventsTail_Truncated(t *testing.T) {
	t.Parallel()
	machine := `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"truncated","message":"pruned","detail":{"code":"events_journal_truncated","since":3,"floor":40,"head":90}}}`
	legacy := `{"error":"pruned","code":"events_journal_truncated","since":3,"floor":40,"head":90}`
	for name, rep := range map[string]reply{
		"machine exit 23": {stdout: machine, err: exitError{code: 23}},
		"legacy exit 1":   {stdout: legacy, err: exitError{code: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return rep })
			_, err := newRecordedBeads(t.TempDir(), rec).EventsTail(3, 0)
			var trunc *EventsTruncatedError
			if !errors.As(err, &trunc) {
				t.Fatalf("err = %v, want *EventsTruncatedError", err)
			}
			if trunc.Since != 3 || trunc.Floor != 40 || trunc.Head != 90 {
				t.Errorf("truncation = %+v", trunc)
			}
		})
	}
}

// A bd from before machine mode prints one JSON record per line.
func TestEventsTail_ReadsLegacyJSONLines(t *testing.T) {
	t.Parallel()
	out := `{"seq":1,"op":"create","issue_id":"gt-a","issue":{"id":"gt-a","status":"open"}}
{"seq":2,"op":"update","issue_id":"gt-a","issue":{"id":"gt-a","status":"closed"}}
`
	rec := newRecorder(func([]string) reply { return reply{stdout: out} })
	page, err := newRecordedBeads(t.TempDir(), rec).EventsTail(0, 2)
	if err != nil {
		t.Fatalf("EventsTail: %v", err)
	}
	if len(page.Records) != 2 || page.NextSince != 2 || !page.More || page.Records[1].Status != "closed" {
		t.Fatalf("page = %+v", page)
	}
	empty := newRecorder(func([]string) reply { return reply{stdout: ""} })
	page, err = newRecordedBeads(t.TempDir(), empty).EventsTail(9, 0)
	if err != nil || len(page.Records) != 0 || page.NextSince != 9 || page.More {
		t.Fatalf("empty legacy tail = %+v, %v; want no records, next 9", page, err)
	}
}

func TestEventsTail_RefusesUnreadableOutput(t *testing.T) {
	t.Parallel()
	for name, rep := range map[string]reply{
		"prose":          {stdout: "Warning: journal disabled\n"},
		"no cursor":      {stdout: `{"schema_version":1,"contract_version":1,"data":{"records":[]},"pagination":{"returned":0,"truncated":false},"error":null}`},
		"envelope error": {stdout: `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"store_unavailable","message":"x"}}`, err: exitError{code: 25}},
		"store failure":  {stderr: "Error: database not found: gastown", err: exitError{code: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return rep })
			if page, err := newRecordedBeads(t.TempDir(), rec).EventsTail(0, 0); err == nil {
				t.Errorf("EventsTail accepted %s as %+v", name, page)
			}
		})
	}
}

// A viewer reads the journal either way — bd per poll, or one persistent
// connection — so the two must show one stream: the same records, the same
// cursor and the same pruned-past verdict (gt-jw9sr). The rows here carry the
// DATETIME shapes CAST(ts AS CHAR) yields, and the envelope the RFC3339Nano
// one bd normalizes them to.
func TestEventsPageFromJournalRows_MatchesTheBDPath(t *testing.T) {
	t.Parallel()
	// bd cuts the page to the limit it was asked for and says more follow; the
	// connection reads one row past it, which is how the same page is arrived
	// at from the table.
	const envelope = `{"schema_version":1,"contract_version":1,"data":{"records":[
{"seq":6,"ts":"2026-09-29T00:00:00Z","op":"close","issue_id":"gt-a","actor":"x","issue":{"id":"gt-a","status":"closed"}},
{"seq":7,"ts":"2026-09-29T00:00:01.5Z","op":"update","issue_id":"gt-a","actor":"x","issue":{"id":"gt-a","status":"in_progress"}}],"next_since":7},
"pagination":{"returned":2,"truncated":true,"limit":2,"next_cursor":"7"},"error":null}`
	rows := []EventJournalRow{
		{Seq: 6, TS: "2026-09-29 00:00:00", Op: "close", IssueID: "gt-a", Actor: "x", Issue: `{"id":"gt-a","status":"closed"}`},
		{Seq: 7, TS: "2026-09-29 00:00:01.5", Op: "update", IssueID: "gt-a", Actor: "x", Issue: `{"id":"gt-a","status":"in_progress"}`},
		{Seq: 8, TS: "2026-09-29 00:00:02", Op: "delete", IssueID: "gt-b"},
	}
	rec := newRecorder(func([]string) reply { return reply{stdout: envelope} })
	fromBD, err := newRecordedBeads(t.TempDir(), rec).EventsTail(5, 2)
	if err != nil {
		t.Fatalf("bd path: %v", err)
	}
	// head 8 is the counter after the last record, which is what the row path
	// is handed instead of bd's answer.
	fromRows, err := EventsPageFromJournalRows(rows, 5, 2, 8)
	if err != nil {
		t.Fatalf("row path: %v", err)
	}
	if !reflect.DeepEqual(fromRows, fromBD) {
		t.Fatalf("row path = %+v\nbd path  = %+v", fromRows, fromBD)
	}
	if len(fromRows.Records) != 2 || fromRows.NextSince != 7 || !fromRows.More {
		t.Fatalf("page = %+v, want two records, next 7, more", fromRows)
	}
}

func TestEventsPageFromJournalRows_TruncationMatchesTheBDPath(t *testing.T) {
	t.Parallel()
	// A pruned prefix: the first row retained sits above the caller's cursor,
	// so the same Floor, Head and Since come back from either read.
	machine := `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"truncated","message":"pruned","detail":{"code":"events_journal_truncated","since":5,"floor":40,"head":90}}}`
	rec := newRecorder(func([]string) reply { return reply{stdout: machine, err: exitError{code: 23}} })
	_, err := newRecordedBeads(t.TempDir(), rec).EventsTail(5, 0)
	var fromBD *EventsTruncatedError
	if !errors.As(err, &fromBD) {
		t.Fatalf("bd path err = %v, want *EventsTruncatedError", err)
	}
	_, err = EventsPageFromJournalRows([]EventJournalRow{{Seq: 40, Op: "create", IssueID: "gt-a"}}, 5, 0, 90)
	var fromRows *EventsTruncatedError
	if !errors.As(err, &fromRows) {
		t.Fatalf("row path err = %v, want *EventsTruncatedError", err)
	}
	if *fromRows != *fromBD {
		t.Fatalf("row path truncation = %+v, bd path = %+v", fromRows, fromBD)
	}

	// Every row pruned out from under the cursor: "nothing retained" is not
	// "nothing new", and the floor one past the head is the same verdict bd's
	// counter read reaches.
	_, err = EventsPageFromJournalRows(nil, 5, 0, 9)
	if want := (EventsTruncatedError{Since: 5, Floor: 10, Head: 9}); *asTrunc(t, err) != want {
		t.Fatalf("row path = %v, want %+v", err, want)
	}
}

func TestEventsPageFromJournalRows_CaughtUpIsAnEmptyPage(t *testing.T) {
	t.Parallel()
	// The counter has not moved past the cursor: the journal has not moved, so
	// the page is empty and the cursor stands. A store that is polled for an
	// hour pays for this read and no row query at all.
	page, err := EventsPageFromJournalRows(nil, 12, 500, 12)
	if err != nil || page.NextSince != 12 || len(page.Records) != 0 || page.More {
		t.Fatalf("page = %+v, %v; want an empty page at cursor 12", page, err)
	}
}

func asTrunc(t *testing.T, err error) *EventsTruncatedError {
	t.Helper()
	var trunc *EventsTruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *EventsTruncatedError", err)
	}
	return trunc
}

// The store's config.yaml alone turns the events journal on (gt-7iwy0.7):
// gastown neither forces it nor passes an inherited override through.
func TestSuppressBDSideEffects_LeavesEventsJournalToConfig(t *testing.T) {
	t.Parallel()
	env := SuppressBDSideEffects([]string{"BD_EVENTS_JOURNAL=0"})
	if v, ok := lastEnvValue(env, "BD_EVENTS_JOURNAL"); ok {
		t.Errorf("BD_EVENTS_JOURNAL = %q, want unset", v)
	}
}
