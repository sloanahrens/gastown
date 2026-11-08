package cloudledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
)

// newPostgres returns a Postgres source over the DSN file, closed when the test
// ends so its pool does not outlive it.
func newPostgres(t *testing.T, path string) *cloudledger.PostgresSource {
	t.Helper()
	src := cloudledger.NewPostgresSource(path)
	t.Cleanup(func() {
		if err := src.Close(); err != nil {
			t.Errorf("closing the source: %v", err)
		}
	})
	return src
}

// pgSource registers l under a fresh DSN and returns a source over it.
func pgSource(t *testing.T, l *ledgertest.Ledger) *cloudledger.PostgresSource {
	t.Helper()
	_, path := ledgertest.Register(t, l)
	return newPostgres(t, path)
}

func emptyReport() *ledgertest.ReportRow {
	return &ledgertest.ReportRow{SchemaVersion: 1, Projects: []byte(`[]`), Findings: []byte(`[]`)}
}

// Each way the ledger tables answer badly is a named state, not a zero value
// read as an empty report.
func TestCloudLedgerPostgresNamesItsStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("no report row", func(t *testing.T) {
		t.Parallel()
		if _, err := pgSource(t, &ledgertest.Ledger{}).LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrNoReport) {
			t.Fatalf("err = %v, want ErrNoReport", err)
		}
	})

	t.Run("a report whose JSON is not there", func(t *testing.T) {
		t.Parallel()
		for name, report := range map[string]*ledgertest.ReportRow{
			"projects": {SchemaVersion: 1, Projects: []byte("{"), Findings: []byte(`[]`)},
			"findings": {SchemaVersion: 1, Projects: []byte(`[]`), Findings: []byte("{")},
		} {
			if _, err := pgSource(t, &ledgertest.Ledger{Report: report}).LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrInvalidReport) {
				t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
			}
		}
	})

	t.Run("a row whose columns do not scan", func(t *testing.T) {
		t.Parallel()
		_, err := pgSource(t, &ledgertest.Ledger{Report: emptyReport(), ReportScanErr: true}).LatestPatrolReport(ctx)
		if err == nil || errors.Is(err, cloudledger.ErrNoReport) {
			t.Errorf("err = %v, want the scan error", err)
		}
	})

	t.Run("no heartbeat", func(t *testing.T) {
		t.Parallel()
		if _, err := pgSource(t, &ledgertest.Ledger{}).Heartbeat(ctx); !errors.Is(err, cloudledger.ErrNoHeartbeat) {
			t.Fatalf("err = %v, want ErrNoHeartbeat", err)
		}
	})

	t.Run("a heartbeat query that fails", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{}
		l.QueryErr = errors.New("database is down")
		if _, err := pgSource(t, l).Heartbeat(ctx); err == nil {
			t.Fatal("a failed heartbeat query answered a time")
		}
	})

	t.Run("releases that cannot be read", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{}
		l.QueryErr = errors.New("database is down")
		if _, err := pgSource(t, l).ReleasesSince(ctx, "app", ledgerNow); err == nil {
			t.Error("a failed release query answered a list")
		}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); err == nil {
			t.Error("a failed release query answered a release")
		}
		if _, err := pgSource(t, l).Apps(ctx); err == nil {
			t.Error("a failed app query answered a list")
		}
	})

	t.Run("a release line that is not JSON", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{Releases: map[string][]ledgertest.ReleaseRow{"app": {{At: ledgerNow, Line: []byte("{not json")}}}}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrBadRelease) {
			t.Errorf("LastRelease = %v, want ErrBadRelease", err)
		}
		if _, err := pgSource(t, l).ReleasesSince(ctx, "app", ledgerNow.Add(-time.Hour)); !errors.Is(err, cloudledger.ErrBadRelease) {
			t.Errorf("ReleasesSince = %v, want ErrBadRelease", err)
		}
	})

	t.Run("a release row whose column does not scan", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{
			Releases:       map[string][]ledgertest.ReleaseRow{"app": {{At: ledgerNow, Line: []byte(`{}`)}}},
			ReleaseScanErr: true,
		}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); err == nil {
			t.Error("an unscannable release row answered a release")
		}
		if _, err := pgSource(t, l).ReleasesSince(ctx, "app", ledgerNow.Add(-time.Hour)); err == nil {
			t.Error("an unscannable release row answered a list")
		}
	})

	t.Run("an app with no release rows", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{Releases: map[string][]ledgertest.ReleaseRow{}}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrNoRelease) {
			t.Fatalf("err = %v, want ErrNoRelease", err)
		}
	})

	t.Run("an app row whose column does not scan", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{
			Releases:    map[string][]ledgertest.ReleaseRow{"app": {{Line: []byte(`{}`)}}},
			AppsScanErr: true,
		}
		if _, err := pgSource(t, l).Apps(ctx); err == nil {
			t.Error("an unscannable app row listed a name")
		}
	})

	t.Run("a role probe that cannot be read", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{ProbeErr: errors.New("permission denied")}
		_, err := pgSource(t, l).LatestPatrolReport(ctx)
		if err == nil || !strings.Contains(err.Error(), "checking the reader role") {
			t.Fatalf("err = %v, want the probe failure", err)
		}
	})

	t.Run("no DSN file setting", func(t *testing.T) {
		t.Parallel()
		if _, err := cloudledger.NewPostgresSource("").LatestPatrolReport(ctx); err == nil {
			t.Fatal("a source with no DSN file answered a report")
		}
	})
}
