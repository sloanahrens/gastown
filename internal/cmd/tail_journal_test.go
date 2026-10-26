package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// tailPollCount is a viewer left open for a minute: gt tail -f polls every
// three seconds, and gt dashboard no more often than that.
const tailPollCount = 20

// journalTestClock is the store's clock, moved by hand so a test can wait out
// the retry a failure sets without sleeping.
type journalTestClock struct{ t time.Time }

func (c *journalTestClock) now() time.Time          { return c.t }
func (c *journalTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

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

// countingBDJournal is the bd path of a store's journal with its invocations
// counted: one call per bd the production client would run, so a test can say
// which path answered a poll. The minute-length count of those invocations runs
// against the production client itself, at the seam it runs bd through
// (TestTailJournalStore_MinuteOfPollsRunsNoBDPerPoll).
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

// A viewer left open for a minute makes one bd-backed read — the
// events-journal config read — where it used to make one per poll
// (gt-jw9sr). The count is of the client's reads of the seam; the bd starts
// behind them are counted in the integration tier, whose harness can start a
// bd (TestIntegrationTailJournalSpawnsNoBDPerPoll): the unit tier's refuses
// every one.
func TestTailJournalStore_MinuteOfPollsRunsNoBDPerPoll(t *testing.T) {
	t.Parallel()
	spawns := 0
	bd := &countingBDJournal{spawns: &spawns}
	direct := &stubJournalRead{records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-1", Status: "open"},
	}}
	j := newTailJournalStore(bd, func() (tailJournalRead, error) { return direct, nil })
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events create gt-1 status=open seq=1"}) {
		t.Fatalf("first poll = %q", got)
	}
	for i := 1; i < tailPollCount; i++ {
		if got := texts(s.Poll()); len(got) != 0 {
			t.Fatalf("poll %d reprinted: %q", i, got)
		}
	}
	if want := []string{"config"}; !reflect.DeepEqual(bd.calls, want) {
		t.Errorf("bd-backed reads over %d polls = %v, want the config read alone", tailPollCount, bd.calls)
	}
	if direct.calls != tailPollCount {
		t.Errorf("the connection answered %d of %d polls", direct.calls, tailPollCount)
	}
}

// The connection reads the journal on the store's behalf: a poll the
// connection cannot answer is answered by bd, so the stream keeps its records
// rather than reporting a failure (gt-jw9sr).
func TestTailJournalStore_FailedReadIsAnsweredByBD(t *testing.T) {
	t.Parallel()
	clk := &journalTestClock{t: tailNow}
	spawns := 0
	bd := countingBDJournal{spawns: &spawns, records: []beads.EventRecord{
		{Seq: 2, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-2"},
	}}
	failed := &stubJournalRead{err: errors.New("connection reset")}
	recovered := &stubJournalRead{}
	opens := 0
	j := newTailJournalStore(&bd, func() (tailJournalRead, error) {
		opens++
		if opens == 1 {
			return failed, nil
		}
		return recovered, nil
	})
	j.now = clk.now
	s := &eventsSource{rig: "hq", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events create gt-2 seq=2"}) {
		t.Fatalf("poll = %q", got)
	}
	if failed.closed != 1 {
		t.Errorf("the failed connection was closed %d times, want once", failed.closed)
	}
	bd.records = append(bd.records, beads.EventRecord{Seq: 3, TS: "2026-09-30T13:59:30Z", Op: "close", IssueID: "gt-2", Status: "closed"})
	clk.advance(time.Second)
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events close gt-2 status=closed seq=3"}) {
		t.Fatalf("second poll = %q", got)
	}
	// The failure is remembered: the polls inside the wait are answered by bd
	// and no connection is opened, so a connection that keeps failing costs a
	// bd process per poll at most — never a doomed open and read on top of one.
	for i := 0; i < 5; i++ {
		clk.advance(time.Second)
		if got := texts(s.Poll()); len(got) != 0 {
			t.Fatalf("poll %d reprinted: %q", i, got)
		}
	}
	if opens != 1 {
		t.Errorf("opens = %d, want the failure remembered for the wait", opens)
	}
	// The wait over, the store reads through a connection again.
	clk.advance(tailJournalRetryWait)
	if got := texts(s.Poll()); len(got) != 0 {
		t.Fatalf("poll after the wait = %q", got)
	}
	if opens != 2 || recovered.calls != 1 {
		t.Errorf("opens = %d, connection reads = %d; want the retry's connection to answer", opens, recovered.calls)
	}
	if spawns != 8 { // one config read, then one bd journal poll per tick to here
		t.Errorf("bd processes = %d (%v), want 8", spawns, bd.calls)
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
		t.Errorf("opens = %d, bd processes = %d; want one refusal then bd every poll", refusals, spawns)
	}
}

