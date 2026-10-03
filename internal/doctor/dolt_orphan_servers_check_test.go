package doctor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/util"
)

func TestNewDoltOrphanServersCheck(t *testing.T) {
	t.Parallel()
	check := NewDoltOrphanServersCheck()

	if check.Name() != "dolt-orphan-servers" {
		t.Errorf("expected name 'dolt-orphan-servers', got %q", check.Name())
	}
	if check.Category() != CategoryCleanup {
		t.Errorf("expected category %q, got %q", CategoryCleanup, check.Category())
	}
	if !check.CanFix() {
		t.Error("expected CanFix to return true — orphaned servers and stale temp dirs are auto-fixable")
	}
}

// TestDoltOrphanServersCheck_Run maps what the process and temp-dir scans
// found to a status: nothing is OK, any finding is a fixable warning, and a
// process table that could not be read is skipped, not a clean pass.
func TestDoltOrphanServersCheck_Run(t *testing.T) {
	t.Parallel()
	orphan := util.DoltOrphanServer{PID: 42, PPID: 1, Reason: "orphan", Age: 600, ConfigPath: "/tmp/beads-bd-tests-1/config.yaml"}
	unexpected := util.DoltOrphanServer{PID: 43, PPID: 7, Reason: "unexpected", Age: 60, ConfigPath: "/home/me/config.yaml"}
	tests := []struct {
		name        string
		orphans     []util.DoltOrphanServer
		orphanErr   error
		staleDirs   []string
		wantStatus  CheckStatus
		wantMessage string
	}{
		{name: "clean", wantStatus: StatusOK},
		{name: "orphans and stale dirs", orphans: []util.DoltOrphanServer{orphan, unexpected}, staleDirs: []string{"/tmp/beads-bd-tests-2"},
			wantStatus: StatusWarning, wantMessage: "2 orphaned dolt sql-server process(es) (1 auto-fixable), 1 stale test temp dir(s)"},
		{name: "stale dirs only", staleDirs: []string{"/tmp/beads-bd-tests-2"},
			wantStatus: StatusWarning, wantMessage: "0 orphaned dolt sql-server process(es) (0 auto-fixable), 1 stale test temp dir(s)"},
		{name: "process table unreadable", orphanErr: errors.New("ps: exit status 1"), wantStatus: StatusSkipped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := NewDoltOrphanServersCheck()
			check.findOrphans = func(string) ([]util.DoltOrphanServer, error) { return tt.orphans, tt.orphanErr }
			check.findStaleDirs = func() ([]string, error) { return tt.staleDirs, nil }

			result := check.Run(&CheckContext{TownRoot: t.TempDir()})

			if result.Status != tt.wantStatus {
				t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, tt.wantStatus)
			}
			if tt.wantMessage != "" && result.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", result.Message, tt.wantMessage)
			}
			if (result.Status == StatusWarning) != (result.FixHint != "") {
				t.Errorf("FixHint = %q with status %v; want one exactly when warning", result.FixHint, result.Status)
			}
		})
	}
}

// TestDoltOrphanServersCheck_RunNamesRemedy pins gt-gyw5w: each reported pid
// carries its port, its data-dir, and the command that clears it — kill
// -imposters for a stray on the town's own endpoint, `kill <pid>` for one the
// imposter matcher can never reach.
func TestDoltOrphanServersCheck_RunNamesRemedy(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	townDataDir := filepath.Join(townRoot, ".dolt-data")

	foreign := util.DoltOrphanServer{
		PID: 29490, PPID: 1, Reason: "orphan", Age: 600,
		Port: 3399, DataDir: "/tmp/doltprobe/data",
		ConfigPath: "/tmp/doltprobe/data/config.yaml",
	}
	imposter := util.DoltOrphanServer{
		PID: 500, PPID: 1, Reason: "unexpected", Age: 600,
		Port: 0, DataDir: townDataDir,
	}

	check := NewDoltOrphanServersCheck()
	check.findOrphans = func(string) ([]util.DoltOrphanServer, error) {
		return []util.DoltOrphanServer{foreign, imposter}, nil
	}
	check.findStaleDirs = func() ([]string, error) { return nil, nil }

	result := check.Run(&CheckContext{TownRoot: townRoot})
	joined := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"port 3399",
		"/tmp/doltprobe/data",
		"remedy: kill 29490",
		"remedy: gt dolt kill-imposters",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing %q:\n%s", want, joined)
		}
	}
}

func TestDoltOrphanServersCheck_FixIsSafeWithNoFindings(t *testing.T) {
	t.Parallel()
	check := NewDoltOrphanServersCheck()
	ctx := &CheckContext{TownRoot: t.TempDir()}

	// Fix must not panic or error when Run hasn't cached any findings.
	if err := check.Fix(ctx); err != nil {
		t.Errorf("Fix() with no cached findings returned error: %v", err)
	}
}
