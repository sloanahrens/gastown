package daemon

import (
	"errors"
	"io"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// recordingAlerts is the reapAlertSink the tests inject: it records the calls
// the pass would make to gt escalate instead of shelling out. failRaise and
// failClear make the next calls of that kind report a dropped delivery, so a
// test can drive the retry-on-next-tick path.
type recordingAlerts struct {
	raised  []alertRaise
	cleared []alertClear

	failRaise bool
	failClear bool
}

type alertRaise struct{ key, source, message string }

type alertClear struct {
	reason string
	keys   []string
}

// errAlertUndelivered stands in for the error gt escalate returns when its
// retries are exhausted (Dolt contention, slot starvation).
var errAlertUndelivered = errors.New("gt escalate did not deliver")

func (r *recordingAlerts) Raise(key, source, message string) error {
	r.raised = append(r.raised, alertRaise{key: key, source: source, message: message})
	if r.failRaise {
		return errAlertUndelivered
	}
	return nil
}

func (r *recordingAlerts) Clear(reason string, keys ...string) error {
	r.cleared = append(r.cleared, alertClear{reason: reason, keys: append([]string(nil), keys...)})
	if r.failClear {
		return errAlertUndelivered
	}
	return nil
}

// alertRig is one daemon's blocked-seat alert pass over a temp town root, plus
// the recorder its sink writes to. restart rebuilds the daemon, recorder and
// ledger over the same root with empty in-memory state — the daemon-restart
// case, where the persisted store and ledger survive but the memory does not.
type alertRig struct {
	t      *testing.T
	root   string
	wc     *agentconfig.WorktreeCleanupConfig
	daemon *Daemon
	rec    *recordingAlerts
	ledger *patrolscan.FileLedger
}

func newAlertRig(t *testing.T, wc *agentconfig.WorktreeCleanupConfig) *alertRig {
	t.Helper()
	r := &alertRig{t: t, root: t.TempDir(), wc: wc}
	r.start()
	return r
}

func (r *alertRig) start() {
	r.t.Helper()
	rec := &recordingAlerts{}
	r.daemon = &Daemon{
		logger: log.New(io.Discard, "", 0),
		config: &Config{TownRoot: r.root},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true, WorktreeCleanup: r.wc},
		}},
		reapAlertSink: rec,
	}
	r.rec = rec
	r.ledger = patrolscan.NewFileLedger(patrolscan.LedgerPath(r.root), 7*24*time.Hour)
}

// restart is a daemon restart: fresh in-memory state, the store and ledger
// re-read from the same town root.
func (r *alertRig) restart() { r.start() }

func (r *alertRig) tick(rig string, report patrolscan.Report, threshold int) {
	r.daemon.reapAlerts(rig, report, threshold, r.ledger)
}

func (r *alertRig) tickIfEnabled(rig string, report patrolscan.Report) {
	r.daemon.reapAlertsIfEnabled(rig, report, r.ledger)
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
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	r.tick("gastown", blockedReport("gastown", map[string]string{
		"agate":  "verdict PENDING_MR: a merge request is open",
		"basalt": "verdict NEEDS_RECOVERY: git state unknown (not measured live)",
	}), 5)

	want := []string{
		reapBlockedKey("gastown", "agate", "verdict PENDING_MR: a merge request is open"),
		reapBlockedKey("gastown", "basalt", "verdict NEEDS_RECOVERY: git state unknown (not measured live)"),
	}
	if got := raisedKeys(r.rec); !equalStrings(got, want) {
		t.Fatalf("raised keys = %v, want %v", got, want)
	}
	for _, a := range r.rec.raised {
		if a.source != "patrol-scan" {
			t.Errorf("source = %q, want patrol-scan", a.source)
		}
	}
	if len(r.rec.cleared) != 0 {
		t.Errorf("cleared %d key(s) on the first tick, want none", len(r.rec.cleared))
	}
	// The message must name the seat, the blockers and the operator's next step.
	first := r.rec.raised[0].message
	for _, want := range []string{"gastown/agate", "PENDING_MR", "gt polecat check-recovery gastown/agate", "--force"} {
		if !strings.Contains(first, want) {
			t.Errorf("message %q does not mention %q", first, want)
		}
	}
}

func TestReapAlertsThrottlesAnUnchangedSeat(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	seats := map[string]string{"agate": "verdict PENDING_MR: a merge request is open"}

	for i := 0; i < 3; i++ {
		r.tick("gastown", blockedReport("gastown", seats), 5)
	}

	if n := len(r.rec.raised); n != 1 {
		t.Fatalf("raises over 3 ticks = %d, want 1 (once per report window, gt-g49yk)", n)
	}
	if len(r.rec.cleared) != 0 {
		t.Errorf("cleared %v on an unchanged detail, want none", clearedKeys(r.rec))
	}
}

