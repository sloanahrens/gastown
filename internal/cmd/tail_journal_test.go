package cmd

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// tailPollCount is a viewer left open for a minute: gt tail -f polls every
// three seconds, and gt dashboard no more often than that.
const tailPollCount = 20

// stubJournalRead is a store's journal over a connection a test drives: the
// records it serves, how many reads it answered, and whether it was closed.
type stubJournalRead struct {
	records []beads.EventRecord
	err     error
	calls   int
	closed  int
}

func (j *stubJournalRead) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	j.calls++
	if j.err != nil {
		return nil, j.err
	}
	page := &beads.EventsPage{NextSince: since}
	for _, r := range j.records {
		if r.Seq <= since {
			continue
		}
		page.Records = append(page.Records, r)
		page.NextSince = r.Seq
	}
	return page, nil
}

func (j *stubJournalRead) Close() error {
	j.closed++
	return nil
}

// countingBDJournal is the bd path of a store's journal with the processes
// counted instead of started: every call is one bd the production journal would
// have run.
type countingBDJournal struct {
	spawns  *int
	records []beads.EventRecord
	calls   []string // "config" or "tail", in order
}

func (j *countingBDJournal) ConfigGet(string) (string, error) {
	*j.spawns++
	j.calls = append(j.calls, "config")
	return "true", nil
}

func (j *countingBDJournal) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	*j.spawns++
	j.calls = append(j.calls, "tail")
	page := &beads.EventsPage{NextSince: since}
	for _, r := range j.records {
		if r.Seq > since {
			page.Records = append(page.Records, r)
			page.NextSince = r.Seq
		}
	}
	return page, nil
}

// A viewer left open for a minute starts bd once per store — the events-journal
// config read a run makes once — where it used to start one per poll (gt-jw9sr).
func TestTailJournalStore_MinuteOfPollsStartsNoBDPerPoll(t *testing.T) {
	t.Parallel()
	spawns := 0
	bd := countingBDJournal{spawns: &spawns}
	direct := &stubJournalRead{records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-1", Status: "open"},
	}}
	j := newTailJournalStore(&bd, func() (tailJournalRead, error) { return direct, nil })
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events create gt-1 status=open seq=1"}) {
		t.Fatalf("first poll = %q", got)
	}
	for i := 1; i < tailPollCount; i++ {
		if got := s.Poll(); len(got) != 0 {
			t.Fatalf("poll %d reprinted: %q", i, texts(got))
		}
	}
	if spawns != 1 {
		t.Errorf("%d bd processes over %d polls (%v), want the config read alone", spawns, tailPollCount, bd.calls)
	}
	if direct.calls != tailPollCount {
		t.Errorf("the connection answered %d of %d polls", direct.calls, tailPollCount)
	}
	if direct.closed != 0 {
		t.Errorf("the connection was closed %d times while it was working", direct.closed)
	}
}

// The connection reads the journal on the store's behalf: a poll the
// connection cannot answer is answered by bd, so the stream keeps its records
// rather than reporting a failure (gt-jw9sr).
func TestTailJournalStore_FailedReadIsAnsweredByBD(t *testing.T) {
	t.Parallel()
	spawns := 0
	bd := countingBDJournal{spawns: &spawns, records: []beads.EventRecord{
		{Seq: 2, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-2"},
	}}
	failed := &stubJournalRead{err: errors.New("connection reset")}
	opens := 0
	j := newTailJournalStore(&bd, func() (tailJournalRead, error) {
		opens++
		return failed, nil
	})
	s := &eventsSource{rig: "hq", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events create gt-2 seq=2"}) {
		t.Fatalf("poll = %q", got)
	}
	if failed.closed != 1 {
		t.Errorf("the failed connection was closed %d times, want once", failed.closed)
	}
	// The next poll opens another, and bd carries the cursor on from the page
	// it served: the failure is not remembered and nothing is reread.
	bd.records = append(bd.records, beads.EventRecord{Seq: 3, TS: "2026-09-30T13:59:30Z", Op: "close", IssueID: "gt-2", Status: "closed"})
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events close gt-2 status=closed seq=3"}) {
		t.Fatalf("second poll = %q", got)
	}
	if opens != 2 {
		t.Errorf("opens = %d, want a fresh connection on the poll after the failure", opens)
	}
	if spawns != 3 { // one config read, then one journal poll per tick
		t.Errorf("bd processes = %d (%v), want 3", spawns, bd.calls)
	}
}

// A refusal that stays true until bd migrates is remembered: the polls after it
// go straight to bd rather than paying for a doomed open each time.
func TestTailJournalStore_RemembersASchemaRefusal(t *testing.T) {
	t.Parallel()
	spawns := 0
	refusals := 0
	bd := countingBDJournal{spawns: &spawns}
	j := newTailJournalStore(&bd, func() (tailJournalRead, error) {
		refusals++
		return nil, &beadsql.SchemaMismatchError{Database: "gastown", Found: 67}
	})
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	for i := 0; i < tailPollCount; i++ {
		if got := s.Poll(); len(got) != 0 {
			t.Fatalf("poll %d = %q", i, texts(got))
		}
	}
	if refusals != 1 || spawns != tailPollCount+1 {
		// One config read on the first poll, and one journal poll per tick:
		// the refusal is never paid for again.
		t.Errorf("opens = %d, bd spawns = %d; want one refusal then bd every poll", refusals, spawns)
	}
}

