package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// cloudFindingFixture is an urgent finding unless severity says otherwise.
func cloudFindingFixture(id, kind string) cloudFinding {
	return cloudFinding{ID: id, Kind: kind, Project: "gt-platform-m4xq", Resource: "resource/" + id, Severity: cloudUrgent}
}

// cloudReportsFixture writes a reports directory holding report.json with the
// given findings and returns its path.
func cloudReportsFixture(t *testing.T, findings ...cloudFinding) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(cloudReport{Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cloudReportFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadCloudReport(t *testing.T) {
	t.Parallel()

	t.Run("reads the findings", func(t *testing.T) {
		t.Parallel()
		dir := cloudReportsFixture(t, cloudFindingFixture("f1", "api_enabled"))
		rep, err := readCloudReport(dir)
		if err != nil {
			t.Fatalf("readCloudReport: %v", err)
		}
		if len(rep.Findings) != 1 || rep.Findings[0].ID != "f1" {
			t.Errorf("findings = %+v", rep.Findings)
		}
	})

	t.Run("a missing report is an error, not an empty one", func(t *testing.T) {
		t.Parallel()
		if _, err := readCloudReport(t.TempDir()); err == nil {
			t.Fatal("a missing report read as valid")
		}
	})

	t.Run("an oversized report is refused before it is read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		big := make([]byte, cloudReportMaxBytes+1)
		if err := os.WriteFile(filepath.Join(dir, cloudReportFile), big, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCloudReport(dir); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v, want the size refusal", err)
		}
	})

	t.Run("a report that is not a regular file is refused unread", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, cloudReportFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := readCloudReport(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want the regular-file refusal", err)
		}
	})

	t.Run("malformed JSON is an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, cloudReportFile), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCloudReport(dir); err == nil {
			t.Fatal("malformed JSON read as valid")
		}
	})

	t.Run("a file that cannot be read is an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, cloudReportFile), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		gone := errors.New("the report went away mid-read")
		_, err := readCloudReportWith(dir, func(string) ([]byte, error) { return nil, gone })
		if !errors.Is(err, gone) {
			t.Fatalf("err = %v, want the read failure", err)
		}
	})
}