func TestReapAlertsRaisesAgainAfterTheWindow(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	r.daemon.patrolConfig.Patrols.PatrolScan.ReportWindow = "1h"
	clk := clockwork.NewFakeClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	r.daemon.clock = clk

	seats := map[string]string{"agate": "verdict PENDING_MR"}
	r.tick("gastown", blockedReport("gastown", seats), 5)

	clk.Advance(30 * time.Minute)
	r.tick("gastown", blockedReport("gastown", seats), 5)
	if n := len(r.rec.raised); n != 1 {
		t.Fatalf("raises inside the window = %d, want 1", n)
	}

	clk.Advance(2 * time.Hour)
	r.tick("gastown", blockedReport("gastown", seats), 5)
	if n := len(r.rec.raised); n != 2 {
		t.Fatalf("raises after the window = %d, want 2 (a repeat re-notifies)", n)
	}
}

func TestReapAlertsChangedDetailRaisesNewKeyAndClearsOld(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict PENDING_MR"}), 5)
	oldKey := r.rec.raised[0].key
	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}), 5)

	if len(r.rec.raised) != 2 || r.rec.raised[1].key == oldKey {
		t.Fatalf("raises = %v, want a second raise under a new key", raisedKeys(r.rec))
	}
	if got := clearedKeys(r.rec); !equalStrings(got, []string{oldKey}) {
		t.Fatalf("cleared = %v, want the stale key %q", got, oldKey)
	}
}

func TestReapAlertsClearsSeatThatStopsBeingBlocked(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}), 5)
	key := r.rec.raised[0].key

	// Next tick the seat is reaped, not blocked: no raise, one clear.
	reaped := patrolscan.Report{Rig: "gastown", Findings: []patrolscan.Finding{
		{Kind: reapFindingKind, Subject: "agate", Outcome: patrolscan.OutcomeReaped, Detail: "idle 31m"},
	}}
	r.tick("gastown", reaped, 5)

	if len(r.rec.raised) != 1 {
		t.Errorf("raises = %d, want only the first tick's", len(r.rec.raised))
	}
	if got := clearedKeys(r.rec); !equalStrings(got, []string{key}) {
		t.Fatalf("cleared = %v, want the seat's key %q", got, key)
	}
	// A third tick with the seat still gone must not clear it again: the pass
	// forgot the key when it cleared it.
	before := len(r.rec.cleared)
	r.tick("gastown", reaped, 5)
	if len(r.rec.cleared) != before {
		t.Errorf("re-cleared an already cleared key: %v", r.rec.cleared[before:])
	}
}

