package cloudledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// pgSource registers l under a fresh DSN and returns a source over it.
func pgSource(t *testing.T, l *fakeLedger) *cloudledger.PostgresSource {
	t.Helper()
	_, path := registerDSN(t, l)
	return cloudledger.NewPostgresSource(path)
}

func emptyReport() *fakeReport {
	return &fakeReport{schema: 1, projects: []byte(`[]`), findings: []byte(`[]`)}
}

// Each way the ledger tables answer badly is a named state, not a zero value
// read as an empty report.
func TestCloudLedgerPostgresNamesItsStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("no report row", func(t *testing.T) {
		t.Parallel()
		if _, err := pgSource(t, &fakeLedger{}).LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrNoReport) {
			t.Fatalf("err = %v, want ErrNoReport", err)
		}
	})

	t.Run("a report whose JSON is not there", func(t *testing.T) {
		t.Parallel()
		for name, report := range map[string]*fakeReport{
			"projects": {schema: 1, projects: []byte("{"), findings: []byte(`[]`)},
			"findings": {schema: 1, projects: []byte(`[]`), findings: []byte("{")},
		} {
			if _, err := pgSource(t, &fakeLedger{report: report}).LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrInvalidReport) {
				t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
			}
		}
	})

	t.Run("a row whose columns do not scan", func(t *testing.T) {
		t.Parallel()
		_, err := pgSource(t, &fakeLedger{report: emptyReport(), reportScanErr: true}).LatestPatrolReport(ctx)
		if err == nil || errors.Is(err, cloudledger.ErrNoReport) {
			t.Errorf("err = %v, want the scan error", err)
		}
	})

	t.Run("no heartbeat", func(t *testing.T) {
		t.Parallel()
		if _, err := pgSource(t, &fakeLedger{}).Heartbeat(ctx); !errors.Is(err, cloudledger.ErrNoHeartbeat) {
			t.Fatalf("err = %v, want ErrNoHeartbeat", err)
		}
	})

	t.Run("a heartbeat query that fails", func(t *testing.T) {
		t.Parallel()
		l := &fakeLedger{}
		l.queryErr = errors.New("database is down")
		if _, err := pgSource(t, l).Heartbeat(ctx); err == nil {
			t.Fatal("a failed heartbeat query answered a time")
		}
	})

	t.Run("releases that cannot be read", func(t *testing.T) {
		t.Parallel()
		l := &fakeLedger{}
		l.queryErr = errors.New("database is down")
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
		l := &fakeLedger{releases: map[string][]fakeRelease{"app": {{at: ledgerNow, line: []byte("{not json")}}}}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrBadRelease) {
			t.Errorf("LastRelease = %v, want ErrBadRelease", err)
		}
		if _, err := pgSource(t, l).ReleasesSince(ctx, "app", ledgerNow.Add(-time.Hour)); !errors.Is(err, cloudledger.ErrBadRelease) {
			t.Errorf("ReleasesSince = %v, want ErrBadRelease", err)
		}
	})

	t.Run("a release row whose column does not scan", func(t *testing.T) {
		t.Parallel()
		l := &fakeLedger{
			releases:       map[string][]fakeRelease{"app": {{at: ledgerNow, line: []byte(`{}`)}}},
			releaseScanErr: true,
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
		l := &fakeLedger{releases: map[string][]fakeRelease{}}
		if _, err := pgSource(t, l).LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrNoRelease) {
			t.Fatalf("err = %v, want ErrNoRelease", err)
		}
	})

	t.Run("an app row whose column does not scan", func(t *testing.T) {
		t.Parallel()
		l := &fakeLedger{
			releases:    map[string][]fakeRelease{"app": {{line: []byte(`{}`)}}},
			appsScanErr: true,
		}
		if _, err := pgSource(t, l).Apps(ctx); err == nil {
			t.Error("an unscannable app row listed a name")
		}
	})

	t.Run("a role probe that cannot be read", func(t *testing.T) {
		t.Parallel()
		l := &fakeLedger{probeErr: errors.New("permission denied")}
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
