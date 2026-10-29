package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// restartFailureOutput is what a failed `gt session restart` writes: the
// child's own error line, then the usage block cobra prints after it. The
// block's last line is a flag description, and a daemon that kept only the
// last line logged that instead of the cause (gt-6hhp8).
const restartFailureOutput = `Starting session for gastown/goose...
Error: starting session: pushing polecat branch: refused: non-fast-forward
Usage:
  gt session restart <rig>/<polecat> [flags]

Flags:
  -f, --force                  Force immediate shutdown
      --requested-by string   Caller performing an automated restart (e.g. "daemon/patrol-scan"); recorded in the town log wake line
`

// A failed restart's error carries the child's own reason, not the tail of
// the usage block that follows it in the child's output (gt-6hhp8).
func TestPatrolScanRestartReportsTheChildsErrorLine(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(cliBySub(map[string]cliReply{
		"session restart": {code: 1, stderr: restartFailureOutput},
	}))
	d := &Daemon{
		logger:  discardLogger,
		config:  &Config{TownRoot: t.TempDir()},
		gtPath:  "gt",
		execCmd: gt.run,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true, Rigs: []string{"gastown"}},
		}},
	}

	err := d.restartPolecatSession(supervisor.SeatFor("gastown", constants.RolePolecat, "goose"))
	if err == nil {
		t.Fatal("restart of a gt that exited non-zero = success, want its error")
	}
	msg := err.Error()
	const want = "gt session restart gastown/goose: exit status 1: starting session: pushing polecat branch: refused: non-fast-forward"
	if !strings.HasSuffix(msg, want) {
		t.Errorf("restart error = %q, want it to end %q", msg, want)
	}
	for _, usage := range []string{"Usage:", "--requested-by", "Force immediate shutdown"} {
		if strings.Contains(msg, usage) {
			t.Errorf("restart error carries usage text %q: %q", usage, msg)
		}
	}
}

// A seat whose restart fails with the same cause on two consecutive patrol
// scans raises one escalation naming the seat and the cause, keeps it to one
// however many scans follow, and clears it when a restart succeeds (gt-6hhp8).
func TestPatrolScanRestartEscalatesASeatThatKeepsFailing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := notifyfake.New()

	failure := errors.New("restarting gastown/goose: gt session restart gastown/goose: exit status 1: " +
		"starting session: pushing polecat branch: refused: non-fast-forward")
	var stillFailing error = failure
	d := &Daemon{
		logger:           discardLogger,
		config:           &Config{TownRoot: town},
		notifier:         rec,
		prefixRegistryFn: rigPrefixes("gastown"),
	}
	d.supervisor = supervisor.New(supervisor.Options{
		TownRoot: town,
		Budget:   10,
		Prefixes: rigPrefixes("gastown")(),
		Restart:  func(supervisor.Seat) error { return stillFailing },
		Logf:     func(string, ...any) {},
	})
	h := &patrolScanHost{d: d}
	const key = "patrol-restart:gastown/goose"

	// One failure is not a streak: the seat may simply recover.
	if err := h.Restart("gastown", "goose", "patrol scan: dead"); err == nil {
		t.Fatal("restart answered success, want the executor's failure")
	}
	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("escalations after one failed restart = %+v, want none", got)
	}

	// The same cause on the next scan is escalated, once.
	if err := h.Restart("gastown", "goose", "patrol scan: dead"); err == nil {
		t.Fatal("second restart answered success, want the executor's failure")
	}
	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one", got)
	}
	if fp := got[0].Escalation.Fingerprint; fp != key {
		t.Errorf("escalation fingerprint = %q, want %q", fp, key)
	}
	if reason := got[0].Escalation.Reason; !strings.Contains(reason, "gastown/goose") ||
		!strings.Contains(reason, "refused: non-fast-forward") {
		t.Errorf("escalation reason = %q, want the seat and the cause named", reason)
	}

	// A third consecutive failure reports the same condition: one escalation,
	// not a new one per scan.
	if err := h.Restart("gastown", "goose", "patrol scan: dead"); err == nil {
		t.Fatal("third restart answered success, want the executor's failure")
	}
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations after three failed restarts = %d, want 1", n)
	}

	// A restart that succeeds clears it.
	stillFailing = nil
	if err := h.Restart("gastown", "goose", "patrol scan: dead"); err != nil {
		t.Fatalf("restart answered %v, want success", err)
	}
	cleared := slices.ContainsFunc(rec.Clears(), func(c notifyfake.Call) bool {
		return slices.Contains(c.Fingerprints, key)
	})
	if !cleared {
		t.Errorf("clears = %+v, want one naming %q", rec.Clears(), key)
	}
}

