package beads

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// EventRecord is one row of bd's events journal (bd events tail): a
// committed mutation, numbered by a gapless per-replica sequence.
type EventRecord struct {
	Seq     int64
	TS      string
	Op      string // create, update, close, delete, dep_add, dep_remove, comment
	IssueID string
	Actor   string
	// Status is the issue's status after the mutation; "" when the record
	// carries no issue (a delete).
	Status string
}

// EventsPage is one read of the journal. NextSince is the --since for the
// next read: the last seq returned, or the since given when nothing was.
// More reports that a limit cut the page and records follow.
type EventsPage struct {
	Records   []EventRecord
	NextSince int64
	More      bool
}

// EventsTruncatedError is bd refusing a read whose --since lies below the
// oldest retained record: the records in between were pruned. Floor is the
// oldest seq still held, Head the highest ever assigned. Resuming from
// Floor-1 continues with a known gap.
type EventsTruncatedError struct {
	Since, Floor, Head int64
}

func (e *EventsTruncatedError) Error() string {
	return fmt.Sprintf("events journal pruned past --since %d (oldest retained %d, head %d)", e.Since, e.Floor, e.Head)
}

// eventsJournalTruncatedCode is the code bd puts on a pruned-past read.
const eventsJournalTruncatedCode = "events_journal_truncated"

