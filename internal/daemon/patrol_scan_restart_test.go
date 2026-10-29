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
