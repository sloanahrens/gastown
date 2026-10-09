package doctor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/slot"
)

// dockerPSLine renders one container the way `docker ps --format {{json .}}`
// does, so the check sees what production sees.
func dockerPSLine(id, image, name string, created time.Time, labels string) string {
	return fmt.Sprintf(`{"ID":%q,"Image":%q,"Names":%q,"CreatedAt":%q,"Labels":%q}`,
		id, image, name, created.Format("2006-01-02 15:04:05 -0700 MST"), labels)
}

// fakeContainers is a docker that lists fixed lines and records removals
// (or fails them with removeErr) instead of reaching the host's docker.
type fakeContainers struct {
	lines     []string
	listErr   error
	removeErr error
	removed   []string
}

func (f *fakeContainers) List() ([]string, error) { return f.lines, f.listErr }

func (f *fakeContainers) Remove(id string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeContainers) Info() (slot.VMInfo, error) { return slot.VMInfo{}, nil }

// debrisCheck is a SlotDebrisCheck whose gate sees docker as rt.
func debrisCheck(rt *fakeContainers) *SlotDebrisCheck {
	c := NewSlotDebrisCheck()
	c.gate = slot.NewGate(slot.WithRuntime(rt))
	return c
}

func TestSlotDebrisCheck_HealthyWhenNothingIsStale(t *testing.T) {
	t.Parallel()
	rt := &fakeContainers{lines: []string{dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite",
		time.Now().Add(-time.Minute), "")}}

	result := debrisCheck(rt).Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a young container: %s", result.Status, result.Message)
	}
}

func TestSlotDebrisCheck_WarnsOnOrphanAndFixReapsIt(t *testing.T) {
	t.Parallel()
	rt := &fakeContainers{lines: []string{dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg",
		time.Now().Add(-5*time.Hour), "")}}
	removed := &rt.removed

	check := debrisCheck(rt)
	ctx := &CheckContext{TownRoot: t.TempDir()}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for an hours-old orphan: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "1 stale gate container") {
		t.Errorf("Message = %q, want it to count the orphan", result.Message)
	}
	if len(*removed) != 0 {
		t.Fatalf("Run removed %v — the check must only inspect", *removed)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "wizardly_goldberg") {
		t.Errorf("Details = %v, want the orphan named", result.Details)
	}

	if !check.CanFix() {
		t.Fatal("CanFix() = false, want the debris check to be fixable")
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(*removed) != 1 || (*removed)[0] != "orphan-id" {
		t.Fatalf("removed = %v, want exactly the orphan's id", *removed)
	}
}

func TestSlotDebrisCheck_UnlistableContainersIsSkippedNotOK(t *testing.T) {
	t.Parallel()
	rt := &fakeContainers{listErr: errors.New("Cannot connect to the Docker daemon")}

	result := debrisCheck(rt).Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped when the containers cannot be listed: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestSlotDebrisCheck_FixReportsRemovalFailures(t *testing.T) {
	t.Parallel()
	rt := &fakeContainers{
		lines: []string{dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg",
			time.Now().Add(-5*time.Hour), "")},
		removeErr: errors.New("Error response from daemon: No such container"),
	}

	err := debrisCheck(rt).Fix(&CheckContext{TownRoot: t.TempDir()})
	if err == nil {
		t.Fatal("Fix returned no error while docker refused the removal")
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("Fix error = %q, want docker's own message", err)
	}
}

// TestSlotDebrisCheck_HeldGateSaysSo pins that a gate held by a live suite
// reads as held, not as "no container older than the window": the reap leaves
// age-only debris alone while a suite runs (gt-c115n), and the clean-pass
// wording would hide that.
func TestSlotDebrisCheck_HeldGateSaysSo(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(slot.LockDir(townRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := lock.FlockTryAcquire(slot.SlotLockPath(townRoot, 0))
	if err != nil || !ok {
		t.Fatalf("holding slot 0: ok=%v err=%v", ok, err)
	}
	defer unlock()

	rt := &fakeContainers{lines: []string{dockerPSLine("ryuk-id", "testcontainers/ryuk:0.9.0", "ryuk-old",
		time.Now().Add(-45*time.Minute), "")}}

	result := debrisCheck(rt).Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK with nothing to remove: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "holds the gate") {
		t.Errorf("Message = %q, want it to name the held gate rather than report a clean window", result.Message)
	}
}