// EventsTail reads journal records with seq above since, at most limit of
// them (0 = all), through one machine-mode bd events tail. A bd from before
// machine mode answers in JSON lines, which are read too. A pruned-past
// since is an *EventsTruncatedError; any other failure, and output that is
// neither form, is an error, never an empty page.
func (b *Beads) EventsTail(since int64, limit int) (*EventsPage, error) {
	args := []string{"events", "tail", "--since", strconv.FormatInt(since, 10)}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	out, err := b.runMachine(args...)
	if err != nil {
		if trunc := eventsTruncationOf(err); trunc != nil {
			return nil, trunc
		}
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		// Legacy bd prints nothing when no record follows since. A machine-
		// mode bd always writes an envelope, even for an empty page, so this
		// branch is only ever the pre-machine-mode build.
		return &EventsPage{NextSince: since}, nil
	}
	env, isEnvelope, err := decodeMachineEnvelope(out, "bd events tail")
	if err != nil {
		return nil, err
	}
	if isEnvelope {
		var data struct {
			Records   []journalRecord `json:"records"`
			NextSince *int64          `json:"next_since"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("parsing bd events tail data: %w", err)
		}
		if data.NextSince == nil {
			// Every envelope bd writes for tail carries the cursor; one
			// without it is not a page to advance on.
			return nil, fmt.Errorf("bd events tail: envelope has no next_since")
		}
		page := &EventsPage{NextSince: *data.NextSince, More: env.Pagination != nil && env.Pagination.NextCursor != ""}
		for _, r := range data.Records {
			page.Records = append(page.Records, r.record())
		}
		if page.NextSince < since {
			page.NextSince = since
		}
		return page, nil
	}
	return parseLegacyEventLines(out, since, limit)
}

// journalRecord is a journal row as bd serializes it.
type journalRecord struct {
	Seq     int64           `json:"seq"`
	TS      string          `json:"ts"`
	Op      string          `json:"op"`
	IssueID string          `json:"issue_id"`
	Actor   string          `json:"actor"`
	Issue   json.RawMessage `json:"issue"`
}

func (r journalRecord) record() EventRecord {
	rec := EventRecord{Seq: r.Seq, TS: r.TS, Op: r.Op, IssueID: r.IssueID, Actor: r.Actor}
	var issue struct {
		Status string `json:"status"`
	}
	if len(r.Issue) > 0 && json.Unmarshal(r.Issue, &issue) == nil {
		rec.Status = issue.Status
	}
	return rec
}

// EventJournalRow is one row of bd's journal table (bd_events_journal) as a
// read of the table returns it: the columns EventsTail gets from bd, which a
// persistent connection reads directly (gt-jw9sr). Issue is the issue's state
// after the mutation as bd stored it, raw and unparsed; "" when the row
// carries none (a delete).
type EventJournalRow struct {
	Seq     int64
	TS      string
	Op      string
	IssueID string
	Actor   string
	Issue   string
}

// EventsPageFromJournalRows projects a read of the journal table onto
// EventsPage: the same records, the same seq cursor and the same pruned-past
// verdict as EventsTail's bd path, so a viewer may read either and show one
// stream (gt-jw9sr). rows are the page the caller fetched — in seq order, and
// one row longer than limit when the caller asked for a limit, which is what
// lets a full page say that more follow. head is bd_events_seq's counter, the
// highest sequence ever assigned.
func EventsPageFromJournalRows(rows []EventJournalRow, since int64, limit int, head int64) (*EventsPage, error) {
	if head <= since {
		// The journal has not moved, so the cursor stands and no row is read:
		// this is what a poll of a store with no new mutations costs.
		return &EventsPage{NextSince: since}, nil
	}
	if err := journalRowsTruncation(rows, since, head); err != nil {
		return nil, err
	}
	page := &EventsPage{NextSince: since}
	for _, r := range rows {
		page.Records = append(page.Records, r.record())
	}
	if limit > 0 && len(page.Records) > limit {
		page.Records, page.More = page.Records[:limit], true
	}
	if n := len(page.Records); n > 0 {
		page.NextSince = page.Records[n-1].Seq
	}
	return page, nil
}

// journalRowsTruncation is EventsTail's pruned-past verdict reached from the
// rows instead of from bd, with the three shapes and the fields bd's own
// ComputeEventsTruncation answers with (internal/storage/issueops). Seqs are
// gapless by construction and a prune only ever deletes a prefix, so a gap is
// a prune the reader must be told about rather than a page to skip.
func journalRowsTruncation(rows []EventJournalRow, since, head int64) error {
	if len(rows) == 0 {
		// Nothing is retained, and the caller is provably behind it.
		return &EventsTruncatedError{Since: since, Floor: head + 1, Head: head}
	}
	if rows[0].Seq != since+1 {
		return &EventsTruncatedError{Since: since, Floor: rows[0].Seq, Head: head}
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Seq != rows[i-1].Seq+1 {
			return &EventsTruncatedError{Since: rows[i-1].Seq, Floor: rows[i].Seq, Head: head}
		}
	}
	return nil
}

// record projects one journal-table row onto the record both read paths
// publish, so a row bd hands back and a row a connection reads say the same
// thing.
func (r EventJournalRow) record() EventRecord {
	rec := journalRecord{Seq: r.Seq, TS: normalizeJournalTS(r.TS), Op: r.Op, IssueID: r.IssueID, Actor: r.Actor}
	if r.Issue != "" {
		rec.Issue = json.RawMessage(r.Issue)
	}
	return rec.record()
}

// normalizeJournalTS emits the RFC3339Nano UTC timestamp bd publishes whatever
// shape the driver stringified the DATETIME as, so the two read paths agree on
// the record and not merely on the instant.
func normalizeJournalTS(raw string) string {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC().Format(time.RFC3339Nano)
		}
	}
	return raw
}

// parseLegacyEventLines reads bd events tail's pre-machine-mode output: one
// JSON record per line. It has no paging signal, so a page that filled
// limit reports More.
func parseLegacyEventLines(out []byte, since int64, limit int) (*EventsPage, error) {
	page := &EventsPage{NextSince: since}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r journalRecord
		if err := json.Unmarshal(line, &r); err != nil || r.Seq == 0 {
			return nil, fmt.Errorf("bd events tail: unreadable record line %q", string(line))
		}
		page.Records = append(page.Records, r.record())
		if r.Seq > page.NextSince {
			page.NextSince = r.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("bd events tail: %w", err)
	}
	page.More = limit > 0 && len(page.Records) >= limit
	return page, nil
}

// eventsTruncationOf reads a pruned-past failure off a failed tail: the
// machine envelope's error kind "truncated" with its detail, or the legacy
// {"code":"events_journal_truncated",...} object. nil when err is not one.
func eventsTruncationOf(err error) *EventsTruncatedError {
	var ue *unavailableError
	if !errors.As(err, &ue) {
		return nil
	}
	stdout := bytes.TrimSpace(ue.stdout)
	if len(stdout) == 0 || stdout[0] != '{' {
		return nil
	}
	type window struct {
		Code  string `json:"code"`
		Since int64  `json:"since"`
		Floor int64  `json:"floor"`
		Head  int64  `json:"head"`
	}
	var env struct {
		Error *struct {
			Kind   string `json:"kind"`
			Detail window `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(stdout, &env) == nil && env.Error != nil && env.Error.Kind == "truncated" {
		d := env.Error.Detail
		return &EventsTruncatedError{Since: d.Since, Floor: d.Floor, Head: d.Head}
	}
	var legacy window
	if json.Unmarshal(stdout, &legacy) == nil && legacy.Code == eventsJournalTruncatedCode {
		return &EventsTruncatedError{Since: legacy.Since, Floor: legacy.Floor, Head: legacy.Head}
	}
	return nil
}
