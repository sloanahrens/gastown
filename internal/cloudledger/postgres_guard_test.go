package cloudledger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// A database that never answers ends the read at the source's deadline with a
// named error, and the cloud line shows that as UNKNOWN rather than holding the
// poll that asked.
func TestCloudLedgerBoundsAReadWithADeadline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	l := &ledgertest.Ledger{Block: true}
	_, path := ledgertest.Register(t, l)
	src := cloudledger.WithReadTimeout(newPostgres(t, path), 20*time.Millisecond)

	if _, err := src.LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrReadTimeout) {
		t.Fatalf("report err = %v, want ErrReadTimeout", err)
	}
	if _, err := src.Heartbeat(ctx); !errors.Is(err, cloudledger.ErrReadTimeout) {
		t.Errorf("heartbeat err = %v, want ErrReadTimeout", err)
	}

	c := dashboard.NewCloudReaderFromSource(src).Read(ledgerNow)
	if c.State != dashboard.CloudError {
		t.Errorf("state = %q, want %q", c.State, dashboard.CloudError)
	}
	if !strings.Contains(c.Note, "postgres source") || !strings.Contains(c.Note, "timed out") {
		t.Errorf("note = %q, want the source named and the timeout", c.Note)
	}
}

// A poll that reads every app opens one pool and proves the reader role once:
// later reads reuse both.
func TestCloudLedgerPostgresReusesItsPool(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	at := ledgerNow
	l := &ledgertest.Ledger{
		Report:    emptyReport(),
		Heartbeat: &at,
		Releases:  map[string][]ledgertest.ReleaseRow{"alpha": {{At: ledgerNow, Line: []byte(`{}`)}}},
	}
	src := pgSource(t, l)

	for range 3 {
		if _, err := src.LatestPatrolReport(ctx); err != nil {
			t.Fatalf("report: %v", err)
		}
		if _, err := src.Heartbeat(ctx); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if _, err := src.Apps(ctx); err != nil {
			t.Fatalf("apps: %v", err)
		}
	}

	if n := l.Opens(); n != 1 {
		t.Errorf("opened %d connections over nine reads, want one pool", n)
	}
	probes := 0
	for _, q := range l.Statements() {
		if strings.Contains(q, "has_table_privilege") {
			probes++
		}
	}
	// One probe per ledger table, all in the first read.
	if probes != 3 {
		t.Errorf("ran %d role probes, want one per table (3)", probes)
	}
}

// A driver error that quotes the password, raw or percent-encoded, reaches the
// caller with neither form in it: the redaction covers the password on its own.
func TestCloudLedgerRedactsAnEncodedPassword(t *testing.T) {
	t.Parallel()

	const (
		password = "p@ss/w:rd"
		encoded  = "p%40ss%2Fw%3Ard"
	)
	dsn := "postgres://reader:" + encoded + "@fake/ledger-encoded"
	l := &ledgertest.Ledger{
		QueryErr: fmt.Errorf("password authentication failed for %s (%s)", encoded, password),
	}
	_, path := ledgertest.RegisterDSN(t, l, dsn)

	_, err := newPostgres(t, path).LatestPatrolReport(context.Background())
	if err == nil {
		t.Fatal("a failed query answered a report")
	}
	for _, secret := range []string{encoded, password, dsn} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("err = %q carries %q", err, secret)
		}
	}
	if !strings.Contains(err.Error(), "<dsn>") {
		t.Errorf("err = %q, want the redaction marker in place of the secret", err)
	}
}

// A keyword-form DSN keeps its password as a value, quoted with a backslash
// escape when it has a space or a quote. A driver error that quotes that
// password, in the written form or the meaning, reaches the caller with
// neither in it.
func TestCloudLedgerRedactsAKeywordPassword(t *testing.T) {
	t.Parallel()

	const (
		password = `p'ss w`
		written  = `'p\'ss w'`
	)
	dsn := `host=fake user=reader password=` + written + ` dbname=ledger-keyword`
	l := &ledgertest.Ledger{
		QueryErr: fmt.Errorf("password authentication failed for %s (%s)", password, written),
	}
	_, path := ledgertest.RegisterDSN(t, l, dsn)

	_, err := newPostgres(t, path).LatestPatrolReport(context.Background())
	if err == nil {
		t.Fatal("a failed query answered a report")
	}
	for _, secret := range []string{password, written, dsn} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("err = %q carries %q", err, secret)
		}
	}
	if !strings.Contains(err.Error(), "<dsn>") {
		t.Errorf("err = %q, want the redaction marker in place of the secret", err)
	}
}

// A binary that does not link the driver fails the read with a named error,
// before any database is opened.
func TestCloudLedgerNamesAnUnlinkedDriver(t *testing.T) {
	t.Parallel()

	l := &ledgertest.Ledger{}
	_, path := ledgertest.Register(t, l)
	src := cloudledger.WithDriver(newPostgres(t, path), "cloudledger-unlinked")

	_, err := src.LatestPatrolReport(context.Background())
	if !errors.Is(err, cloudledger.ErrNoDriver) || !strings.Contains(err.Error(), "cloudledger-unlinked") {
		t.Fatalf("err = %v, want ErrNoDriver naming the driver", err)
	}
	if n := l.Opens(); n != 0 {
		t.Errorf("opened %d connections without a driver, want none", n)
	}
}

// A pool the DSN file has moved on from is closed before the next read opens
// its replacement. A pool that will not close fails that read, named, rather
// than being left open beside the new one.
func TestCloudLedgerNamesAStalePoolThatWillNotClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	old := &ledgertest.Ledger{Report: emptyReport(), CloseErr: errors.New("close refused")}
	_, path := ledgertest.Register(t, old)
	src := newPostgres(t, path)
	if _, err := src.LatestPatrolReport(ctx); err != nil {
		t.Fatalf("first read: %v", err)
	}

	nextDSN, _ := ledgertest.Register(t, &ledgertest.Ledger{})
	if err := os.WriteFile(path, []byte(nextDSN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := src.LatestPatrolReport(ctx)
	if err == nil || !strings.Contains(err.Error(), "close refused") {
		t.Fatalf("err = %v, want the stale pool's close failure", err)
	}
	if strings.Contains(err.Error(), nextDSN) {
		t.Errorf("err = %q carries the DSN", err)
	}
}
