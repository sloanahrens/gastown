package doctor

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// parkSession parks the seat behind a session name in town.
func parkSession(t *testing.T, town, sess string) {
	t.Helper()
	seat, err := supervisor.SeatForSession(sess)
	if err != nil {
		t.Fatalf("SeatForSession(%q): %v", sess, err)
	}
	if _, err := intent.Update(town, supervisor.IntentSeat(seat), func(r *intent.Record) error {
		r.Desired = intent.DesiredPark
		r.Reason = "operator hold"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// gt doctor --fix kills zombie seats through the supervisor: a parked seat
// is refused and reported, and a free one is killed and logged.
func TestZombieSessionCheck_FixHonorsAParkedSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lister := &fakeZombieLister{alive: map[string]bool{}}
	check := NewZombieSessionCheckWithLister(lister)
	check.zombieSessions = []string{"hq-dog-alpha", "hq-mayor"}
	parkSession(t, town, "hq-dog-alpha")

	err := check.Fix(&CheckContext{TownRoot: town})

	if len(lister.killed) != 1 || lister.killed[0] != "hq-mayor" {
		t.Fatalf("killed = %v, want only the unparked hq-mayor", lister.killed)
	}
	if err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("Fix error = %v, want the parked-seat refusal reported", err)
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(town))
	if !strings.Contains(string(lines), `"actor":"gt doctor --fix"`) {
		t.Fatalf("doctor kills missing from the action log: %s", lines)
	}
}

// fakeOrphanTmux lists and kills orphan sessions.
type fakeOrphanTmux struct {
	sessions []string
	killed   []string
}

func (f *fakeOrphanTmux) ListSessions() ([]string, error) { return f.sessions, nil }
func (f *fakeOrphanTmux) KillSessionWithProcesses(name string) error {
	f.killed = append(f.killed, name)
	return nil
}

// Orphan sessions belong to no seat: they go through KillStray, which a town
// e-stop refuses.
func TestOrphanSessionCheck_FixHonorsTheTownEstop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := &fakeOrphanTmux{}
	check := NewOrphanSessionCheckWithSessionLister(fake)
	check.orphanSessions = []string{"zz-ghost"}
	_ = estop.Activate(town, estop.TriggerManual, "drill")

	if err := check.Fix(&CheckContext{TownRoot: town}); err == nil {
		t.Fatal("Fix under e-stop reported success")
	}
	if len(fake.killed) != 0 {
		t.Fatalf("killed %v under a town e-stop", fake.killed)
	}
}

// A name that does not parse to a seat is refused, never killed as a stray:
// it may be a parked seat under a prefix the registry does not know.
func TestKillSessionForFix_RefusesAnUnparsableName(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{}
	if err := killSessionForFix(t.TempDir(), lister, "not-a-town-session-", "zombie cleanup"); err == nil {
		t.Fatal("an unparsable session name was not refused")
	}
	if len(lister.killed) != 0 {
		t.Fatalf("killed %v", lister.killed)
	}
}
