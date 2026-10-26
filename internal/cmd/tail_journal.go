package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The budgets one journal poll runs under: how long a connection may take to
// open, and how long a read may take. A viewer polls every three seconds, so a
// read that outlives its poll is a read that would pile up behind the next one.
const (
	tailJournalOpenTimeout = 5 * time.Second
	tailJournalReadTimeout = 10 * time.Second
	// tailJournalRetryWait is how long a store whose connection failed waits
	// before opening another one (gt-jw9sr). A viewer polls every three
	// seconds, so a store that reopened on every poll would pay for a doomed
	// open — and, for a failed read, a doomed read — on top of the bd process
	// that answers the poll anyway: more than the bd path alone ever cost. One
	// attempt per wait is what keeps a broken connection at the bd path's cost.
	tailJournalRetryWait = 30 * time.Second
	// tailJournalConnLifetime is how long a store's connection is kept before
	// the pool retires it. A viewer polls for as long as a human leaves it open,
	// so a session started against a Dolt that has since restarted would
	// otherwise be reused until a read happened to fail.
	tailJournalConnLifetime = 5 * time.Minute
)

// tailJournalRead is one store's journal as a single connection reads it. Its
// events are the journal alone: the store's config key is bd's to answer.
type tailJournalRead interface {
	EventsTail(since int64, limit int) (*beads.EventsPage, error)
	Close() error
}

// journalReads is the read surface of a store's Dolt connection: beadsql's
// Query path, where a statement passes ReadOnly and the schema level was
// checked when the connection opened (ADR 0001 D1). It is an interface so a
// test can drive the journal reads without a Dolt server, and it takes a
// beadsql.Query rather than a statement string so nothing from here can reach
// the server unvetted.
type journalReads interface {
	Query(ctx context.Context, q beadsql.Query) (journalRowStream, error)
}

// journalRowStream is the rows one read walks. *sql.Rows is one.
type journalRowStream interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// sqlJournalReads is a store's connection as journalReads: beadsql's DB, whose
// Query hands back *sql.Rows rather than this interface, and which the journal
// reads close when the store is done.
type sqlJournalReads struct{ db *beadsql.DB }

var (
	_ journalReads = sqlJournalReads{}
	// A store's connection is closed through the same value it is read
	// through, so an adapter that stopped forwarding Close would leave the
	// session open rather than fail to compile.
	_ interface{ Close() error } = sqlJournalReads{}
)

func (r sqlJournalReads) Query(ctx context.Context, q beadsql.Query) (journalRowStream, error) {
	return r.db.Query(ctx, q)
}

// Close closes the store's connection pool.
func (r sqlJournalReads) Close() error { return r.db.Close() }

// tailJournalConn reads one store's events journal over one persistent Dolt
// connection, with bd's own records and seq cursor (gt-jw9sr).
//
// The journal is a table bd writes and reads itself, so a viewer polling every
// three seconds does not need a bd process per poll: it is the same SELECT on a
// connection this process holds open. What `bd events tail` adds around the
// read — the projected record, the cursor, the refusal of a cursor pruned past
// — is beads.EventsPageFromJournalRows, which both paths share.
//
// The two reads are fields rather than methods on the connection so the poll
// logic can be driven without a Dolt server: newTailJournalConn binds them to
// one, and a test binds them to stubs.
type tailJournalConn struct {
	head  func(ctx context.Context) (int64, error)
	rows  func(ctx context.Context, since int64, limit int) ([]beads.EventJournalRow, error)
	close func() error
}

// newTailJournalConn binds the journal reads to db.
func newTailJournalConn(db *beadsql.DB) *tailJournalConn {
	return bindTailJournalConn(sqlJournalReads{db: db})
}

// bindTailJournalConn binds the journal reads to their connection.
func bindTailJournalConn(db journalReads) *tailJournalConn {
	return &tailJournalConn{
		head: func(ctx context.Context) (int64, error) { return journalHead(ctx, db) },
		rows: func(ctx context.Context, since int64, limit int) ([]beads.EventJournalRow, error) {
			return journalRows(ctx, db, since, limit)
		},
		close: func() error {
			if c, ok := db.(interface{ Close() error }); ok {
				return c.Close()
			}
			return nil
		},
	}
}

