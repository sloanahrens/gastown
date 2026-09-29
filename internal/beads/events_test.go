package beads

import (
	"errors"
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

// Every bd process gastown starts journals its mutations, whatever the
// workspace's config.yaml says: the convoy manager reads closes from the
// journal (gt-7iwy0.2), and the rig config.yaml that would turn it on is
// git-tracked in mayor/rig.
func TestSuppressBDSideEffects_EnablesEventsJournal(t *testing.T) {
	t.Parallel()
	env := SuppressBDSideEffects([]string{"BD_EVENTS_JOURNAL=0"})
	if v, _ := lastEnvValue(env, "BD_EVENTS_JOURNAL"); v != "1" {
		t.Errorf("BD_EVENTS_JOURNAL = %q, want 1", v)
	}
}