// A server that is merely unreachable is retried: it can come back, and the
// store's journal should stop paying for a process once it does.
func TestTailJournalStore_RetriesAConnectionThatCouldNotOpen(t *testing.T) {
	t.Parallel()
	spawns := 0
	bd := countingBDJournal{spawns: &spawns}
	direct := &stubJournalRead{}
	opens := 0
	j := newTailJournalStore(&bd, func() (tailJournalRead, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
		}
		return direct, nil
	})
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	s.Poll() // config + a failed open, answered by bd
	s.Poll() // the retry opens the connection
	if opens != 2 {
		t.Fatalf("opens = %d, want a retry after the first one failed", opens)
	}
	for i := 2; i < tailPollCount; i++ {
		s.Poll()
	}
	if direct.calls != tailPollCount-1 {
		t.Errorf("the connection answered %d polls, want %d", direct.calls, tailPollCount-1)
	}
	if spawns != 2 { // one config read, and the one poll the open refused
		t.Errorf("bd processes = %d (%v), want 2", spawns, bd.calls)
	}
}

// A cursor pruned past is a verdict, not a failure: bd is asked the same
// question and answers the same, so it is not re-read (gt-jw9sr).
func TestTailJournalStore_TruncationIsNotAFallback(t *testing.T) {
	t.Parallel()
	trunc := &beads.EventsTruncatedError{Since: 5, Floor: 40, Head: 90}
	direct := &stubJournalRead{err: trunc}
	j := newTailJournalStore(&countingBDJournal{spawns: new(int)}, func() (tailJournalRead, error) { return direct, nil })
	page, err := j.EventsTail(5, 0)
	if page != nil {
		t.Fatalf("page = %+v, want none", page)
	}
	var got *beads.EventsTruncatedError
	if !errors.As(err, &got) || *got != *trunc {
		t.Fatalf("err = %v, want %+v", err, trunc)
	}
	if direct.closed != 0 {
		t.Errorf("the connection was closed over its own verdict")
	}
}

// The connection reads the table the way bd does: the counter first, and no row
// query at all while the journal has not moved, so an idle store costs one
// indexed row per poll (gt-jw9sr).
func TestTailJournalConn_IdleJournalPaysNoRowQuery(t *testing.T) {
	t.Parallel()
	rowQueries := 0
	c := &tailJournalConn{
		head: func(context.Context) (int64, error) { return 12, nil },
		rows: func(context.Context, int64, int) ([]beads.EventJournalRow, error) {
			rowQueries++
			return nil, errors.New("the idle poll ran a row query")
		},
	}
	page, err := c.EventsTail(12, 500)
	if err != nil || page.NextSince != 12 || len(page.Records) != 0 {
		t.Fatalf("page = %+v, %v; want the cursor to stand", page, err)
	}
	if rowQueries != 0 {
		t.Errorf("row queries = %d, want none", rowQueries)
	}
}

// A page is read one row past its limit, as bd reads it, so a full page says
// more follow and the cursor stops at the last record shown.
func TestTailJournalConn_ReadsOneRowPastThePage(t *testing.T) {
	t.Parallel()
	var got int
	c := &tailJournalConn{
		head: func(context.Context) (int64, error) { return 8, nil },
		rows: func(_ context.Context, since int64, limit int) ([]beads.EventJournalRow, error) {
			got = limit
			if since != 5 {
				t.Errorf("since = %d, want the caller's cursor", since)
			}
			return []beads.EventJournalRow{
				{Seq: 6, TS: "2026-09-30 13:59:00", Op: "create", IssueID: "gt-6"},
				{Seq: 7, TS: "2026-09-30 13:59:01", Op: "create", IssueID: "gt-7"},
				{Seq: 8, TS: "2026-09-30 13:59:02", Op: "create", IssueID: "gt-8"},
			}, nil
		},
	}
	page, err := c.EventsTail(5, 2)
	if err != nil {
		t.Fatalf("EventsTail: %v", err)
	}
	if got != 3 {
		t.Errorf("fetched %d rows, want the limit and one more", got)
	}
	if len(page.Records) != 2 || page.NextSince != 7 || !page.More {
		t.Fatalf("page = %+v, want two records, cursor 7, more", page)
	}
	// The record carries bd's timestamp shape, not the store's DATETIME.
	if page.Records[0].TS != "2026-09-30T13:59:00Z" {
		t.Errorf("ts = %q, want bd's RFC3339 UTC", page.Records[0].TS)
	}
	if page.Records[1].Seq != 7 {
		t.Errorf("records = %+v", page.Records)
	}
}
