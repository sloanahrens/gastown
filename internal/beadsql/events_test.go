package beadsql

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
