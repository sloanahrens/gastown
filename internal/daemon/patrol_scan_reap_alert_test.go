package daemon

import (
	"io"
	"log"
	"regexp"
	"strings"
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// recordingAlerts is the reapAlertSink the tests inject: it records the calls
// the pass would make to gt escalate instead of shelling out.
type recordingAlerts struct {
	raised  []alertRaise
	cleared []alertClear
}

type alertRaise struct{ key, source, message string }

type alertClear struct {
	reason string
	keys   []string
}

func (r *recordingAlerts) Raise(key, source, message string) {
	r.raised = append(r.raised, alertRaise{key: key, source: source, message: message})
}

func (r *recordingAlerts) Clear(reason string, keys ...string) {
	r.cleared = append(r.cleared, alertClear{reason: reason, keys: append([]string(nil), keys...)})
}

// alertDaemon builds the daemon the pass reads its config from, with the alert
// sink replaced by rec.
func alertDaemon(rec *recordingAlerts, wc *agentconfig.WorktreeCleanupConfig) *Daemon {
	return &Daemon{
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true, WorktreeCleanup: wc},
		}},
		reapAlertSink: rec,
	}
}

// blockedReport builds one tick's report from a seat -> detail map.
func blockedReport(rig string, seats map[string]string) patrolscan.Report {
	r := patrolscan.Report{Rig: rig}
	for name, detail := range seats {
		r.Findings = append(r.Findings, patrolscan.Finding{
			Kind: reapFindingKind, Subject: name, Outcome: patrolscan.OutcomeBlocked, Detail: detail,
		})
	}
	return r
}

// raisedKeys flattens the recorded raises, in call order.
func raisedKeys(rec *recordingAlerts) []string {
	keys := make([]string, 0, len(rec.raised))
	for _, a := range rec.raised {
		keys = append(keys, a.key)
	}
	return keys
}

// clearedKeys flattens every recorded clear, in call order.
func clearedKeys(rec *recordingAlerts) []string {
	var keys []string
	for _, c := range rec.cleared {
		keys = append(keys, c.keys...)
	}
	return keys
}

func TestReapAlertsOnePerBlockedSeat(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{
		"agate":  "verdict PENDING_MR: a merge request is open",
		"basalt": "verdict NEEDS_RECOVERY: git state unknown (not measured live)",
	}), 5)

	want := []string{
		reapBlockedKey("gastown", "agate", "verdict PENDING_MR: a merge request is open"),
		reapBlockedKey("gastown", "basalt", "verdict NEEDS_RECOVERY: git state unknown (not measured live)"),
	}
	if got := raisedKeys(rec); !equalStrings(got, want) {
		t.Fatalf("raised keys = %v, want %v", got, want)
	}
	for _, a := range rec.raised {
		if a.source != "patrol-scan" {
			t.Errorf("source = %q, want patrol-scan", a.source)
		}
	}
	if len(rec.cleared) != 0 {
		t.Errorf("cleared %d key(s) on the first tick, want none", len(rec.cleared))
	}
	// The message must name the seat, the blockers and the operator's next step.
	first := rec.raised[0].message
	for _, want := range []string{"gastown/agate", "PENDING_MR", "gt polecat check-recovery gastown/agate", "--force"} {
		if !strings.Contains(first, want) {
			t.Errorf("message %q does not mention %q", first, want)
		}
	}
}

func TestReapAlertsUnchangedDetailReusesKeys(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	seats := map[string]string{"agate": "verdict PENDING_MR: a merge request is open"}

	d.reapAlerts("gastown", blockedReport("gastown", seats), 5)
	first := raisedKeys(rec)
	d.reapAlerts("gastown", blockedReport("gastown", seats), 5)

	if len(rec.raised) != 2 {
		t.Fatalf("raises = %d, want 2 (one per tick; the CLI dedupes)", len(rec.raised))
	}
	if got := rec.raised[1].key; got != first[0] {
		t.Errorf("second tick key = %q, want the first tick's %q", got, first[0])
	}
	if len(rec.cleared) != 0 {
		t.Errorf("cleared %v on an unchanged detail, want none", clearedKeys(rec))
	}
}

func TestReapAlertsChangedDetailRaisesNewKeyAndClearsOld(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{"agate": "verdict PENDING_MR"}), 5)
	oldKey := rec.raised[0].key
	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}), 5)

	if len(rec.raised) != 2 || rec.raised[1].key == oldKey {
		t.Fatalf("raises = %v, want a second raise under a new key", raisedKeys(rec))
	}
	if got := clearedKeys(rec); !equalStrings(got, []string{oldKey}) {
		t.Fatalf("cleared = %v, want the stale key %q", got, oldKey)
	}
}

func TestReapAlertsClearsSeatThatStopsBeingBlocked(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}), 5)
	key := rec.raised[0].key

	// Next tick the seat is reaped, not blocked: no raise, one clear.
	reaped := patrolscan.Report{Rig: "gastown", Findings: []patrolscan.Finding{
		{Kind: reapFindingKind, Subject: "agate", Outcome: patrolscan.OutcomeReaped, Detail: "idle 31m"},
	}}
	d.reapAlerts("gastown", reaped, 5)

	if len(rec.raised) != 1 {
		t.Errorf("raises = %d, want only the first tick's", len(rec.raised))
	}
	if got := clearedKeys(rec); !equalStrings(got, []string{key}) {
		t.Fatalf("cleared = %v, want the seat's key %q", got, key)
	}
	// A third tick with the seat still gone must not clear it again: the pass
	// forgot the key when it cleared it.
	before := len(rec.cleared)
	d.reapAlerts("gastown", reaped, 5)
	if len(rec.cleared) != before {
		t.Errorf("re-cleared an already cleared key: %v", rec.cleared[before:])
	}
}

