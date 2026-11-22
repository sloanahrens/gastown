package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/townbackup"
)

// omittedNightlyToken is token-shaped and built from parts, so no credential
// literal sits in this file.
const omittedNightlyToken = "sk-" + "abcdefghijklmnop1234"

// nightlyOmittedDaemon is the nightly harness with the notifier's alerts and
// clears recorded, so a test never reaches `gt escalate`.
func nightlyOmittedDaemon(t *testing.T, town string, logs *strings.Builder) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	d := nightlyTestDaemon(town, logs)
	rec := notifyfake.New()
	d.notifier = rec
	return d, rec
}

// TestTownStateNightlyOmittedLogsTheFiles is the first acceptance box: a night
// that omits files logs one line naming the count and both paths, commits the
// night, and returns nil.
func TestTownStateNightlyOmittedLogsTheFiles(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	for _, name := range []string{"settings/a-notes.json", "settings/b-notes.json"} {
		writeTownFile(t, town, name, `{"note":"`+omittedNightlyToken+`"}`)
	}
	root := t.TempDir()
	now := nightlyTestNow()
	var logs strings.Builder
	d, _ := nightlyOmittedDaemon(t, town, &logs)

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("maintenanceTownStateBackup = %v, want the night to commit despite the omissions", err)
	}

	if _, ok, err := townbackup.Newest(root); err != nil || !ok {
		t.Fatalf("Newest = ok %v, %v; want the committed night", ok, err)
	}

	logged := logs.String()
	if !strings.Contains(logged, "town_state_backup: night omitted 2 file(s):") {
		t.Errorf("the log carries no omission line with the count: %q", logged)
	}
	for _, name := range []string{"settings/a-notes.json", "settings/b-notes.json"} {
		if got := strings.Count(logged, name); got != 1 {
			t.Errorf("the log names %s %d times, want once: %q", name, got, logged)
		}
	}
	if strings.Contains(logged, omittedNightlyToken) {
		t.Errorf("the log carries the credential's value: %q", logged)
	}
}

// TestTownStateNightlyOmittedConfigAlertsThenClears is the second acceptance
// box: a night whose export leaves the town config out raises one alert under
// the stable key, and a later night that carries the config clears it.
func TestTownStateNightlyOmittedConfigAlertsThenClears(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	writeTownFile(t, town, "settings/config.json", `{"theme":"dark","note":"`+omittedNightlyToken+`"}`)
	root := t.TempDir()
	var logs strings.Builder
	d, rec := nightlyOmittedDaemon(t, town, &logs)

	if err := d.maintenanceTownStateBackup(root, nightlyTestNow()); err != nil {
		t.Fatalf("maintenanceTownStateBackup (omitting night) = %v, want the night to commit", err)
	}

	raised := rec.Escalations()
	if len(raised) != 1 {
		t.Fatalf("escalations = %+v, want exactly one for the omitted config", raised)
	}
	if got := raised[0].Escalation.Fingerprint; got != alertKeyTownStateConfigOmitted {
		t.Errorf("escalation fingerprint = %q, want %q", got, alertKeyTownStateConfigOmitted)
	}
	if reason := raised[0].Escalation.Reason; !strings.Contains(reason, "town config") {
		t.Errorf("escalation reason = %q, want it to name the town config", reason)
	}

	// The next night carries a clean config, so the omission is over.
	writeTownFile(t, town, "settings/config.json", `{"theme":"dark"}`)
	if err := d.maintenanceTownStateBackup(root, nightlyTestNow().AddDate(0, 0, 1)); err != nil {
		t.Fatalf("maintenanceTownStateBackup (carrying night) = %v", err)
	}

	cleared := rec.Clears()
	if len(cleared) != 1 {
		t.Fatalf("clears = %+v, want exactly one after the clean night", cleared)
	}
	if got := cleared[0].Fingerprints; len(got) != 1 || got[0] != alertKeyTownStateConfigOmitted {
		t.Errorf("clear fingerprints = %v, want only %q", got, alertKeyTownStateConfigOmitted)
	}
	if cleared[0].Reason != "the night carries the town config" {
		t.Errorf("clear reason = %q", cleared[0].Reason)
	}
	if got := len(rec.Escalations()); got != 1 {
		t.Errorf("escalations = %d after the clean night, want the first night's one only", got)
	}
}