// A failure whose cause changes starts a new streak: the second scan with the
// new cause is what escalates, and the cause it names is the new one.
func TestPatrolScanRestartEscalatesOnlyARepeatedCause(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := notifyfake.New()

	first := errors.New("restarting gastown/goose: gt session restart gastown/goose: exit status 1: push refused")
	second := errors.New("restarting gastown/goose: gt session restart gastown/goose: exit status 1: tmux server gone")
	var stillFailing error
	d := &Daemon{
		logger:           discardLogger,
		config:           &Config{TownRoot: town},
		notifier:         rec,
		prefixRegistryFn: rigPrefixes("gastown"),
	}
	d.supervisor = supervisor.New(supervisor.Options{
		TownRoot: town,
		Budget:   10,
		Prefixes: rigPrefixes("gastown")(),
		Restart:  func(supervisor.Seat) error { return stillFailing },
		Logf:     func(string, ...any) {},
	})
	h := &patrolScanHost{d: d}

	stillFailing = first
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	stillFailing = second
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("two different causes escalated after one scan each: %+v", got)
	}
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one for the repeated cause", got)
	}
	if reason := got[0].Escalation.Reason; !strings.Contains(reason, "tmux server gone") {
		t.Errorf("escalation reason = %q, want the repeated cause", reason)
	}
}

// newPatrolRestartDaemon builds a daemon whose supervisor runs restart in
// place of the real executor, with the restart budget set by the test.
func newPatrolRestartDaemon(t *testing.T, budget int, restart func(supervisor.Seat) error) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	return newPatrolRestartDaemonOn(t, rec, budget, restart), rec
}

// newPatrolRestartDaemonOn is newPatrolRestartDaemon delivering into an
// existing recorder: a test that restarts the daemon runs the new process
// against the same town, where the escalations it inherits were filed.
func newPatrolRestartDaemonOn(t *testing.T, rec *notifyfake.Recorder, budget int, restart func(supervisor.Seat) error) *Daemon {
	t.Helper()
	town := t.TempDir()
	d := &Daemon{
		logger:           discardLogger,
		config:           &Config{TownRoot: town},
		notifier:         rec,
		prefixRegistryFn: rigPrefixes("gastown"),
	}
	d.supervisor = supervisor.New(supervisor.Options{
		TownRoot: town,
		Budget:   budget,
		Prefixes: rigPrefixes("gastown")(),
		Restart:  restart,
		Logf:     func(string, ...any) {},
	})
	return d
}

// patrolRestartEscalations returns the escalations filed under a seat's
// patrol-restart key, so a supervisor's own escalations do not count.
func patrolRestartEscalations(rec *notifyfake.Recorder, key string) []notifyfake.Call {
	var got []notifyfake.Call
	for _, c := range rec.Escalations() {
		if c.Escalation.Fingerprint == key {
			got = append(got, c)
		}
	}
	return got
}

