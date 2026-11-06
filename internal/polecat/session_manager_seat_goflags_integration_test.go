//go:build integration

package polecat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// seatGoFlagsLaunchCounter keeps each seat's worktree and session name apart
// across the subtests and across -count=N runs of this binary.
var seatGoFlagsLaunchCounter atomic.Int64

// TestIntegrationLaunchCarriesTheSeatGoFlagsIntoThePane drives the production
// launch against a real tmux server and reads the GOFLAGS the pane itself
// received. The seat's go_flags is resolved from the town's polecat_pool and
// merged after the agent env's GOFLAGS, and the launch hands the result to the
// session through tmux's -e flags; the unit tier answers that spawn with a fake
// tmux, so the delivery is only exercised here (gt-gg9hv.42, gt-3lk11a).
func TestIntegrationLaunchCarriesTheSeatGoFlagsIntoThePane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("the integration tier needs tmux on PATH: %v", err)
	}

	tests := []struct {
		seat string
		want string
	}{
		// cpus 4 over two seats of cap 1 plans -p=2, and the agent env's own
		// -mod=readonly is the value the seat's flags are appended to.
		{seat: "seat-trimpath", want: "-mod=readonly -trimpath -p=2"},
		{seat: "seat-plain", want: "-mod=readonly -p=2"},
	}
	for _, tt := range tests {
		t.Run(tt.seat, func(t *testing.T) {
			n := seatGoFlagsLaunchCounter.Add(1)
			polecat := fmt.Sprintf("seatflags%d", n)
			town, rigPath, workDir := seatGoFlagsLaunchTown(t, polecat)

			outFile := filepath.Join(town, "pane-goflags.txt")
			// The pane writes the GOFLAGS its shell inherited, shows an agent
			// prompt so the dialog polls do not wait out their timeout, and
			// then becomes a non-shell process: WaitForCommand returns only
			// once the pane stops running a shell.
			command := "sh -c 'printf \"<%s>\" \"$GOFLAGS\" > " + outFile + "; echo \"❯ \"; exec sleep 60'"

			tm := tmux.NewTmux()
			var lines []string
			m := NewSessionManager(tm, &rig.Rig{Name: "gastown", Path: rigPath}, nil)
			m.logf = func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}
			sessionID := m.SessionName(polecat)
			_ = tm.KillSession(sessionID)
			// The pane runs nothing but a bounded sleep, so killing the
			// session is enough; the process-tree walk would only add its
			// two grace periods to the tier's wall.
			t.Cleanup(func() { _ = tm.KillSession(sessionID) })

			err := m.launch(polecat, sessionID, SessionStartOptions{
				WorkDir: workDir,
				Agent:   tt.seat,
				Command: command,
			})
			if err != nil {
				t.Fatalf("launch: %v", err)
			}

			got := readSeatGoFlagsPaneValue(t, outFile)
			if want := "<" + tt.want + ">"; got != want {
				t.Errorf("pane GOFLAGS = %s, want %s", got, want)
			}
			wantLine := "seat " + tt.seat + ": GOFLAGS=" + tt.want
			if len(lines) != 1 || lines[0] != wantLine {
				t.Errorf("spawn lines = %q, want exactly %q", lines, wantLine)
			}
		})
	}
}

// seatGoFlagsLaunchTown builds a town whose pool holds the two seats the test
// table names, a real git worktree for the polecat to start in, and the agent
// preset both seats point at: a sleep long enough for the launch's checks to
// inspect the pane.
func seatGoFlagsLaunchTown(t *testing.T, polecat string) (town, rigPath, workDir string) {
	t.Helper()

	town = t.TempDir()
	rigPath = filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := func() *config.RuntimeConfig {
		return &config.RuntimeConfig{
			Command: "sleep",
			// An empty tmux block keeps the readiness wait out of the pane's
			// way; the value under test is the session environment.
			Tmux: &config.RuntimeTmuxConfig{},
			Env:  map[string]string{"GOFLAGS": "-mod=readonly"},
		}
	}
	cpus := 4
	ts := config.NewTownSettings()
	ts.Agents = map[string]*config.RuntimeConfig{
		"seat-trimpath": agent(),
		"seat-plain":    agent(),
	}
	ts.PolecatPool = &config.PolecatPool{
		CPUs: &cpus,
		Seats: map[string]config.SeatConfig{
			"seat-trimpath": {Cap: 1, GoFlags: "-trimpath"},
			"seat-plain":    {Cap: 1},
		},
	}
	ts.Operational = &config.OperationalConfig{
		Session: &config.SessionThresholds{StartupNudgeVerifyDelay: "200ms"},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
		t.Fatal(err)
	}

	// The worktree sits inside the town, as a real polecat's does; the launch
	// reads the town as the rig path's parent. It is on a branch of its own,
	// so the launch's base-branch repair has nothing to do.
	workDir = filepath.Join(rigPath, "polecats", polecat, "gastown")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "init", "-b", "work")
	seedFile(t, filepath.Join(workDir, "README.md"), "# seat go flags\n")
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "commit", "-m", "initial")
	return town, rigPath, workDir
}

// readSeatGoFlagsPaneValue waits for the pane's shell to write outFile and
// returns its content. The launch returns once the pane has exec'd its long
// sleep, which is after the write, so the wait is normally one read.
func readSeatGoFlagsPaneValue(t *testing.T, outFile string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(outFile); err == nil {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane never wrote %s", outFile)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
