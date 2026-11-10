package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// cloudDSNBody is a DSN as the Secret holds it, with the newline a file ends in.
const cloudDSNBody = "postgres://reader:s3krit@db/ledger\n"

// cloudDSNKey is the attention item the empty-DSN check raises and clears.
func cloudDSNKey() string {
	return pluginFailureAlertKey(cloudCheckPlugin) + ":" + cloudDSNEmptyKey
}

// cloudDSNFile writes the ledger DSN file with body and returns its path. An
// empty body is written 0644, the mode a Secret mount gives by default.
func cloudDSNFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger-reader.dsn")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o600)
	if body == "" {
		mode = 0o644
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// cloudDSNDaemon returns a daemon whose town is townRoot, with no report
// directory injected, so the cloud step is the one the town's settings select.
// Its log is captured in the returned buffer.
func cloudDSNDaemon(t *testing.T, townRoot string, notes *notifyfake.Recorder, clk clockwork.Clock, bd workBeadReader) (*Daemon, *bytes.Buffer) {
	t.Helper()
	d := cloudCheckDaemon(t, "", notes, clk, bd)
	d.cloudReportsDir = nil
	d.config.TownRoot = townRoot
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	return d, &logs
}

// An empty ledger DSN file raises one attention item that names the file and
// says it is empty. The reason carries no DSN, because the file has none.
func TestCloudLedgerDSNEmptyRaisesOneItem(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)

	d.checkCloudDSN()

	esc := notes.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %d, want one", len(esc))
	}
	if got := esc[0].Escalation.Fingerprint; got != cloudDSNKey() {
		t.Errorf("keyed %q, want %q", got, cloudDSNKey())
	}
	reason := esc[0].Escalation.Reason
	if !strings.Contains(reason, dsnPath) || !strings.Contains(reason, "empty") {
		t.Errorf("reason = %q, want the file named and the word empty", reason)
	}
	if !d.cloudDSN.isRaised() {
		t.Error("the item is not recorded as raised, so a later file with content would not clear it")
	}
	if !strings.Contains(logs.String(), dsnPath) {
		t.Errorf("log = %q, want the refusal logged", logs.String())
	}
}

// The refusal is logged at most once an hour, however often the check runs,
// while the item is refreshed on every run.
func TestCloudLedgerDSNEmptyLogsOncePerHour(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	clk := newFixedClock()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, clk, nil)

	lines := func() int { return strings.Count(logs.String(), "\n") }

	d.checkCloudDSN()
	d.checkCloudDSN()
	if got := lines(); got != 1 {
		t.Fatalf("log lines after two runs in one hour = %d, want 1", got)
	}
	clk.Advance(cloudDSNLogInterval - time.Second)
	d.checkCloudDSN()
	if got := lines(); got != 1 {
		t.Fatalf("log lines just inside the hour = %d, want 1", got)
	}
	clk.Advance(time.Second)
	d.checkCloudDSN()
	if got := lines(); got != 2 {
		t.Fatalf("log lines after the hour = %d, want 2", got)
	}
	if got := len(notes.Escalations()); got != 4 {
		t.Errorf("escalations = %d, want every run refreshing the item", got)
	}
}

// A file that holds a DSN again clears the item this check raised, once.
func TestCloudLedgerDSNWithContentClearsTheRaisedItem(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	d.checkCloudDSN()

	// The empty file was 0644; a rewrite keeps that mode, which the check refuses.
	if err := os.WriteFile(dsnPath, []byte(cloudDSNBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dsnPath, 0o600); err != nil {
		t.Fatal(err)
	}
	d.checkCloudDSN()

	clears := notes.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %d, want one", len(clears))
	}
	if clears[0].Reason != cloudDSNClearedReason || !slices.Equal(clears[0].Fingerprints, []string{cloudDSNKey()}) {
		t.Errorf("clear = %q %v, want %q for the item", clears[0].Reason, clears[0].Fingerprints, cloudDSNClearedReason)
	}
	if d.cloudDSN.isRaised() {
		t.Error("the item is still recorded as raised after its clear")
	}
	d.checkCloudDSN()
	if got := len(notes.Clears()); got != 1 {
		t.Errorf("clears after a second good run = %d, want still one", got)
	}
}