func TestReapAlertsFailedRigReadClearsNothing(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	r.tick("gastown", blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"}), 5)
	r.tick("gastown", blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"}), 5)

	// The next tick could not list the rig: no findings, one error. The seats'
	// absence is unobserved, so no alert may be cleared on the strength of it.
	failed := patrolscan.Report{Rig: "gastown", Errors: []string{"listing polecats: connection refused"}}
	r.tick("gastown", failed, 5)

	if len(r.rec.cleared) != 0 {
		t.Fatalf("a failed rig read cleared %v, want nothing", clearedKeys(r.rec))
	}
	// The keys are still remembered: a later complete tick clears them.
	r.tick("gastown", patrolscan.Report{Rig: "gastown"}, 5)
	if n := len(clearedKeys(r.rec)); n != 2 {
		t.Fatalf("cleared %d key(s) on the complete tick, want 2 (%v)", n, clearedKeys(r.rec))
	}
}

// TestReapAlertsFailedRaiseIsRetried covers the raise half of gt-s3u9a: a raise
// gt escalate dropped must leave the throttle ledger and the persisted seat
// state untouched, so the next tick raises the alert again instead of the
// window swallowing it.
func TestReapAlertsFailedRaiseIsRetried(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	report := blockedReport("gastown", map[string]string{"agate": "verdict PENDING_MR"})
	key := reapBlockedKey("gastown", "agate", "verdict PENDING_MR")

	r.rec.failRaise = true
	r.tick("gastown", report, 5)

	if n := len(r.rec.raised); n != 1 {
		t.Fatalf("raise attempts after a dropped delivery = %d, want 1", n)
	}
	if _, ok := r.ledger.LastReported(reapAlertKeyPrefix + key); ok {
		t.Fatalf("a dropped raise was recorded in the throttle ledger")
	}

	// A tick with the seat recovered has no key to clear: the failed raise
	// never entered the persisted state either.
	r.rec.failRaise = false
	r.tick("gastown", patrolscan.Report{Rig: "gastown"}, 5)
	if n := len(r.rec.cleared); n != 0 {
		t.Fatalf("cleared %v after a raise that never landed", clearedKeys(r.rec))
	}

	// Delivery now works: the alert is raised, and only then throttled.
	r.tick("gastown", report, 5)
	if n := len(r.rec.raised); n != 2 {
		t.Fatalf("raise attempts = %d, want the dropped raise retried", n)
	}
	r.tick("gastown", report, 5)
	if n := len(r.rec.raised); n != 2 {
		t.Fatalf("raise attempts = %d, want the landed alert throttled", n)
	}
}

// TestReapAlertsFailedClearIsRetried covers the clear half of gt-s3u9a: a clear
// gt escalate dropped must keep the key in the persisted state, so the next
// tick clears the still-open escalation again instead of losing it.
func TestReapAlertsFailedClearIsRetried(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}), 5)
	key := r.rec.raised[0].key

	reaped := patrolscan.Report{Rig: "gastown", Findings: []patrolscan.Finding{
		{Kind: reapFindingKind, Subject: "agate", Outcome: patrolscan.OutcomeReaped, Detail: "idle 31m"},
	}}

	r.rec.failClear = true
	r.tick("gastown", reaped, 5)
	if got := clearedKeys(r.rec); !equalStrings(got, []string{key}) {
		t.Fatalf("clear attempts = %v, want the seat's key %q", got, key)
	}

	// The clear did not land, so the key survives: the next tick clears again.
	r.rec.failClear = false
	r.tick("gastown", reaped, 5)
	if got := clearedKeys(r.rec); !equalStrings(got, []string{key, key}) {
		t.Fatalf("cleared = %v, want the key retried on the next tick", got)
	}

	// Landed: the pass forgets it and does not clear a third time.
	r.tick("gastown", reaped, 5)
	if n := len(r.rec.cleared); n != 2 {
		t.Fatalf("clear attempts = %d, want 2 (no clear once it landed)", n)
	}
}

// TestReapAlertsChangedDetailRetriesTheOldClear: when the blocker set changes,
// the stale key is cleared before the new one is raised. A dropped clear keeps
// the state pointing at the old key, so the next tick retries the clear and
// raises the new key only once the old escalation is gone.
func TestReapAlertsChangedDetailRetriesTheOldClear(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict PENDING_MR"}), 5)
	oldKey := r.rec.raised[0].key
	changed := blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"})

	r.rec.failClear = true
	r.tick("gastown", changed, 5)
	if got := clearedKeys(r.rec); !equalStrings(got, []string{oldKey}) {
		t.Fatalf("cleared = %v, want the stale key %q", got, oldKey)
	}
	if n := len(r.rec.raised); n != 1 {
		t.Fatalf("raised = %d, want the new key held back until the clear lands", n)
	}

	r.rec.failClear = false
	r.tick("gastown", changed, 5)
	if got := clearedKeys(r.rec); !equalStrings(got, []string{oldKey, oldKey}) {
		t.Fatalf("cleared = %v, want the clear retried", got)
	}
	if n := len(r.rec.raised); n != 2 || r.rec.raised[1].key == oldKey {
		t.Fatalf("raised = %v, want the changed seat's new key after the clear", raisedKeys(r.rec))
	}
}

// TestReapAlertsThresholdDeliveryGatesState covers the threshold alert's half
// of gt-s3u9a: a dropped raise leaves the alert unopened so it is retried, and
// a dropped clear leaves it open.
func TestReapAlertsThresholdDeliveryGatesState(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	thKey := reapThresholdKey("gastown")
	two := blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"})
	one := blockedReport("gastown", map[string]string{"a": "d1"})

	r.rec.failRaise = true
	r.tick("gastown", two, 2)
	if n := count(raisedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold raise attempts = %d, want 1", n)
	}
	r.rec.failRaise = false
	r.tick("gastown", two, 2)
	if n := count(raisedKeys(r.rec), thKey); n != 2 {
		t.Fatalf("threshold raises = %d, want the dropped raise retried", n)
	}

	r.rec.failClear = true
	r.tick("gastown", one, 2)
	if n := count(clearedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold clear attempts = %d, want 1", n)
	}
	r.rec.failClear = false
	r.tick("gastown", one, 2)
	if n := count(clearedKeys(r.rec), thKey); n != 2 {
		t.Fatalf("threshold clears = %d, want the dropped clear retried", n)
	}
	r.tick("gastown", one, 2)
	if n := count(clearedKeys(r.rec), thKey); n != 2 {
		t.Fatalf("threshold clears = %d, want no clear once it landed", n)
	}
}

