package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/templates"
)

// daemonRespawnBaseline is the runtime baseline for the daemon-respawn check,
// persisted at <town>/.runtime/daemon-respawn.json: the spawn count the
// supervisor reported when this check last ran. A count that rose since then
// is what makes a job with no process a respawn loop rather than one spawn
// that has not come back yet.
type daemonRespawnBaseline struct {
	Runs       int       `json:"runs"`
	RecordedAt time.Time `json:"recorded_at"`
}

// DaemonRespawnCheck reports a supervisor that is failing every spawn of the
// daemon, so the town has no daemon, no dispatch and no landing while nothing
// on disk says why. DaemonCheck reads the lock and DaemonLivenessCheck the
// heartbeat, and a job stuck before the program runs leaves neither: the
// daemon only reaches either once it has initialized (gt-92qx3).
type DaemonRespawnCheck struct {
	BaseCheck

	// probe reads the host's supervisor job and whether it is respawning
	// without ever starting; nil is hostDaemonRespawn, which is launchctl
	// print and nothing else, read-only.
	probe func(townRoot string) (templates.SupervisorState, bool)
	// containerReading is the container-town reading; nil is
	// readContainerDaemon.
	containerReading func(townRoot string) containerDaemonReading
}

// hostDaemonRespawn asks launchctl what it is doing with the daemon's job.
func hostDaemonRespawn(townRoot string) (templates.SupervisorState, bool) {
	return templates.SupervisorRespawn(townRoot, templates.SupervisorJobState)
}

// daemonRespawn is the check's probe: the host's in production, the test's
// canned reading otherwise.
func (c *DaemonRespawnCheck) daemonRespawn(townRoot string) (templates.SupervisorState, bool) {
	if c.probe != nil {
		return c.probe(townRoot)
	}
	return hostDaemonRespawn(townRoot)
}

// NewDaemonRespawnCheck creates a new daemon-respawn check.
func NewDaemonRespawnCheck() *DaemonRespawnCheck {
	return &DaemonRespawnCheck{
		BaseCheck: BaseCheck{
			CheckName:        "daemon-respawn",
			CheckDescription: "Verify the supervisor is not failing every spawn of the daemon",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

func daemonRespawnStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "daemon-respawn.json")
}

// Run reads the supervisor's live job state, read-only. A loop is an error
// whether or not a daemon is up: it is a supervisor that will not bring the
// daemon back, and the repair is the operator's to run — nothing in gt
// bootouts or bootstraps a job.
func (c *DaemonRespawnCheck) Run(ctx *CheckContext) *CheckResult {
	// launchd's spawn accounting says nothing about a container town's daemon,
	// whose restart policy belongs to the container runtime; the container and
	// the daemon's own state are the report (gt-qpeei.20).
	if v := judgeContainerDaemon(c.Name(), ctx.TownRoot, c.containerReading); v.isContainer {
		if v.result != nil {
			return v.result
		}
		return v.reading.okResult(c.Name())
	}

	st, looping := c.daemonRespawn(ctx.TownRoot)

	if st.Err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the supervisor's job state",
			Details: []string{st.Err.Error()},
		}
	}
	if st.Kind != "launchd" {
		// systemd keeps its own restart accounting and no launchd respawn
		// loop exists to find.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "no launchd job serves this town",
		}
	}

	path := daemonRespawnStatePath(ctx.TownRoot)
	base, hadBase := readDaemonRespawnBaseline(path)
	var details []string
	if err := writeDaemonRespawnBaseline(path, daemonRespawnBaseline{Runs: st.Runs, RecordedAt: time.Now()}); err != nil {
		details = append(details, "baseline not recorded, so the next run cannot report a rise: "+err.Error())
	}

	if !looping {
		msg, status := respawnStanding(st)
		return &CheckResult{Name: c.Name(), Status: status, Message: msg, Details: details}
	}

	rise := ""
	if hadBase && st.Runs > base.Runs {
		rise = fmt.Sprintf(", runs rose from %d", base.Runs)
	}
	details = append([]string{"Supervised: " + st.StatusLine(0)}, details...)
	return &CheckResult{
		Name:   c.Name(),
		Status: StatusError,
		Message: fmt.Sprintf("launchd is failing every spawn of the daemon: no pid, last exit code %d, runs %d%s",
			st.LastExit, st.Runs, rise),
		Details: details,
		FixHint: "Run: " + templates.SupervisorRepair(st.Kind) +
			" — then confirm 'launchctl print' reports a pid and spawn type daemon (3)",
	}
}

// respawnStanding names what the supervisor is doing when it is not a respawn
// loop. A job holding no process is a warning rather than a pass: launchd is
// not running the daemon either, which the other daemon checks judge but this
// one must not report as healthy.
func respawnStanding(st templates.SupervisorState) (string, CheckStatus) {
	switch {
	case !st.Loaded:
		return "launchd holds no loaded job for this town", StatusOK
	case st.PID != 0:
		return fmt.Sprintf("launchd is running the daemon (pid %d, runs %d)", st.PID, st.Runs), StatusOK
	default:
		return fmt.Sprintf("launchd holds the job with no process (runs %d, last exit code %d)", st.Runs, st.LastExit), StatusWarning
	}
}

// readDaemonRespawnBaseline reads the baseline state file. The second return
// value is false when no usable baseline exists yet (missing or unparseable
// file, or a zero RecordedAt).
func readDaemonRespawnBaseline(path string) (daemonRespawnBaseline, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonRespawnBaseline{}, false
	}
	var baseline daemonRespawnBaseline
	if err := json.Unmarshal(data, &baseline); err != nil || baseline.RecordedAt.IsZero() {
		return daemonRespawnBaseline{}, false
	}
	return baseline, true
}

// writeDaemonRespawnBaseline persists the baseline state file atomically,
// creating its .runtime parent directory if needed.
func writeDaemonRespawnBaseline(path string, baseline daemonRespawnBaseline) error {
	return atomicfile.EnsureDirAndWriteJSON(path, baseline)
}