// A check that never raised an item clears nothing, so a healthy town's runs
// send no clears to the notifier.
func TestCloudLedgerDSNWithContentRaisesAndClearsNothing(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, cloudDSNBody)
	notes := notifyfake.New()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)

	d.checkCloudDSN()

	if n := len(notes.Escalations()) + len(notes.Clears()); n != 0 {
		t.Errorf("notifier sends = %d, want none", n)
	}
	if d.cloudDSN.isRaised() || logs.Len() != 0 {
		t.Errorf("raised = %v, log = %q; want neither", d.cloudDSN.isRaised(), logs.String())
	}
}

// A clear the notifier refuses leaves the item raised, so the next run clears it.
func TestCloudLedgerDSNRefusedClearIsRetried(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	d.checkCloudDSN()

	if err := os.WriteFile(dsnPath, []byte(cloudDSNBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dsnPath, 0o600); err != nil {
		t.Fatal(err)
	}
	notes.Fail(notifyfake.KindClear, errors.New("notifier down"))
	d.checkCloudDSN()
	if !d.cloudDSN.isRaised() {
		t.Fatal("a refused clear was recorded as cleared")
	}

	notes.Fail(notifyfake.KindClear, nil)
	d.checkCloudDSN()
	if d.cloudDSN.isRaised() {
		t.Error("the retried clear did not land")
	}
}

// An injected report directory is read as files, so the DSN check does not run
// for it, even when the town's settings name an empty DSN file.
func TestCloudLedgerDSNCheckSkipsAnInjectedReportDirectory(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	d.cloudReportsDir = func() string { return cloudReportsFixture(t) }

	d.checkCloudDSN()

	if n := len(notes.Escalations()) + len(notes.Clears()); n != 0 || logs.Len() != 0 {
		t.Errorf("sends = %d, log = %q; want the check not to run", n, logs.String())
	}
}

// A file that cannot be checked is not a clear: a missing file leaves the item
// open, and the refusal is logged.
func TestCloudLedgerDSNUnknownStateLeavesTheItem(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	clk := newFixedClock()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, clk, nil)
	d.checkCloudDSN()

	if err := os.Remove(dsnPath); err != nil {
		t.Fatal(err)
	}
	clk.Advance(cloudDSNLogInterval) // the new refusal is a new line, past the hour's hold
	d.checkCloudDSN()

	if got := len(notes.Clears()); got != 0 {
		t.Errorf("clears = %d, want none: a missing file is not a clear", got)
	}
	if !d.cloudDSN.isRaised() {
		t.Error("the item was forgotten when the file went missing")
	}
	if !strings.Contains(logs.String(), "cannot be checked") {
		t.Errorf("log = %q, want the missing file named", logs.String())
	}
}

// Settings that cannot be read are not a clear either: the state is unknown.
func TestCloudLedgerDSNUnreadableSettingsLeaveTheItem(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	townRoot := townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath})
	notes := notifyfake.New()
	clk := newFixedClock()
	d, logs := cloudDSNDaemon(t, townRoot, notes, clk, nil)
	d.checkCloudDSN()

	if err := os.WriteFile(config.TownSettingsPath(townRoot), []byte(`{"cloud":`), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Advance(cloudDSNLogInterval) // the settings refusal is a new line, past the hour's hold
	d.checkCloudDSN()

	if got := len(notes.Clears()); got != 0 {
		t.Errorf("clears = %d, want none while the settings are unreadable", got)
	}
	if !d.cloudDSN.isRaised() {
		t.Error("the item was forgotten when the settings could not be read")
	}
	if !strings.Contains(logs.String(), "reading town settings") {
		t.Errorf("log = %q, want the settings read named", logs.String())
	}
}

// A town that stops reading its ledger from Postgres clears the item the empty
// file raised, under the reason that says why.
func TestCloudLedgerDSNUnusedClearsTheRaisedItem(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	d.checkCloudDSN()

	d.config.TownRoot = townWithCloud(t, map[string]string{"source": cloudReportsFixture(t)})
	d.checkCloudDSN()

	clears := notes.Clears()
	if len(clears) != 1 || clears[0].Reason != cloudDSNUnusedReason {
		t.Fatalf("clears = %+v, want one under %q", clears, cloudDSNUnusedReason)
	}
}

