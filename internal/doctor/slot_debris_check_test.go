package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// dockerPSLine renders one container the way `docker ps --format {{json .}}`
// does, so the check sees what production sees.
func dockerPSLine(id, image, name string, created time.Time, labels string) string {
	return fmt.Sprintf(`{"ID":%q,"Image":%q,"Names":%q,"CreatedAt":%q,"Labels":%q}`,
		id, image, name, created.Format("2006-01-02 15:04:05 -0700 MST"), labels)
}

// stubSlotContainers points the gate's docker listing at fixed lines and
// records removals instead of reaching the host's docker.
func stubSlotContainers(t *testing.T, lines ...string) *[]string {
	t.Helper()
	restoreLister := slot.SetContainerListerForTest(func() ([]string, error) { return lines, nil })
	t.Cleanup(restoreLister)

	var removed []string
	restoreRemover := slot.SetContainerRemoverForTest(func(id string) error {
		removed = append(removed, id)
		return nil
	})
	t.Cleanup(restoreRemover)
	return &removed
}

func TestSlotDebrisCheck_HealthyWhenNothingIsStale(t *testing.T) {
	stubSlotContainers(t, dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite",
		time.Now().Add(-time.Minute), ""))

	result := NewSlotDebrisCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a young container: %s", result.Status, result.Message)
	}
}

func TestSlotDebrisCheck_WarnsOnOrphanAndFixReapsIt(t *testing.T) {
	removed := stubSlotContainers(t, dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg",
		time.Now().Add(-5*time.Hour), ""))

	check := NewSlotDebrisCheck()
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
	restore := slot.SetContainerListerForTest(func() ([]string, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	})
	t.Cleanup(restore)

	result := NewSlotDebrisCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped when the containers cannot be listed: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestSlotDebrisCheck_FixReportsRemovalFailures(t *testing.T) {
	restoreLister := slot.SetContainerListerForTest(func() ([]string, error) {
		return []string{dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg",
			time.Now().Add(-5*time.Hour), "")}, nil
	})
	t.Cleanup(restoreLister)
	restoreRemover := slot.SetContainerRemoverForTest(func(string) error {
		return errors.New("Error response from daemon: No such container")
	})
	t.Cleanup(restoreRemover)

	err := NewSlotDebrisCheck().Fix(&CheckContext{TownRoot: t.TempDir()})
	if err == nil {
		t.Fatal("Fix returned no error while docker refused the removal")
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("Fix error = %q, want docker's own message", err)
	}
}