func TestReapAlertsFailedRigReadClearsNothing(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"}), 5)
	d.reapAlerts("gastown", blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"}), 5)

	// The next tick could not list the rig: no findings, one error. The seats'
	// absence is unobserved, so no alert may be cleared on the strength of it.
	failed := patrolscan.Report{Rig: "gastown", Errors: []string{"listing polecats: connection refused"}}
	d.reapAlerts("gastown", failed, 5)

	if len(rec.cleared) != 0 {
		t.Fatalf("a failed rig read cleared %v, want nothing", clearedKeys(rec))
	}
	// The keys are still remembered: a later complete tick clears them.
	d.reapAlerts("gastown", patrolscan.Report{Rig: "gastown"}, 5)
	if n := len(clearedKeys(rec)); n != 2 {
		t.Fatalf("cleared %d key(s) on the complete tick, want 2 (%v)", n, clearedKeys(rec))
	}
}

func TestReapAlertsThresholdFiresAtTheCountAndClearsBelow(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	thresholdKey := reapThresholdKey("gastown")

	two := blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"})
	one := blockedReport("gastown", map[string]string{"a": "d1"})

	d.reapAlerts("gastown", one, 2) // below: no threshold alert
	if containsKey(raisedKeys(rec), thresholdKey) {
		t.Fatalf("threshold alert fired below the threshold: %v", raisedKeys(rec))
	}

	rec = &recordingAlerts{}
	d = alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	d.reapAlerts("gastown", two, 2) // at: one threshold alert
	if n := count(raisedKeys(rec), thresholdKey); n != 1 {
		t.Fatalf("threshold raises at the count = %d, want 1 (%v)", n, raisedKeys(rec))
	}
	d.reapAlerts("gastown", two, 2) // still at: a repeat, not a clear
	if n := count(raisedKeys(rec), thresholdKey); n != 2 {
		t.Fatalf("threshold raises while still at the count = %d, want 2 (a repeat firing)", n)
	}
	if containsKey(clearedKeys(rec), thresholdKey) {
		t.Fatalf("threshold alert cleared while still at the count: %v", clearedKeys(rec))
	}

	d.reapAlerts("gastown", one, 2) // below again: cleared once
	if n := count(clearedKeys(rec), thresholdKey); n != 1 {
		t.Fatalf("threshold clears below the count = %d, want 1 (%v)", n, clearedKeys(rec))
	}
	d.reapAlerts("gastown", one, 2) // still below: no second clear
	if n := count(clearedKeys(rec), thresholdKey); n != 1 {
		t.Fatalf("threshold re-cleared while still below: %d clears", n)
	}
}

func TestReapAlertsUnsetThresholdDefaultsToFive(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	seats := make(map[string]string, 4)
	for _, name := range []string{"a", "b", "c", "d"} {
		seats[name] = "blocked"
	}
	d.reapAlerts("gastown", blockedReport("gastown", seats), 0)

	if containsKey(raisedKeys(rec), reapThresholdKey("gastown")) {
		t.Fatalf("unset threshold behaved as unlimited: %v", raisedKeys(rec))
	}
}

func TestReapAlertsDisabledRaisesNothing(t *testing.T) {
	t.Parallel()
	for name, wc := range map[string]*agentconfig.WorktreeCleanupConfig{
		"nil block":     nil,
		"enabled false": {Enabled: false, Rigs: []string{"gastown"}},
		"other rig":     {Enabled: true, Rigs: []string{"beads"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingAlerts{}
			d := alertDaemon(rec, wc)
			d.reapAlertsIfEnabled("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}))
			if len(rec.raised) != 0 || len(rec.cleared) != 0 {
				t.Fatalf("disabled pass alerted: raised=%v cleared=%v", raisedKeys(rec), clearedKeys(rec))
			}
		})
	}
}

func TestReapAlertsEnabledCoversRigAndUsesConfigThreshold(t *testing.T) {
	t.Parallel()
	rec := &recordingAlerts{}
	d := alertDaemon(rec, &agentconfig.WorktreeCleanupConfig{
		Enabled: true, Rigs: []string{"gastown"}, BlockedAlertThreshold: 1,
	})

	d.reapAlertsIfEnabled("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}))

	if !containsKey(raisedKeys(rec), reapThresholdKey("gastown")) {
		t.Fatalf("config threshold 1 did not fire: %v", raisedKeys(rec))
	}
	if !containsKey(raisedKeys(rec), reapBlockedKey("gastown", "agate", "verdict NEEDS_RECOVERY")) {
		t.Fatalf("per-seat alert missing: %v", raisedKeys(rec))
	}
}

func TestReapBlockedKeyShape(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`^reap-blocked:gastown/agate:[0-9a-f]{8}$`)
	key := reapBlockedKey("gastown", "agate", "verdict PENDING_MR")
	if !re.MatchString(key) {
		t.Fatalf("key = %q, want reap-blocked:<rig>/<name>:<8 hex>", key)
	}
	if again := reapBlockedKey("gastown", "agate", "verdict PENDING_MR"); again != key {
		t.Errorf("key is not stable: %q != %q", again, key)
	}
	if other := reapBlockedKey("gastown", "agate", "verdict NEEDS_RECOVERY"); other == key {
		t.Error("a changed detail kept the key")
	}
	if other := reapBlockedKey("gastown", "basalt", "verdict PENDING_MR"); other == key {
		t.Error("a different seat kept the key")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func count(keys []string, want string) int {
	n := 0
	for _, k := range keys {
		if k == want {
			n++
		}
	}
	return n
}

func containsKey(keys []string, want string) bool { return count(keys, want) > 0 }