// TestReapAlertsRestartThrottlesAndClearsRecovered is the restart-safety case:
// a fresh Daemon (empty memory) must not re-raise a seat whose blocker set is
// unchanged inside the window, and must clear a seat that recovered while the
// daemon was down. Both come from the persisted store, not memory.
func TestReapAlertsRestartThrottlesAndClearsRecovered(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	r.tick("gastown", blockedReport("gastown", map[string]string{
		"agate":  "verdict PENDING_MR",
		"basalt": "verdict NEEDS_RECOVERY",
	}), 5)
	if len(r.rec.raised) != 2 {
		t.Fatalf("first tick raised %v, want both seats", raisedKeys(r.rec))
	}

	r.restart()
	r.tick("gastown", blockedReport("gastown", map[string]string{"agate": "verdict PENDING_MR"}), 5)

	if n := len(r.rec.raised); n != 0 {
		t.Fatalf("after restart raises = %d, want 0 (agate unchanged inside the window, %v)", n, raisedKeys(r.rec))
	}
	want := reapBlockedKey("gastown", "basalt", "verdict NEEDS_RECOVERY")
	if got := clearedKeys(r.rec); !equalStrings(got, []string{want}) {
		t.Fatalf("after restart cleared = %v, want the recovered seat's key %q", got, want)
	}
}

// TestReapAlertsThresholdThrottlesAndClearsAfterRestart covers the threshold
// alert's half of gt-g49yk: it follows the same throttle, and a restart still
// clears it once the count falls back below the threshold.
func TestReapAlertsThresholdThrottlesAndClearsAfterRestart(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})
	thKey := reapThresholdKey("gastown")
	two := blockedReport("gastown", map[string]string{"a": "d1", "b": "d2"})

	r.tick("gastown", two, 2)
	if n := count(raisedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold raises at the count = %d, want 1 (%v)", n, raisedKeys(r.rec))
	}
	r.tick("gastown", two, 2)
	if n := count(raisedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold raised %d times inside the window, want 1", n)
	}
	if containsKey(clearedKeys(r.rec), thKey) {
		t.Fatalf("threshold alert cleared while still at the count: %v", clearedKeys(r.rec))
	}

	r.restart()
	r.tick("gastown", blockedReport("gastown", map[string]string{"a": "d1"}), 2) // below
	if n := count(clearedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold clears after restart = %d, want 1 (%v)", n, clearedKeys(r.rec))
	}
	r.tick("gastown", blockedReport("gastown", map[string]string{"a": "d1"}), 2) // still below
	if n := count(clearedKeys(r.rec), thKey); n != 1 {
		t.Fatalf("threshold re-cleared while still below: %d clears", n)
	}
}

func TestReapAlertsUnsetThresholdDefaultsToFive(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}})

	seats := make(map[string]string, 4)
	for _, name := range []string{"a", "b", "c", "d"} {
		seats[name] = "blocked"
	}
	r.tick("gastown", blockedReport("gastown", seats), 0)

	if containsKey(raisedKeys(r.rec), reapThresholdKey("gastown")) {
		t.Fatalf("unset threshold behaved as unlimited: %v", raisedKeys(r.rec))
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
			r := newAlertRig(t, wc)
			r.tickIfEnabled("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}))
			if len(r.rec.raised) != 0 || len(r.rec.cleared) != 0 {
				t.Fatalf("disabled pass alerted: raised=%v cleared=%v", raisedKeys(r.rec), clearedKeys(r.rec))
			}
		})
	}
}

func TestReapAlertsEnabledCoversRigAndUsesConfigThreshold(t *testing.T) {
	t.Parallel()
	r := newAlertRig(t, &agentconfig.WorktreeCleanupConfig{
		Enabled: true, Rigs: []string{"gastown"}, BlockedAlertThreshold: 1,
	})

	r.tickIfEnabled("gastown", blockedReport("gastown", map[string]string{"agate": "verdict NEEDS_RECOVERY"}))

	if !containsKey(raisedKeys(r.rec), reapThresholdKey("gastown")) {
		t.Fatalf("config threshold 1 did not fire: %v", raisedKeys(r.rec))
	}
	if !containsKey(raisedKeys(r.rec), reapBlockedKey("gastown", "agate", "verdict NEEDS_RECOVERY")) {
		t.Fatalf("per-seat alert missing: %v", raisedKeys(r.rec))
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
