package slot

import (
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// dockerRmTimeout bounds one `docker rm -f`. Generous next to dockerPSTimeout
// because force-removing a container waits for the daemon to finish tearing it
// down, which a loaded VM can stretch.
const dockerRmTimeout = 30 * time.Second

// ReapOptions controls Reap.
type ReapOptions struct {
	// OlderThan is the staleness window. Zero means StaleContainerWindow.
	OlderThan time.Duration
	// DryRun reports what would be removed and removes nothing.
	DryRun bool
}

// StaleOwnerFile is an owner metadata file whose slot nobody holds: the file a
// holder left behind when it died.
type StaleOwnerFile struct {
	Slot       int
	PID        int
	AcquiredAt time.Time
	Path       string
}

// ReapReport is what one Reap call found and what it did about it.
type ReapReport struct {
	OlderThan time.Duration
	DryRun    bool

	// Debris is every container classified as debris, with its evidence.
	Debris []ContainerVerdict
	// Kept is every container left alone, with the reason it was left.
	Kept []ContainerVerdict
	// Removed names the containers actually deleted; empty on a dry run.
	Removed []string
	// Failed is one entry per container whose removal errored.
	Failed []string
	// OwnerFiles is every stale owner file, removed unless DryRun.
	OwnerFiles []StaleOwnerFile
}

// Reap removes the gate's debris: gate containers past the staleness window
// with no live ryuk reaper for their session, and owner metadata files whose
// slot nobody holds.
//
// A non-nil error means the container half could not run — the list came back
// unreadable, so nothing was classified and nothing removed. The owner-file
// half is local and has already run by then, which is why the report comes
// back alongside the error rather than being discarded.
//
// Reaping is the mayor's and the doctor's call. A polecat's slot token is the
// promise that its own suite cleans up after itself; a polecat reaping another
// holder's containers mid-run would break that suite.
func Reap(townRoot string, opts ReapOptions) (ReapReport, error) {
	return NewGate().Reap(townRoot, opts)
}

// Reap is the package-level Reap on this gate.
func (g *Gate) Reap(townRoot string, opts ReapOptions) (ReapReport, error) {
	report := ReapReport{DryRun: opts.DryRun, OlderThan: opts.OlderThan}
	if report.OlderThan <= 0 {
		report.OlderThan = StaleContainerWindow
	}

	report.OwnerFiles = reapStaleOwnerFiles(townRoot, opts.DryRun)

	containers, err := g.gateContainers()
	if err != nil {
		return report, fmt.Errorf("listing gate containers: %w", err)
	}

	for _, verdict := range g.owner.classify(containers, g.clock.Now(), report.OlderThan) {
		if !verdict.Blocks() {
			report.Debris = append(report.Debris, verdict)
		} else {
			report.Kept = append(report.Kept, verdict)
			continue
		}
		if opts.DryRun {
			continue
		}
		if verdict.Container.ID == "" {
			report.Failed = append(report.Failed,
				fmt.Sprintf("%s: no container id in the docker listing, so it cannot be removed", verdict.Container.Display()))
			continue
		}
		if removeErr := g.runtime.Remove(verdict.Container.ID); removeErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", verdict.Container.Display(), removeErr))
			continue
		}
		report.Removed = append(report.Removed, verdict.Container.Display())
	}
	return report, nil
}

// reapStaleOwnerFiles removes the owner file of every slot that is not held.
//
// Holding the flock is what makes an owner file live, so a slot whose flock is
// free has no holder left to describe and its file is decoration (see the
// package doc). That test is strictly stronger than asking whether the
// recorded pid is still running: a live holder always holds the flock, and
// pids come back around.
func reapStaleOwnerFiles(townRoot string, dryRun bool) []StaleOwnerFile {
	var stale []StaleOwnerFile
	for _, i := range discoverSlots(townRoot) {
		path := SlotOwnerPath(townRoot, i)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(townRoot, i))
		if err != nil || !ok {
			// Held, or unreadable: either way the file may belong to a live
			// holder, whose own Release removes it.
			continue
		}
		unlock()

		file := StaleOwnerFile{Slot: i, Path: path}
		if owner := readSlotOwner(townRoot, i); owner != nil {
			file.PID = owner.PID
			file.AcquiredAt = owner.AcquiredAt
		}
		if !dryRun {
			_ = os.Remove(path)
		}
		stale = append(stale, file)
	}
	return stale
}

// removeContainer force-removes one container (see ContainerRuntime.Remove).
// Declared as a var so a test in another package that feeds the gate fake
// debris can never reach the host's real docker: a stub container id has no
// business being deleted for real.
var removeContainer = func(id string) error { return dockerCLI{}.Remove(id) }

// SetContainerRemoverForTest overrides removeContainer, for tests in other
// packages that drive Reap and must not delete a real container. Returns a
// restore func the caller must invoke (typically via t.Cleanup).
func SetContainerRemoverForTest(fn func(id string) error) (restore func()) {
	prev := removeContainer
	removeContainer = fn
	return func() { removeContainer = prev }
}
