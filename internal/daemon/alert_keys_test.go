package daemon

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// daemonWithRecorder returns a Daemon whose mail, nudges and escalations go
// to a notifyfake.Recorder instead of a gt subprocess.
func daemonWithRecorder(t *testing.T) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	return &Daemon{
		logger:   discardLogger,
		config:   &Config{TownRoot: t.TempDir()},
		notifier: rec,
	}, rec
}

// TestEscalateAlert_SendsTheStableKey pins the contract the dedupe depends on:
// the daemon must hand gt escalate the same key every time a condition fires,
// or a persisting condition mints a new bead per patrol cycle (gt-vwry).
func TestEscalateAlert_SendsTheStableKey(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", "spike detected:\nhq 1952 -> 932")

	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one", got)
	}
	want := notify.Escalation{
		Severity:    "HIGH",
		Description: "jsonl_git_backup: spike detected:",
		Reason:      "spike detected:\nhq 1952 -> 932",
		Fingerprint: alertKeyJSONLSpike,
	}
	if got[0].Escalation != want {
		t.Errorf("escalation = %+v\nwant         %+v", got[0].Escalation, want)
	}
}

// TestEscalate_DerivesKeyFromTitleForUnkeyedProducers covers the producers that
// hand over a message whose first line already names the condition: the key
// falls out of the title, so they dedupe without naming a class.
func TestEscalate_DerivesKeyFromTitleForUnkeyedProducers(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	d.escalate("compactor_dog", "compact hq: 900 commits")

	got := rec.Escalations()
	want := escalationTitle("compactor_dog", "compact hq: 900 commits")
	if len(got) != 1 || got[0].Escalation.Fingerprint != want {
		t.Errorf("escalations = %+v, want one keyed %q", got, want)
	}
}

// TestClearAlerts_OneInvocationCarriesEveryKey: a producer clears its whole
// owned key set on a healthy cycle, and that must cost one gt invocation, not
// one per key.
func TestClearAlerts_OneInvocationCarriesEveryKey(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	d.clearAlerts("backup cycle clean", alertKeyJSONLSpike, alertKeyJSONLPush)

	got := rec.Calls()
	if len(got) != 1 || got[0].Kind != notifyfake.KindClear {
		t.Fatalf("calls = %+v, want exactly one clear", got)
	}
	if got[0].Reason != "backup cycle clean" || !slices.Equal(got[0].Fingerprints, []string{alertKeyJSONLSpike, alertKeyJSONLPush}) {
		t.Errorf("clear = %+v, want both keys under the given reason", got[0])
	}
}

func TestClearAlerts_NoKeysIsANoOp(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	d.clearAlerts("nothing to clear")

	if calls := rec.Calls(); len(calls) != 0 {
		t.Errorf("expected no send when there are no keys, got %+v", calls)
	}
}

// TestAlertKeysAreDistinctAndNonEmpty pins the keys themselves. A key that
// collides with another condition's, or one that drifts, silently breaks one of
// the two halves of the lifecycle — dedupe would merge unrelated alerts, or
// auto-close would stop finding the alert it is meant to close.
func TestAlertKeysAreDistinctAndNonEmpty(t *testing.T) {
	t.Parallel()
	keys := map[string]string{
		"alertKeyJSONLInit":   alertKeyJSONLInit,
		"alertKeyJSONLNoDBs":  alertKeyJSONLNoDBs,
		"alertKeyJSONLExport": alertKeyJSONLExport,
		"alertKeyJSONLScrub":  alertKeyJSONLScrub,
		"alertKeyJSONLSpike":  alertKeyJSONLSpike,
		"alertKeyJSONLPush":   alertKeyJSONLPush,
	}

	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		if key == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if prev, ok := seen[key]; ok {
			t.Errorf("%s and %s share the key %q; their alerts would merge", prev, name, key)
			continue
		}
		seen[key] = name
	}
}