// An item the operator closed within the reopen window is not raised again,
// as a plugin failure's would not be.
func TestCloudLedgerDSNHonorsAnOperatorClose(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	clk := newFixedClock()
	bd := beadsfake.New(beadsfake.WithClock(clk))
	seedClosedEscalation(t, bd, cloudDSNKey())
	notes := notifyfake.New()
	d, logs := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, clk, bd)

	d.checkCloudDSN()

	if got := len(notes.Escalations()); got != 0 {
		t.Errorf("escalations = %d, want none within the reopen window", got)
	}
	if !strings.Contains(logs.String(), "closed within") {
		t.Errorf("log = %q, want the suppression named", logs.String())
	}
}

// A raise the notifier refuses is not recorded as raised, so the next run
// raises it again. The notifier's retries sleep on the daemon's clock, so the
// clock is driven until the check returns.
func TestCloudLedgerDSNRefusedRaiseIsRetried(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	notes.Fail(notifyfake.KindEscalate, errors.New("notifier down"))
	clk := newFixedClock()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, clk, nil)

	done := make(chan struct{})
	go func() {
		d.checkCloudDSN()
		close(done)
	}()
	driveClock(t, clk, time.Second, done)

	if d.cloudDSN.isRaised() {
		t.Fatal("a refused raise was recorded as raised")
	}

	notes.Fail(notifyfake.KindEscalate, nil)
	d.checkCloudDSN()
	if !d.cloudDSN.isRaised() {
		t.Error("the retried raise did not land")
	}
}

// The cloud-check run calls the check after it finishes, so a run that passes
// over an unread ledger still raises the item. Without the call the state is
// seen only on a failed run.
func TestCloudLedgerDSNCheckRunsAfterACloudCheck(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	d.execCmd = scriptBash(cliReply{}).run // the run passes
	p := scriptPlugin(t, cloudCheckPlugin, "exit 0\n")

	d.startScriptPlugin(p, &fakeRecorder{})
	d.scripts.wait()

	esc := notes.Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != cloudDSNKey() {
		t.Fatalf("escalations = %+v, want the empty-DSN item after a passing run", esc)
	}
	if strings.Contains(esc[0].Escalation.Reason, "failed") {
		t.Errorf("reason = %q: the run was not a passing one", esc[0].Escalation.Reason)
	}
}

// The DSN item belongs to the check alone: a failed run and then a passing run
// over the same empty file never close it, so it is not raised again each time
// a run passes.
func TestCloudLedgerDSNItemSurvivesAFailedThenPassingRun(t *testing.T) {
	t.Parallel()

	dsnPath := cloudDSNFile(t, "")
	notes := notifyfake.New()
	d, _ := cloudDSNDaemon(t, townWithCloud(t, map[string]string{"ledger_dsn_file": dsnPath}), notes, newFixedClock(), nil)
	p := scriptPlugin(t, cloudCheckPlugin, "exit 1\n")

	d.startScriptPlugin(p, &fakeRecorder{})
	d.scripts.wait()
	d.execCmd = scriptBash(cliReply{}).run // the next run passes
	d.startScriptPlugin(p, &fakeRecorder{})
	d.scripts.wait()

	for _, c := range notes.Clears() {
		if slices.Contains(c.Fingerprints, cloudDSNKey()) {
			t.Fatalf("a passing run cleared the DSN item (%q); the check owns it", c.Reason)
		}
	}
	if !d.cloudDSN.isRaised() {
		t.Error("the DSN item is no longer recorded as raised")
	}
}

// A failed run over an empty DSN file keeps the plugin's own failure key: the
// empty-DSN item is the check's to raise and clear, not the failure's. The empty
// file is never read as a DSN, so the ledger is not opened.
func TestCloudLedgerDaemonLeavesAnEmptyDSNToTheCheck(t *testing.T) {
	t.Parallel()

	l := &ledgertest.Ledger{Report: cloudLedgerReportRow(t)}
	_, path := ledgertest.Register(t, l)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	key, summary := cloudCheckAlertFor(t, townWithCloud(t, map[string]string{"ledger_dsn_file": path}))
	if key != "" || summary != "" {
		t.Errorf("key %q summary %q, want the plugin's own key and no finding set", key, summary)
	}
	if l.Opens() != 0 {
		t.Errorf("the ledger was opened %d time(s) for an empty file", l.Opens())
	}
}