// A restart whose child printed no "Error:" line still logs the reason the
// child gave: the last line before its usage block, or its last line when no
// usage block follows (gt-6hhp8).
func TestPatrolScanRestartFallsBackToTheChildsOwnLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "line before the usage block",
			stderr: "Starting session for gastown/goose...\nUsage:\n  gt session restart <rig>/<polecat> [flags]\n\nFlags:\n  -f, --force   Force immediate shutdown\n",
			want:   "Starting session for gastown/goose...",
		},
		{
			name:   "no usage block: the last line",
			stderr: "starting tmux\npush refused by the remote\n",
			want:   "push refused by the remote",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gt := newFakeCLI(cliBySub(map[string]cliReply{
				"session restart": {code: 1, stderr: tc.stderr},
			}))
			d := &Daemon{
				logger:  discardLogger,
				config:  &Config{TownRoot: t.TempDir()},
				gtPath:  "gt",
				execCmd: gt.run,
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					PatrolScan: &PatrolScanConfig{Enabled: true, Rigs: []string{"gastown"}},
				}},
			}
			err := d.restartPolecatSession(supervisor.SeatFor("gastown", constants.RolePolecat, "goose"))
			if err == nil {
				t.Fatal("restart of a gt that exited non-zero = success, want its error")
			}
			if want := "exit status 1: " + tc.want; !strings.HasSuffix(err.Error(), want) {
				t.Errorf("restart error = %q, want it to end %q", err.Error(), want)
			}
		})
	}
}

// A refusal is not a failed attempt: it neither escalates the seat nor
// counts toward a run, and the supervisor's own refusal escalates under its
// own key (gt-6hhp8).
func TestPatrolScanRestartRefusalDoesNotEscalate(t *testing.T) {
	t.Parallel()
	failure := errors.New("restarting gastown/goose: exit status 1: push refused")
	// Budget 1: the first failed attempt spends it, so the next restart is
	// refused by the supervisor.
	d, rec := newPatrolRestartDaemon(t, 1, func(supervisor.Seat) error { return failure })
	h := &patrolScanHost{d: d}
	const key = "patrol-restart:gastown/goose"

	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	err := h.Restart("gastown", "goose", "patrol scan: dead")
	if !errors.Is(err, supervisor.ErrRefused) {
		t.Fatalf("second restart = %v, want the supervisor's refusal", err)
	}
	if got := patrolRestartEscalations(rec, key); len(got) != 0 {
		t.Errorf("refusal escalated the seat under %q: %+v", key, got)
	}
}

// A run counts consecutive scans, not attempts: a seat that fails once, is
// not restarted on a later scan, and fails once more with the same cause is
// not escalated, while the same cause on two consecutive scans is (gt-6hhp8).
func TestPatrolScanRestartQuietScanBreaksTheRun(t *testing.T) {
	t.Parallel()
	failure := errors.New("restarting gastown/goose: exit status 1: push refused")
	const key = "patrol-restart:gastown/goose"

	t.Run("a scan with no restart of the seat", func(t *testing.T) {
		t.Parallel()
		d, rec := newPatrolRestartDaemon(t, 10, func(supervisor.Seat) error { return failure })
		h := &patrolScanHost{d: d}
		_ = h.Restart("gastown", "goose", "patrol scan: dead")
		d.endPatrolRestartScan("gastown")
		d.endPatrolRestartScan("gastown") // the seat recovered: not restarted
		_ = h.Restart("gastown", "goose", "patrol scan: dead")
		d.endPatrolRestartScan("gastown")
		if got := patrolRestartEscalations(rec, key); len(got) != 0 {
			t.Fatalf("failures on scans 1 and 3 escalated: %+v", got)
		}
		_ = h.Restart("gastown", "goose", "patrol scan: dead")
		if got := patrolRestartEscalations(rec, key); len(got) != 1 {
			t.Errorf("failures on consecutive scans 3 and 4 escalations = %d, want 1", len(got))
		}
	})

	t.Run("another rig's scan leaves the run", func(t *testing.T) {
		t.Parallel()
		d, rec := newPatrolRestartDaemon(t, 10, func(supervisor.Seat) error { return failure })
		h := &patrolScanHost{d: d}
		_ = h.Restart("gastown", "goose", "patrol scan: dead")
		d.endPatrolRestartScan("gastown")
		d.endPatrolRestartScan("otherrig") // a scan of a rig that does not hold the seat
		_ = h.Restart("gastown", "goose", "patrol scan: dead")
		if got := patrolRestartEscalations(rec, key); len(got) != 1 {
			t.Errorf("escalations = %d, want 1: a scan of another rig broke the run", len(got))
		}
	})
}

