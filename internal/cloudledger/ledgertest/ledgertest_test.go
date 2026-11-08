package ledgertest

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// The fake answers each failure it is told to, and refuses what no ledger
// query should ask: a test of the cloud step that drives the fake into a state
// the fake does not name would read a fake's error as the source's.
func TestFakeLedgerNamesItsFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := (fakeDriver{}).Open("postgres://nobody@fake/none"); err == nil {
		t.Error("a DSN with no ledger opened a connection")
	}

	l := &Ledger{}
	c := &fakeConn{l: l}
	if _, err := c.Prepare("SELECT 1"); err == nil {
		t.Error("Prepare answered a statement; the reader only queries")
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO "+releaseTable, nil); err == nil {
		t.Error("ExecContext wrote to the fake ledger")
	}
	if _, err := c.QueryContext(ctx, "SELECT 1 FROM nowhere", nil); err == nil || !strings.Contains(err.Error(), "unexpected query") {
		t.Errorf("an unknown query answered: %v", err)
	}

	probe := []driver.NamedValue{{Value: cloudledger.PatrolReportTable}}
	l.ProbeErr = errors.New("probe refused")
	if _, err := c.QueryContext(ctx, "SELECT has_table_privilege(current_user, $1, 'INSERT')", probe); !errors.Is(err, l.ProbeErr) {
		t.Errorf("probe err = %v, want the probe failure", err)
	}
	l.ProbeErr = nil

	l.QueryErr = errors.New("ledger unreadable")
	for _, query := range []string{
		"SELECT schema_version, projects, findings FROM " + cloudledger.PatrolReportTable,
		"SELECT at FROM " + cloudledger.PatrolHeartbeatTable,
		"SELECT DISTINCT app FROM " + cloudledger.ReleaseTable,
		"SELECT line FROM " + cloudledger.ReleaseTable,
	} {
		if _, err := c.QueryContext(ctx, query, []driver.NamedValue{{Value: "app"}}); !errors.Is(err, l.QueryErr) {
			t.Errorf("%q err = %v, want the read failure", query, err)
		}
	}
	l.QueryErr = nil

	l.CloseErr = errors.New("close refused")
	if err := c.Close(); !errors.Is(err, l.CloseErr) {
		t.Errorf("close err = %v, want the close failure", err)
	}

	rows := &fakeRows{cols: []string{"at"}}
	if err := rows.Next(make([]driver.Value, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("Next on no rows = %v, want io.EOF", err)
	}
}
