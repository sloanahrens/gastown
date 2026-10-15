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
)

// tailJournalRead is one store's journal as a single connection reads it. Its
// events are the journal alone: the store's config key is bd's to answer.
type tailJournalRead interface {
	EventsTail(since int64, limit int) (*beads.EventsPage, error)
	Close() error
}

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
	return &tailJournalConn{
		head: func(ctx context.Context) (int64, error) { return journalHead(ctx, db) },
		rows: func(ctx context.Context, since int64, limit int) ([]beads.EventJournalRow, error) {
			return journalRows(ctx, db, since, limit)
		},
		close: db.Close,
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
func journalHead(ctx context.Context, db *beadsql.DB) (int64, error) {
	var head int64
	err := db.QueryRowContext(ctx, beadsql.JournalHead().String()).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the events journal head: %w", err)
	}
	return head, nil
}

// journalRows reads one page of journal rows in seq order.
func journalRows(ctx context.Context, db *beadsql.DB, since int64, limit int) ([]beads.EventJournalRow, error) {
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

	mu      sync.Mutex
	direct  tailJournalRead
	refused bool
}

// newTailJournalStore wraps a store's bd journal with a connection of its own.
// open is called on the first poll that finds no connection, and not again
// while one stands.
func newTailJournalStore(bd tailJournal, open func() (tailJournalRead, error)) *tailJournalStore {
	return &tailJournalStore{bd: bd, open: open}
}

// ConfigGet answers from bd. The events-journal key is bd's own config
// resolution — config.yaml, the store's config table, the environment — and a
// second reader of it would report a store that journals nothing as one that
// does. It is read once per run, not once per poll.
func (s *tailJournalStore) ConfigGet(key string) (string, error) { return s.bd.ConfigGet(key) }

// EventsTail reads the journal over the connection, and through bd for a poll
// the connection cannot answer. A failed read drops the connection so the next
// poll opens another; the fallback answers this one, so the viewer shows no gap
// while that happens.
func (s *tailJournalStore) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	direct := s.connection()
	if direct == nil {
		return s.bd.EventsTail(since, limit)
	}
	page, err := direct.EventsTail(since, limit)
	if err == nil {
		return page, nil
	}
	var trunc *beads.EventsTruncatedError
	if errors.As(err, &trunc) {
		// A pruned-past cursor is the read's verdict, not its failure: bd is
		// asked the same question and answers the same, so it is returned.
		return nil, err
	}
	s.drop()
	return s.bd.EventsTail(since, limit)
}

// connection is the store's persistent read, opened on first use. A refusal
// that cannot change while bd stays as it is is remembered, so the polls after
// it go straight to bd rather than paying for a doomed open each time; a server
// that is merely unreachable is retried, because it can come back.
func (s *tailJournalStore) connection() tailJournalRead {
	if s.open == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.direct != nil || s.refused {
		return s.direct
	}
	read, err := s.open()
	if err != nil {
		if tailJournalRefused(err) {
			s.refused = true
		}
		return nil
	}
	s.direct = read
	return read
}

// drop discards a connection that failed mid-read.
func (s *tailJournalStore) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.direct != nil {
		_ = s.direct.Close()
	}
	s.direct = nil
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
	db, err := beadsql.Open(ctx, dsn, name)
	if err != nil {
		return nil, err
	}
	return newTailJournalConn(db), nil
}