// A patrol-restart alert is filed in the town, not in the daemon, so the
// daemon that restarts the seat may not be the one that raised it: the alert
// raised before a daemon restart must still be cleared by the successful
// restart that follows (gt-eu4ij).
func TestPatrolScanRestartClearsAnAlertRaisedBeforeADaemonRestart(t *testing.T) {
	t.Parallel()
	failure := errors.New("restarting gastown/goose: exit status 1: push refused")
	const key = "patrol-restart:gastown/goose"

	rec := notifyfake.New()
	before := newPatrolRestartDaemonOn(t, rec, 10, func(supervisor.Seat) error { return failure })
	h := &patrolScanHost{d: before}
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	if got := rec.OpenEscalations(key); len(got) != 1 {
		t.Fatalf("open escalations under %q before the daemon restart = %+v, want one", key, got)
	}

	// The daemon restarts, and the new process holds no memory of the alert
	// its predecessor raised. Its restart succeeds.
	after := newPatrolRestartDaemonOn(t, rec, 10, func(supervisor.Seat) error { return nil })
	if err := (&patrolScanHost{d: after}).Restart("gastown", "goose", "patrol scan: dead"); err != nil {
		t.Fatalf("restart answered %v, want success", err)
	}
	if got := rec.OpenEscalations(key); len(got) != 0 {
		t.Errorf("open escalations under %q after the new daemon's successful restart = %+v, want none", key, got)
	}
}

// A successful restart with no alert open under the seat's key clears that key
// and nothing else, files no escalation and reports no error, so every
// successful restart can clear without first asking whether an alert exists
// (gt-eu4ij).
func TestPatrolScanRestartSuccessWithNoAlertOpenClearsOnlyItsKey(t *testing.T) {
	t.Parallel()
	const key = "patrol-restart:gastown/goose"
	d, rec := newPatrolRestartDaemon(t, 10, func(supervisor.Seat) error { return nil })

	if err := (&patrolScanHost{d: d}).Restart("gastown", "goose", "patrol scan: dead"); err != nil {
		t.Fatalf("restart answered %v, want success", err)
	}
	if got := rec.Escalations(); len(got) != 0 {
		t.Errorf("escalations = %+v, want none", got)
	}
	if got := rec.OpenEscalations(key); len(got) != 0 {
		t.Errorf("open escalations under %q = %+v, want none", key, got)
	}
	clears := rec.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %+v, want one naming %q", clears, key)
	}
	if fp := clears[0].Fingerprints; len(fp) != 1 || fp[0] != key {
		t.Errorf("clear named %v, want only %q", fp, key)
	}
}

// An escalation stays open through a quiet scan, and the restart that
// succeeds afterwards clears it (gt-6hhp8).
func TestPatrolScanRestartClearsAnAlertAfterAQuietScan(t *testing.T) {
	t.Parallel()
	failure := errors.New("restarting gastown/goose: exit status 1: push refused")
	var stillFailing error = failure
	d, rec := newPatrolRestartDaemon(t, 10, func(supervisor.Seat) error { return stillFailing })
	h := &patrolScanHost{d: d}
	const key = "patrol-restart:gastown/goose"

	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	d.endPatrolRestartScan("gastown")
	_ = h.Restart("gastown", "goose", "patrol scan: dead")
	if got := patrolRestartEscalations(rec, key); len(got) != 1 {
		t.Fatalf("escalations = %d, want 1", len(got))
	}
	d.endPatrolRestartScan("gastown")
	d.endPatrolRestartScan("gastown") // quiet: the run is broken, the alert is not

	stillFailing = nil
	if err := h.Restart("gastown", "goose", "patrol scan: dead"); err != nil {
		t.Fatalf("restart answered %v, want success", err)
	}
	cleared := slices.ContainsFunc(rec.Clears(), func(c notifyfake.Call) bool {
		return slices.Contains(c.Fingerprints, key)
	})
	if !cleared {
		t.Errorf("clears = %+v, want one naming %q", rec.Clears(), key)
	}
}