// EventsTail reads the journal records above since, at most limit of them
// (0 = all), as `bd events tail --since` would.
func (c *tailJournalConn) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tailJournalReadTimeout)
	defer cancel()
	head, err := c.head(ctx)
	if err != nil {
		return nil, err
	}
	if head <= since {
		// The journal has not moved, so the counter answers the poll and the
		// row query is not run at all. The projection is still the one that
		// decides it, so this is the page the rows would have produced.
		return beads.EventsPageFromJournalRows(nil, since, limit, head)
	}
	// One row past the limit, as bd reads it, so a page that comes back full
	// can say that more follow rather than being cut silently.
	fetch := limit
	if limit > 0 {
		fetch = limit + 1
	}
	rows, err := c.rows(ctx, since, fetch)
	if err != nil {
		return nil, err
	}
	// The counter and the rows are two reads, so a mutation committed between
	// them can put a row above the head this poll saw. A head is the highest
	// seq ever assigned, so the newest row is part of it: raising the stale one
	// keeps the two reads describing one journal, and the page's cursor and
	// truncated-past verdict are read off the same state (gt-jw9sr).
	if n := len(rows); n > 0 && rows[n-1].Seq > head {
		head = rows[n-1].Seq
	}
	return beads.EventsPageFromJournalRows(rows, since, limit, head)
}

// Close closes the connection.
func (c *tailJournalConn) Close() error {
	if c.close == nil {
		return nil
	}
	return c.close()
}

// journalHead reads the sequence counter: the highest seq ever assigned here.
// A store that has never journaled has no counter row, which is a head of zero
// and not a failure (bd's own read of the counter answers the same).
func journalHead(ctx context.Context, db journalReads) (int64, error) {
	rows, err := db.Query(ctx, beadsql.JournalHead())
	if err != nil {
		return 0, fmt.Errorf("reading the events journal head: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("reading the events journal head: %w", err)
		}
		return 0, nil
	}
	var head int64
	if err := rows.Scan(&head); err != nil {
		return 0, fmt.Errorf("reading the events journal head: %w", err)
	}
	return head, nil
}

// journalRows reads one page of journal rows in seq order.
func journalRows(ctx context.Context, db journalReads, since int64, limit int) ([]beads.EventJournalRow, error) {
	result, err := db.Query(ctx, beadsql.JournalTail(since, limit))
	if err != nil {
		return nil, fmt.Errorf("reading the events journal: %w", err)
	}
	defer func() { _ = result.Close() }()
	var out []beads.EventJournalRow
	for result.Next() {
		var (
			row   beads.EventJournalRow
			issue sql.NullString
		)
		if err := result.Scan(&row.Seq, &row.TS, &row.Op, &row.IssueID, &row.Actor, &issue); err != nil {
			return nil, fmt.Errorf("reading the events journal: %w", err)
		}
		row.Issue = issue.String
		out = append(out, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("reading the events journal: %w", err)
	}
	return out, nil
}

// tailJournalStore is one store's journal as a viewer reads it: the journal
// over a persistent connection when the store's Dolt server answers one, and bd
// per poll when it does not (gt-jw9sr).
//
// The bd path is not a leftover. It is what serves a store whose schema is not
// the one gastown's SQL reads were written against — an installed bd older than
// beadsql.SchemaVersion refuses the connection on purpose — and a town whose
// server answers no connection of its own. A viewer that worked before keeps
// working, at the cost it always had.
type tailJournalStore struct {
	bd   tailJournal
	open func() (tailJournalRead, error) // nil = the bd path alone
	now  func() time.Time                // the clock the retry wait is measured on

	// mu guards the connection and the state that decides whether a poll uses
	// one. It is held across a direct read as well, so a poll can never drop the
	// connection out from under another poll's read: the store answers one poll
	// at a time and a second waits, rather than racing it onto a closed handle.
	mu     sync.Mutex
	direct tailJournalRead
	// refused is a schema skew that stays true while bd is as it is, so it is
	// not retried at all. retryAt is when a failure that can pass may be tried
	// again; the polls before it go straight to bd.
	refused bool
	retryAt time.Time
}

// newTailJournalStore wraps a store's bd journal with a connection of its own.
// open is called on the first poll that finds no connection, and not again
// while one stands.
func newTailJournalStore(bd tailJournal, open func() (tailJournalRead, error)) *tailJournalStore {
	return &tailJournalStore{bd: bd, open: open, now: time.Now}
}

// ConfigGet answers from the seam, pinned to this store: bd's own resolution
// of the key — config.yaml and the store's config table, with any inherited
// BD_EVENTS_JOURNAL cleared — and a second reader of it would report a store
// that journals nothing as one that does. It is read once per run, not once
// per poll.
func (s *tailJournalStore) ConfigGet(key string) (string, error) { return s.bd.ConfigGet(key) }

// EventsTail reads the journal over the connection, and through bd for a poll
// the connection cannot answer. A failed read drops the connection and is
// remembered until the retry wait is out: the polls in between go straight to
// bd, which answered the failure, so a connection that keeps failing costs no
// more than the bd path it fell back to (gt-jw9sr). The viewer shows no gap
// while that happens — every poll is still answered.
func (s *tailJournalStore) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	s.mu.Lock()
	direct := s.connectionLocked()
	if direct == nil {
		s.mu.Unlock()
		return s.bd.EventsTail(since, limit)
	}
	page, err := direct.EventsTail(since, limit)
	if err == nil {
		s.mu.Unlock()
		return page, nil
	}
	var trunc *beads.EventsTruncatedError
	if errors.As(err, &trunc) {
		// A pruned-past cursor is the read's verdict, not its failure: bd is
		// asked the same question and answers the same, so it is returned.
		s.mu.Unlock()
		return nil, err
	}
	s.dropLocked()
	s.mu.Unlock()
	return s.bd.EventsTail(since, limit)
}