// TestTownStateNightlyOmittedCapsTheNames pins the line's bound: a night that
// omits more than omittedNamesShown files names the first twenty and counts the
// rest, so one line stays short however many files the night loses.
func TestTownStateNightlyOmittedCapsTheNames(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	// The bead fixes the cap at twenty names.
	const files = 21
	for i := 0; i < files; i++ {
		writeTownFile(t, town, fmt.Sprintf("settings/cap-%02d.json", i), `{"note":"`+omittedNightlyToken+`"}`)
	}
	root := t.TempDir()
	var logs strings.Builder
	d, _ := nightlyOmittedDaemon(t, town, &logs)

	if err := d.maintenanceTownStateBackup(root, nightlyTestNow()); err != nil {
		t.Fatalf("maintenanceTownStateBackup = %v", err)
	}

	logged := logs.String()
	if !strings.Contains(logged, fmt.Sprintf("night omitted %d file(s):", files)) {
		t.Errorf("the log carries no omission line with the count: %q", logged)
	}
	if !strings.Contains(logged, "and 1 more") {
		t.Errorf("the log does not count the path past the cap: %q", logged)
	}
	if want := "settings/cap-19.json"; !strings.Contains(logged, want) {
		t.Errorf("the log drops the twentieth path %s: %q", want, logged)
	}
	if last := fmt.Sprintf("settings/cap-%02d.json", files-1); strings.Contains(logged, last) {
		t.Errorf("the log names the path past the cap %s: %q", last, logged)
	}
}

// TestTownStateNightlyOmittedNothingIsQuiet is the third acceptance box: a
// night that omits nothing logs no omission line and raises no alert.
func TestTownStateNightlyOmittedNothingIsQuiet(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	root := t.TempDir()
	var logs strings.Builder
	d, rec := nightlyOmittedDaemon(t, town, &logs)

	if err := d.maintenanceTownStateBackup(root, nightlyTestNow()); err != nil {
		t.Fatalf("maintenanceTownStateBackup = %v", err)
	}

	if logged := logs.String(); strings.Contains(logged, "night omitted") {
		t.Errorf("the log carries an omission line for a clean night: %q", logged)
	}
	if got := rec.Escalations(); len(got) != 0 {
		t.Errorf("escalations = %+v, want none", got)
	}
}

// TestTownStateNightlyOmittedAlertThatDoesNotLandFailsTheNight: the config-omitted
// alert is the one place a lost backup must be loud, so a delivery that fails (after
// the retries) is not read as raised: the night commits what it wrote and reports the
// failure, which the scheduled step alerts on in its own right (gt-zz76o8).
func TestTownStateNightlyOmittedAlertThatDoesNotLandFailsTheNight(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	writeTownFile(t, town, "settings/config.json", `{"theme":"dark","note":"`+omittedNightlyToken+`"}`)
	root := t.TempDir()
	var logs strings.Builder
	d, rec := nightlyOmittedDaemon(t, town, &logs)
	clk := newFixedClock()
	d.clock = clk
	rec.Fail(notifyfake.KindEscalate, errors.New("gt escalate: exit status 1 (<no output>)"))

	var err error
	runOnClock(t, clk, time.Second, func() { err = d.maintenanceTownStateBackup(root, nightlyTestNow()) })

	if err == nil || !strings.Contains(err.Error(), "did not reach the town") {
		t.Fatalf("maintenanceTownStateBackup = %v, want the night to report that the config alert did not reach the town", err)
	}
	if _, ok, nerr := townbackup.Newest(root); nerr != nil || !ok {
		t.Fatalf("Newest = ok %v, %v; want the night committed despite the alert failure", ok, nerr)
	}
	if !strings.Contains(logs.String(), "the config-omitted alert did not reach the town") {
		t.Errorf("the log does not name the undelivered alert: %q", logs.String())
	}
	if strings.Contains(err.Error()+logs.String(), omittedNightlyToken) {
		t.Errorf("the credential's value reached the error or the log")
	}
}