// A server that is merely unreachable is retried: it can come back, and the
// store's journal should stop paying for a process once it does. The retry
// waits, though: one refused dial is not a server that has come back, and a
// poll that pays for a doomed open as well as the bd process that answers it
// costs more than the bd path alone.
func TestTailJournalStore_RetriesAConnectionThatCouldNotOpen(t *testing.T) {
	t.Parallel()
	clk := &journalTestClock{t: tailNow}
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
	j.now = clk.now
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	s.Poll() // config + a failed open, answered by bd
	for i := 0; i < 5; i++ {
		clk.advance(time.Second)
		s.Poll()
	}
	if opens != 1 {
		t.Fatalf("opens = %d, want the refused dial remembered for the wait", opens)
	}
	clk.advance(tailJournalRetryWait)
	s.Poll() // the retry opens the connection
	if opens != 2 {
		t.Fatalf("opens = %d, want a retry once the wait is out", opens)
	}
	for i := 0; i < tailPollCount; i++ {
		clk.advance(time.Second)
		s.Poll()
	}
	if direct.calls != tailPollCount+1 {
		t.Errorf("the connection answered %d polls, want %d", direct.calls, tailPollCount+1)
	}
	if spawns != 7 { // one config read, and the six polls bd answered before the retry
		t.Errorf("bd processes = %d (%v), want 7", spawns, bd.calls)
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

// A viewer that stops hands its connection back rather than leaving the Dolt
// session to the process's exit (gt-jw9sr).
func TestTailJournalStore_CloseHandsTheConnectionBack(t *testing.T) {
	t.Parallel()
	direct := &stubJournalRead{}
	j := newTailJournalStore(&countingBDJournal{spawns: new(int)}, func() (tailJournalRead, error) { return direct, nil })
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	s.Poll()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if direct.closed != 1 {
		t.Errorf("the connection was closed %d times, want once", direct.closed)
	}
	// A store with nothing to hand back — bd per poll — closes without one.
	if err := (&eventsSource{rig: "gastown", journal: &countingBDJournal{spawns: new(int)}}).Close(); err != nil {
		t.Errorf("Close on the bd path: %v", err)
	}
}

// orderedJournalRead is a connection whose read fails, recording the order its
// read and its close arrived in.
type orderedJournalRead struct{ events []string }

func (j *orderedJournalRead) EventsTail(int64, int) (*beads.EventsPage, error) {
	j.events = append(j.events, "read")
	return nil, errors.New("connection reset")
}

func (j *orderedJournalRead) Close() error {
	j.events = append(j.events, "close")
	return nil
}

// A connection that failed is dropped after the read that failed on it
// returned, so a closed handle is never one a read is still using (gt-jw9sr).
func TestTailJournalStore_FailedReadDropsTheConnection(t *testing.T) {
	t.Parallel()
	direct := &orderedJournalRead{}
	j := newTailJournalStore(&countingBDJournal{spawns: new(int)}, func() (tailJournalRead, error) { return direct, nil })
	if _, err := j.EventsTail(0, 0); err != nil {
		t.Fatalf("EventsTail: %v", err)
	}
	if want := []string{"read", "close"}; !reflect.DeepEqual(direct.events, want) {
		t.Errorf("the connection saw %v, want the close after the read", direct.events)
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

// The counter and the rows are two reads, so a mutation committed between them
// puts a row above the head the counter answered. The page is judged against
// the newest row it holds, which keeps its cursor and its pruned-past verdict
// describing one journal rather than one read behind the other (gt-jw9sr).
func TestTailJournalConn_HeadIsAtLeastTheNewestRow(t *testing.T) {
	t.Parallel()
	c := &tailJournalConn{
		head: func(context.Context) (int64, error) { return 5, nil },
		rows: func(context.Context, int64, int) ([]beads.EventJournalRow, error) {
			return []beads.EventJournalRow{
				{Seq: 6, Op: "create", IssueID: "gt-6"},
				{Seq: 7, Op: "create", IssueID: "gt-7"},
			}, nil
		},
	}
	_, err := c.EventsTail(4, 0)
	var trunc *beads.EventsTruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want the pruned-past verdict the missing seq 5 is", err)
	}
	if trunc.Head != 7 {
		t.Errorf("head = %d, want the newest row the read saw, not the stale counter", trunc.Head)
	}
}

// stubJournalRowStream is the rows one read walks: the values per row, and the
// failure the read ended with.
type stubJournalRowStream struct {
	values [][]any
	err    error
	at     int
	closed int
}

func (r *stubJournalRowStream) Next() bool {
	if r.at >= len(r.values) {
		return false
	}
	r.at++
	return true
}

func (r *stubJournalRowStream) Scan(dest ...any) error {
	row := r.values[r.at-1]
	if len(dest) != len(row) {
		return fmt.Errorf("Scan %d columns into %d destinations", len(row), len(dest))
	}
	for i := range dest {
		if err := assignColumn(dest[i], row[i]); err != nil {
			return err
		}
	}
	return nil
}

// assignColumn copies one column into its destination the way a driver does for
// the journal's column types: a type the destination cannot hold is an error, as
// it is from a real Scan.
func assignColumn(dest, src any) error {
	switch d := dest.(type) {
	case *int64:
		v, ok := src.(int64)
		if !ok {
			return fmt.Errorf("cannot scan %T into int64", src)
		}
		*d = v
	case *string:
		v, ok := src.(string)
		if !ok {
			return fmt.Errorf("cannot scan %T into string", src)
		}
		*d = v
	case *sql.NullString:
		if src == nil {
			*d = sql.NullString{}
			return nil
		}
		v, ok := src.(string)
		if !ok {
			return fmt.Errorf("cannot scan %T into a nullable string", src)
		}
		*d = sql.NullString{String: v, Valid: true}
	default:
		return fmt.Errorf("unsupported destination %T", dest)
	}
	return nil
}

func (r *stubJournalRowStream) Err() error   { return r.err }
func (r *stubJournalRowStream) Close() error { r.closed++; return nil }

// fakeJournalReads is a store's connection as journalReads: the statements it
// was handed, and the rows they answer with.
type fakeJournalReads struct {
	queries []string
	streams []*stubJournalRowStream
	err     error
}

func (f *fakeJournalReads) Query(_ context.Context, q beadsql.Query) (journalRowStream, error) {
	f.queries = append(f.queries, q.String())
	if f.err != nil {
		return nil, f.err
	}
	if len(f.streams) == 0 {
		return nil, errors.New("no rows staged for " + q.String())
	}
	s := f.streams[0]
	f.streams = f.streams[1:]
	return s, nil
}

// The journal reads go through beadsql's Query path, so every statement is
// vetted as read-only before it leaves and the table is read as declared
// (gt-jw9sr).
func TestJournalHead_ReadsTheCounterThroughTheQueryPath(t *testing.T) {
	t.Parallel()
	db := &fakeJournalReads{streams: []*stubJournalRowStream{{values: [][]any{{int64(12)}}}}}
	head, err := journalHead(context.Background(), db)
	if err != nil || head != 12 {
		t.Fatalf("head = %d, %v; want 12", head, err)
	}
	if want := beadsql.JournalHead().String(); len(db.queries) != 1 || db.queries[0] != want {
		t.Errorf("queries = %v, want the declared head read %q", db.queries, want)
	}
}

// A store that has never journaled has no counter row, which is a head of zero
// rather than a failed read.
func TestJournalHead_NoCounterRowIsZero(t *testing.T) {
	t.Parallel()
	db := &fakeJournalReads{streams: []*stubJournalRowStream{{}}}
	head, err := journalHead(context.Background(), db)
	if err != nil || head != 0 {
		t.Fatalf("head = %d, %v; want a head of zero and no error", head, err)
	}
}

// A head read that fails is a failed read, however it failed.
func TestJournalHead_ReportsTheFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		db   *fakeJournalReads
	}{
		{"the statement", &fakeJournalReads{err: errors.New("dial tcp: connection refused")}},
		{"the row", &fakeJournalReads{streams: []*stubJournalRowStream{{err: errors.New("bad connection")}}}},
		{"the value", &fakeJournalReads{streams: []*stubJournalRowStream{{values: [][]any{{"not a number"}}}}}},
	} {
		if _, err := journalHead(context.Background(), tc.db); err == nil {
			t.Errorf("%s failed without an error", tc.name)
		} else if !strings.Contains(err.Error(), "events journal head") {
			t.Errorf("%s error = %v, want it to name the read", tc.name, err)
		}
	}
}

// The rows are read as the journal table holds them: the columns a viewer
// publishes, in seq order, with issue_json null on a delete.
func TestJournalRows_ScansTheJournalColumns(t *testing.T) {
	t.Parallel()
	db := &fakeJournalReads{streams: []*stubJournalRowStream{{values: [][]any{
		{int64(6), "2026-09-30 13:59:00", "create", "gt-6", "gastown/polecats/nux", `{"status":"open"}`},
		{int64(7), "2026-09-30 13:59:01", "delete", "gt-7", "gastown/polecats/nux", nil},
	}}}}
	rows, err := journalRows(context.Background(), db, 5, 3)
	if err != nil {
		t.Fatalf("journalRows: %v", err)
	}
	want := []beads.EventJournalRow{
		{Seq: 6, TS: "2026-09-30 13:59:00", Op: "create", IssueID: "gt-6", Actor: "gastown/polecats/nux", Issue: `{"status":"open"}`},
		{Seq: 7, TS: "2026-09-30 13:59:01", Op: "delete", IssueID: "gt-7", Actor: "gastown/polecats/nux"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v\nwant %+v", rows, want)
	}
	if q := db.queries[0]; !strings.Contains(q, "seq > 5") || !strings.Contains(q, "LIMIT 3") {
		t.Errorf("query = %q, want the caller's cursor and limit", q)
	}
}

// A row that will not scan is a failed read, not a row to skip: a viewer that
// dropped it would show a journal with a hole in it.
func TestJournalRows_ReportsAReadThatFailsMidPage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		db   *fakeJournalReads
	}{
		{"the statement", &fakeJournalReads{err: errors.New("dial tcp: connection refused")}},
		{"a column", &fakeJournalReads{streams: []*stubJournalRowStream{{values: [][]any{{"six", "ts", "create", "gt-6", "nux", nil}}}}}},
		{"the walk", &fakeJournalReads{streams: []*stubJournalRowStream{{err: errors.New("bad connection")}}}},
	} {
		rows, err := journalRows(context.Background(), tc.db, 5, 0)
		if err == nil {
			t.Errorf("%s failed without an error", tc.name)
			continue
		}
		if rows != nil {
			t.Errorf("%s returned %+v with the error", tc.name, rows)
		}
		if !strings.Contains(err.Error(), "events journal") {
			t.Errorf("%s error = %v, want it to name the read", tc.name, err)
		}
	}
}
