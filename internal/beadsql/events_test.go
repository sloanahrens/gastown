package beadsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// openEventsServer opens a DB over a fake server configured with cfg.
func openEventsServer(t *testing.T, cfg fakeServer) (*DB, *fakeServer) {
	t.Helper()
	cfg.level = int64(SchemaVersion)
	s := &cfg
	db, err := open(t, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, s
}

// TestCountEventsBeforeReportsTheQueryError: a failed count is returned, not
// read as zero rows older than the cutoff.
func TestCountEventsBeforeReportsTheQueryError(t *testing.T) {
	t.Parallel()
	db, _ := openEventsServer(t, fakeServer{queryErr: errors.New("events read failed")})

	_, err := db.CountEventsBefore(context.Background(), time.Unix(0, 0).UTC())
	if err == nil || !strings.Contains(err.Error(), "count events older than") || !strings.Contains(err.Error(), "events read failed") {
		t.Errorf("CountEventsBefore error = %v, want it to wrap the query failure", err)
	}
}

// TestDeleteEventsBeforeReportsTheExecError: a failed delete is returned.
func TestDeleteEventsBeforeReportsTheExecError(t *testing.T) {
	t.Parallel()
	db, _ := openEventsServer(t, fakeServer{execErr: errors.New("delete failed")})

	_, err := db.DeleteEventsBefore(context.Background(), time.Unix(0, 0).UTC(), 5)
	if err == nil || !strings.Contains(err.Error(), "delete events older than") || !strings.Contains(err.Error(), "delete failed") {
		t.Errorf("DeleteEventsBefore error = %v, want it to wrap the exec failure", err)
	}
}

// TestDeleteEventsBeforeReportsTheRowsAffectedError: a result whose count
// cannot be read is an error, not a delete reported as zero rows.
func TestDeleteEventsBeforeReportsTheRowsAffectedError(t *testing.T) {
	t.Parallel()
	db, _ := openEventsServer(t, fakeServer{rowsAffectedErr: errors.New("count unavailable")})

	_, err := db.DeleteEventsBefore(context.Background(), time.Unix(0, 0).UTC(), 5)
	if err == nil || !strings.Contains(err.Error(), "count deleted events") {
		t.Errorf("DeleteEventsBefore error = %v, want the row-count failure", err)
	}
}

// TestDeleteEventsBeforeReturnsTheRowsRemoved: a live delete reports the
// server's row count and sends the bounded statement.
func TestDeleteEventsBeforeReturnsTheRowsRemoved(t *testing.T) {
	t.Parallel()
	db, s := openEventsServer(t, fakeServer{execRows: 7})

	got, err := db.DeleteEventsBefore(context.Background(), time.Unix(0, 0).UTC(), 5)
	if err != nil {
		t.Fatalf("DeleteEventsBefore: %v", err)
	}
	if got != 7 {
		t.Errorf("DeleteEventsBefore deleted = %d, want 7", got)
	}
	statements := s.statements()
	if !strings.Contains(statements[len(statements)-1], deleteEventsBefore) {
		t.Errorf("last statement = %q, want the bounded events delete", statements[len(statements)-1])
	}
}

// TestDeleteEventsBeforeRefusesAnUnboundedDelete: a non-positive limit is
// refused before any statement reaches the server, so a caller cannot turn the
// bounded purge statement into an unbounded one.
func TestDeleteEventsBeforeRefusesAnUnboundedDelete(t *testing.T) {
	t.Parallel()
	s := &fakeServer{level: int64(SchemaVersion)}
	db, err := open(t, s)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	before := len(s.statements())
	_, err = db.DeleteEventsBefore(context.Background(), time.Unix(0, 0).UTC(), 0)
	if err == nil {
		t.Fatal("DeleteEventsBefore with limit 0 returned no error")
	}
	if !strings.Contains(err.Error(), "limit 0 is not positive") {
		t.Errorf("error %q does not say the limit is not positive", err)
	}
	if got := s.statements()[before:]; len(got) != 0 {
		t.Errorf("a refused delete reached the server: %v", got)
	}
}