func TestUrgentFindingIDs(t *testing.T) {
	t.Parallel()

	rep := cloudReport{Findings: []cloudFinding{
		cloudFindingFixture("f2", "api_enabled"),
		cloudFindingFixture("f1", "api_enabled"),
		cloudFindingFixture("f0", "service_account"),
		cloudFindingFixture("f2", "api_enabled"), // a repeat names no new finding
		{ID: "n1", Kind: "state_missing", Severity: "normal"},
		{ID: "l1", Kind: "budget", Severity: "low"},
	}}
	ids, kinds := urgentFindingIDs(rep)
	if want := []string{"f0", "f1", "f2"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if kinds["api_enabled"] != 2 || kinds["service_account"] != 1 || len(kinds) != 2 {
		t.Errorf("kinds = %v, want only the urgent findings", kinds)
	}
}

// A report without finding ids still keys on what names the resource, and one
// with nothing to name it on still counts in the summary.
func TestUrgentFindingIDsFallsBackToKindAndResource(t *testing.T) {
	t.Parallel()

	rep := cloudReport{Findings: []cloudFinding{
		{Kind: "api_enabled", Project: "p", Resource: "projects/p/apis/a", Severity: "urgent"},
		{Kind: "", Severity: "urgent"},
	}}
	ids, kinds := urgentFindingIDs(rep)
	if len(ids) != 1 || ids[0] != "api_enabled\x00p\x00projects/p/apis/a" {
		t.Errorf("ids = %q, want only the finding that can be named", ids)
	}
	if kinds["api_enabled"] != 1 || kinds["unknown"] != 1 {
		t.Errorf("kinds = %v, want every urgent finding counted", kinds)
	}
}

func TestFindingKind(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"  api_enabled  ":       "api_enabled",
		"":                      "unknown",
		"bad\nkind":             "badkind",
		strings.Repeat("x", 60): strings.Repeat("x", cloudKindMax) + "…",
	}
	for in, want := range cases {
		if got := findingKind(in); got != want {
			t.Errorf("findingKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindingSetKey(t *testing.T) {
	t.Parallel()

	sorted := findingSetKey(cloudCheckPlugin, []string{"f0", "f1"})
	if reordered := findingSetKey(cloudCheckPlugin, []string{"f1", "f0"}); reordered != sorted {
		t.Errorf("the same set keyed differently: %q vs %q", reordered, sorted)
	}
	if want := pluginFailureAlertKey(cloudCheckPlugin) + ":"; !strings.HasPrefix(sorted, want) {
		t.Errorf("key %q does not extend %q", sorted, want)
	}
	if changed := findingSetKey(cloudCheckPlugin, []string{"f0", "f2"}); changed == sorted {
		t.Errorf("a changed finding set kept the key %q", sorted)
	}
	if other := findingSetKey("another-plugin", []string{"f0", "f1"}); other == sorted {
		t.Errorf("two plugins share the key %q", sorted)
	}
}

func TestFindingSummary(t *testing.T) {
	t.Parallel()

	got := findingSummary(map[string]int{"service_account": 1, "api_enabled": 28})
	want := "cloud patrol findings making the check fail: 29 urgent (api_enabled x28, service_account x1)"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// cloudCheckDaemon is a Daemon that runs the cloud-check plugin against a
// report directory, a fake notifier, an injected clock and an injected beads
// store.
func cloudCheckDaemon(t *testing.T, reportDir string, notes *notifyfake.Recorder, clk clockwork.Clock, bd workBeadReader) *Daemon {
	t.Helper()
	if bd == nil {
		bd = beadsfake.New(beadsfake.WithClock(clk))
	}
	d := &Daemon{
		config:          &Config{TownRoot: t.TempDir()},
		logger:          discardLogger,
		ctx:             t.Context(),
		execCmd:         scriptBash(cliReply{stderr: "the patrol holds urgent findings\n", code: 1}).run,
		notifier:        notes,
		clock:           clk,
		cloudReportsDir: func() string { return reportDir },
		openWorkBeads: func([]string, time.Duration) workBeadReader {
			return bd
		},
	}
	return d
}

// writeCloudReport replaces the report in dir, so one test can change the
// finding set between runs.
func writeCloudReport(t *testing.T, dir string, findings ...cloudFinding) {
	t.Helper()
	raw, err := json.Marshal(cloudReport{Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cloudReportFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two consecutive failing runs over the same finding ids raise one escalation;
// the second run refreshes it, and its reason names the finding kinds and
// counts.
func TestCloudCheckEscalationRefreshesTheSameFindingSet(t *testing.T) {
	t.Parallel()

	dir := cloudReportsFixture(t,
		cloudFindingFixture("f0", "api_enabled"),
		cloudFindingFixture("f2", "service_account"),
		cloudFindingFixture("f1", "api_enabled"),
		cloudFinding{ID: "n1", Kind: "state_missing", Severity: "normal"},
	)
	notes := notifyfake.New()
	d := cloudCheckDaemon(t, dir, notes, newFixedClock(), nil)
	p := scriptPlugin(t, cloudCheckPlugin, "exit 1\n")
	rec := &fakeRecorder{}

	d.startScriptPlugin(p, rec)
	d.scripts.wait()
	d.startScriptPlugin(p, rec)
	d.scripts.wait()

	key := findingSetKey(cloudCheckPlugin, []string{"f0", "f1", "f2"})
	calls := notes.Escalations()
	if len(calls) != 2 {
		t.Fatalf("escalations = %d, want the two failing runs", len(calls))
	}
	for _, c := range calls {
		if c.Escalation.Fingerprint != key {
			t.Errorf("run keyed %q, want %q", c.Escalation.Fingerprint, key)
		}
	}
	open := notes.OpenEscalations(key)
	if len(open) != 1 || open[0].Occurrences != 2 {
		t.Fatalf("open = %+v, want one escalation refreshed twice", open)
	}
	for _, want := range []string{"api_enabled x2", "service_account x1", "3 urgent"} {
		if !strings.Contains(open[0].Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, open[0].Reason)
		}
	}
	if strings.Contains(open[0].Reason, "state_missing") {
		t.Errorf("the non-urgent finding is in the escalation:\n%s", open[0].Reason)
	}
}

// A changed finding set raises a new escalation rather than refreshing the
// old one.
func TestCloudCheckEscalationKeysOnTheChangedFindingSet(t *testing.T) {
	t.Parallel()

	dir := cloudReportsFixture(t, cloudFindingFixture("f0", "api_enabled"))
	notes := notifyfake.New()
	d := cloudCheckDaemon(t, dir, notes, newFixedClock(), nil)
	p := scriptPlugin(t, cloudCheckPlugin, "exit 1\n")
	rec := &fakeRecorder{}

	d.startScriptPlugin(p, rec)
	d.scripts.wait()
	first := findingSetKey(cloudCheckPlugin, []string{"f0"})

	writeCloudReport(t, dir,
		cloudFindingFixture("f0", "api_enabled"),
		cloudFindingFixture("f1", "service_account"),
	)
	d.startScriptPlugin(p, rec)
	d.scripts.wait()
	second := findingSetKey(cloudCheckPlugin, []string{"f0", "f1"})

	if first == second {
		t.Fatalf("the changed set kept the key %q", first)
	}
	calls := notes.Escalations()
	if len(calls) != 2 || calls[0].Escalation.Fingerprint != first || calls[1].Escalation.Fingerprint != second {
		t.Fatalf("escalations = %+v, want one per finding set", calls)
	}
	if len(notes.OpenEscalations(first)) != 1 || len(notes.OpenEscalations(second)) != 1 {
		t.Errorf("each finding set must have its own escalation")
	}
}

// A set whose escalation the operator closed is not raised again inside the
// reopen window, and is raised once the window passes.
func TestCloudCheckEscalationWaitsOutAClose(t *testing.T) {
	t.Parallel()

	clk := newFixedClock()
	dir := cloudReportsFixture(t, cloudFindingFixture("f0", "api_enabled"))
	key := findingSetKey(cloudCheckPlugin, []string{"f0"})

	bd := beadsfake.New(beadsfake.WithClock(clk))
	seedClosedEscalation(t, bd, key)

	notes := notifyfake.New()
	d := cloudCheckDaemon(t, dir, notes, clk, bd)
	p := scriptPlugin(t, cloudCheckPlugin, "exit 1\n")
	rec := &fakeRecorder{}

	d.startScriptPlugin(p, rec)
	d.scripts.wait()
	if calls := notes.Escalations(); len(calls) != 0 {
		t.Fatalf("a close minutes ago was re-raised: %+v", calls)
	}

	clk.Advance(pluginEscalationReopenWindow + time.Hour)
	d.startScriptPlugin(p, rec)
	d.scripts.wait()
	calls := notes.Escalations()
	if len(calls) != 1 || calls[0].Escalation.Fingerprint != key {
		t.Fatalf("after the window: escalations = %+v, want one keyed %q", calls, key)
	}
}

// A good run closes every key this daemon raised, not only the last one: the
// key moves with the finding set, and a recovery resolves them all.
func TestClearPluginFailureClosesEveryRaisedKey(t *testing.T) {
	t.Parallel()

	notes := notifyfake.New()
	d := cloudCheckDaemon(t, t.TempDir(), notes, newFixedClock(), nil)
	p := scriptPlugin(t, cloudCheckPlugin, "exit 0\n")
	d.scriptsOnce.Do(func() { d.scripts = newScriptRunner() })

	d.scripts.markEscalated(p.Name, "plugin:cloud-check:failed:aa")
	d.scripts.markEscalated(p.Name, "plugin:cloud-check:failed:bb")
	d.clearPluginFailure(p)

	clears := notes.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %+v, want one call", clears)
	}
	want := []string{"plugin:cloud-check:failed:aa", "plugin:cloud-check:failed:bb"}
	if !slices.Equal(clears[0].Fingerprints, want) {
		t.Errorf("cleared %v, want %v", clears[0].Fingerprints, want)
	}
}

// seedClosedEscalation files an escalation under key's fingerprint label and
// closes it, as the operator's close leaves it.
func seedClosedEscalation(t *testing.T, bd *beadsfake.Fake, key string) {
	t.Helper()
	is, err := bd.Create(beads.CreateOptions{
		Title:     "plugin:cloud-check: an earlier failure",
		Labels:    []string{"gt:escalation", notify.FingerprintLabel(key)},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("seeding the escalation: %v", err)
	}
	if err := bd.CloseWithReason("tracked elsewhere", is.ID); err != nil {
		t.Fatalf("closing the escalation: %v", err)
	}
}

// A plugin other than cloud-check keeps its plain key and its existing
// re-raise behavior: the finding-set key is the cloud check's alone.
func TestPluginFailureKeyIsUnchangedForOtherPlugins(t *testing.T) {
	t.Parallel()

	notes := notifyfake.New()
	dir := cloudReportsFixture(t, cloudFindingFixture("f0", "api_enabled"))
	d := cloudCheckDaemon(t, dir, notes, newFixedClock(), nil)
	p := scriptPlugin(t, "rebuild-gt", "exit 1\n")
	rec := &fakeRecorder{}

	d.startScriptPlugin(p, rec)
	d.scripts.wait()

	calls := notes.Escalations()
	if len(calls) != 1 || calls[0].Escalation.Fingerprint != pluginFailureAlertKey("rebuild-gt") {
		t.Fatalf("escalations = %+v, want the plain plugin key", calls)
	}
	if strings.Contains(calls[0].Escalation.Reason, "cloud patrol findings") {
		t.Errorf("another plugin's escalation carries the cloud findings:\n%s", calls[0].Escalation.Reason)
	}
}