// Close drops the store's connection, if it holds one. A viewer closes its
// stores when it stops; a later poll opens another.
func (s *tailJournalStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.direct == nil {
		return nil
	}
	err := s.direct.Close()
	s.direct = nil
	return err
}

// connectionLocked is the store's persistent read, opened on first use. The
// caller holds s.mu. Nothing is opened while the wait a failure set is still
// running, and a refusal that cannot change while bd stays as it is is
// remembered for the run, so the polls after it go straight to bd rather than
// paying for a doomed open each time.
func (s *tailJournalStore) connectionLocked() tailJournalRead {
	if s.open == nil {
		return nil
	}
	if s.direct != nil || s.refused {
		return s.direct
	}
	if !s.retryAt.IsZero() && s.clock().Before(s.retryAt) {
		return nil
	}
	read, err := s.open()
	if err != nil {
		s.rememberFailureLocked(err)
		return nil
	}
	s.retryAt = time.Time{}
	s.direct = read
	return read
}

// dropLocked discards a connection that failed mid-read and starts the wait
// before another is opened. The caller holds s.mu.
func (s *tailJournalStore) dropLocked() {
	if s.direct != nil {
		_ = s.direct.Close()
	}
	s.direct = nil
	s.retryAt = s.clock().Add(tailJournalRetryWait)
}

// rememberFailureLocked records a connection this store could not use. The
// caller holds s.mu.
func (s *tailJournalStore) rememberFailureLocked(err error) {
	if tailJournalRefused(err) {
		s.refused = true
		return
	}
	s.retryAt = s.clock().Add(tailJournalRetryWait)
}

// clock is the store's clock, the process's unless a test set one.
func (s *tailJournalStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// tailJournalRefused reports whether an open failed for a reason that stays
// true until bd migrates: gastown's reads are written against
// beadsql.SchemaVersion, and the schema check refuses any other level.
func tailJournalRefused(err error) bool {
	var skew *beadsql.SchemaMismatchError
	return errors.As(err, &skew) || errors.Is(err, beadsql.ErrNotBeads)
}

// openTailJournalSQL opens one store's journal on a connection of gastown's
// own. SQL against bd's tables goes through beadsql, which is where the schema
// level and the read-only rule are checked (ADR 0001 D1).
//
// The connection is a store's only one, and is retired after a time so that a
// Dolt that restarts or moves hands the store a fresh session rather than a
// handle to a server that is gone.
//
// Every failure is the caller's to answer with bd: a server that is not
// running, a store whose metadata names no database, a bd whose schema has
// moved past these reads.
func openTailJournalSQL(townRoot, dir string) (tailJournalRead, error) {
	name := doltserver.DatabaseForBeadsDir(dir)
	if name == "" {
		return nil, fmt.Errorf("no dolt_database for %s", dir)
	}
	dsn := buildDoltDSNFromConfig(doltserver.DefaultConfig(townRoot), name, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "10s",
		WriteTimeout: "10s",
	})
	ctx, cancel := context.WithTimeout(context.Background(), tailJournalOpenTimeout)
	defer cancel()
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// One connection per store: the poll loop uses one at a time, and a town's
	// viewer holds one store per rig. The lifetime retires it now and then, so a
	// server that restarted hands the store a fresh session instead of a pooled
	// handle to the server that is gone.
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	raw.SetConnMaxLifetime(tailJournalConnLifetime)
	db, err := beadsql.New(ctx, raw, name)
	if err != nil {
		return nil, err
	}
	return newTailJournalConn(db), nil
}
