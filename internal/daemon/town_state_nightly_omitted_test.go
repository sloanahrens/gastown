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

// TestTownStateNightlyOmittedNamesAreQuoted: a file name is not a trusted string, so
// the omission line carries each name quoted and cut, and withholds a
// credential-shaped one: a newline, a carriage return, an escape and a NUL are
// shown escaped (no raw control byte reaches the line), a 200-character name is
// cut at 80 bytes inside its quotes, a token-shaped name is withheld, and the
// cap of twenty names keeps its "and K more" tail (gt-ea3yjo).
func TestTownStateNightlyOmittedNamesAreQuoted(t *testing.T) {
	t.Parallel()
	token := "settings/ghp_" + strings.Repeat("a", 36) + ".json"
	long := "settings/" + strings.Repeat("n", 200) + ".json"
	for _, tc := range []struct {
		name    string
		names   []string
		want    string
		notWant []string
	}{
		{"a plain name", []string{"settings/a.json"}, `"settings/a.json"`, nil},
		{"a newline that forges a line", []string{"settings/x\n2026/11/21 [daemon] night omitted 0 file(s): "}, `\n2026/11/21`, []string{"\n"}},
		{"a carriage return, an escape and a NUL", []string{"settings/a\r\x1b[31m\x00.json"}, `\r\x1b[31m\x00`, []string{"\r", "\x1b", "\x00"}},
		{"a 200-character name", []string{long}, `..."`, []string{strings.Repeat("n", 100)}},
		{"a credential-shaped name", []string{token}, `"<name withheld: credential-shaped>"`, []string{"ghp_"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := townStateOmittedNames(tc.names)
			if !strings.Contains(got, tc.want) {
				t.Errorf("townStateOmittedNames = %q, want it to contain %q", got, tc.want)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("townStateOmittedNames = %q, must not contain %q", got, bad)
				}
			}
		})
	}
	t.Run("more than twenty names", func(t *testing.T) {
		t.Parallel()
		var names []string
		for i := 0; i < omittedNamesShown+1; i++ {
			names = append(names, fmt.Sprintf("settings/n-%02d.json", i))
		}
		got := townStateOmittedNames(names)
		if !strings.HasSuffix(got, ", and 1 more") || strings.Count(got, `"settings/`) != omittedNamesShown {
			t.Errorf("townStateOmittedNames = %q, want twenty quoted names and an \"and 1 more\" tail", got)
		}
	})
}

// TestTownStateNightlyOmittedNamesEndToEnd: a night that omits a file named with a
// newline and a forged daemon line, and one named with a token, logs ONE omission
// line, and the token is nowhere in the log (gt-ea3yjo).
func TestTownStateNightlyOmittedNamesEndToEnd(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	forged := "settings/x\n2026/11/21 03:00:00 [daemon] town_state_backup: night omitted 0 file(s): \n.json"
	named := "settings/ghp_" + strings.Repeat("a", 36) + ".json"
	for _, name := range []string{forged, named} {
		writeTownFile(t, town, name, `{"note":"`+omittedNightlyToken+`"}`)
	}
	var logs strings.Builder
	d, _ := nightlyOmittedDaemon(t, town, &logs)
	if err := d.maintenanceTownStateBackup(t.TempDir(), nightlyTestNow()); err != nil {
		t.Fatalf("maintenanceTownStateBackup = %v", err)
	}
	logged := logs.String()
	lines := 0
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "night omitted") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("the log has %d lines naming an omission, want one:\n%s", lines, logged)
	}
	if strings.Contains(logged, "ghp_") {
		t.Errorf("the log carries the credential-shaped name:\n%s", logged)
	}
}
