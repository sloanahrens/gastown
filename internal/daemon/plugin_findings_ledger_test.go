package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
	"github.com/steveyegge/gastown/internal/plugin"
)

// townWithCloud returns a town root whose settings carry block as the cloud
// block, the way the operator sets it.
func townWithCloud(t *testing.T, block map[string]string) string {
	t.Helper()
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cloud, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"cloud":` + string(cloud) + `}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// cloudCheckAlertFor runs the cloud-check alert over the town's settings, with
// no directory injected, so the source is the one the settings select.
func cloudCheckAlertFor(t *testing.T, townRoot string) (key, summary string) {
	t.Helper()
	d := cloudCheckDaemon(t, "", nil, newFixedClock(), nil)
	d.cloudReportsDir = nil
	d.config.TownRoot = townRoot
	return d.cloudCheckAlert(&plugin.Plugin{Name: cloudCheckPlugin})
}

// cloudLedgerReportRow is the patrol's report row for findings: the same
// findings the file fixture writes, as the ledger's JSON columns hold them.
func cloudLedgerReportRow(t *testing.T, findings ...cloudFinding) *ledgertest.ReportRow {
	t.Helper()
	projects, err := json.Marshal([]string{"gt-platform-m4xq"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return &ledgertest.ReportRow{SchemaVersion: 1, Projects: projects, Findings: raw}
}

// The same urgent findings, read through the file source and through the
// Postgres source the town's settings select, reach the cloud-check escalation
// under the same key with the same reason.
func TestCloudLedgerDaemonSourcesAreEquivalent(t *testing.T) {
	t.Parallel()

	findings := []cloudFinding{
		cloudFindingFixture("f1", "orphan_resource"),
		cloudFindingFixture("f2", "state_missing"),
	}
	reportDir := cloudReportsFixture(t, findings...)
	fileKey, fileSummary := cloudCheckAlertFor(t, townWithCloud(t, map[string]string{"source": reportDir}))

	l := &ledgertest.Ledger{Report: cloudLedgerReportRow(t, findings...)}
	_, dsnPath := ledgertest.Register(t, l)
	pgKey, pgSummary := cloudCheckAlertFor(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}))

	if fileKey == "" || strings.Contains(fileKey, cloudUnreadableKey) {
		t.Fatalf("the file source did not read the findings: key %q", fileKey)
	}
	if l.Opens() == 0 {
		t.Fatal("the Postgres selection opened no database: it did not read the ledger")
	}
	if pgKey != fileKey || pgSummary != fileSummary {
		t.Errorf("postgres key %q summary %q, want the file source's %q and %q", pgKey, pgSummary, fileKey, fileSummary)
	}
}

// A Postgres source that cannot be read fails closed: the escalation is keyed as
// an unreadable report, with a reason that names the failure and never the DSN.
func TestCloudLedgerDaemonFailsClosedOverPostgres(t *testing.T) {
	t.Parallel()

	want := pluginFailureAlertKey(cloudCheckPlugin) + ":" + cloudUnreadableKey

	t.Run("an unreachable database", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{QueryErr: errors.New("dialing the ledger: connection refused")}
		dsn, dsnPath := ledgertest.Register(t, l)
		key, summary := cloudCheckAlertFor(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}))
		assertUnreadableAlert(t, key, summary, want)
		if !strings.Contains(summary, "connection refused") {
			t.Errorf("reason = %q, want the database failure named", summary)
		}
		if strings.Contains(summary, dsn) {
			t.Errorf("reason carries the DSN: %q", summary)
		}
	})

	t.Run("a missing DSN file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "gone.dsn")
		key, summary := cloudCheckAlertFor(t, townWithCloud(t, map[string]string{"ledger_dsn_file": path}))
		assertUnreadableAlert(t, key, summary, want)
		if !strings.Contains(summary, path) {
			t.Errorf("reason = %q, want the DSN file named", summary)
		}
	})
}

// assertUnreadableAlert checks an alert that failed closed: keyed as an
// unreadable report and carrying a reason, never a finding set.
func assertUnreadableAlert(t *testing.T, key, summary, want string) {
	t.Helper()
	if key != want {
		t.Errorf("key %q, want %q", key, want)
	}
	if strings.TrimSpace(summary) == "" {
		t.Error("the unreadable escalation has no reason")
	}
}
