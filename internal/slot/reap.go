package slot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lock"
)

// dockerRmTimeout bounds one `docker rm -f -v`. Generous next to dockerPSTimeout
// because force-removing a container waits for the daemon to finish tearing it
// down, which a loaded VM can stretch.
const dockerRmTimeout = 30 * time.Second

// ReapOptions controls Reap.
type ReapOptions struct {
	// OlderThan is the staleness window. Zero means StaleContainerWindow.
	OlderThan time.Duration
	// DryRun reports what would be removed and removes nothing.
	DryRun bool
	// Protected names containers no reap may remove, matched exactly against
	// docker's container name. A nil or empty list protects nothing.
	Protected []string
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

	// Debris is every container Reap classifies as debris and would remove
	// (or did remove, outside a dry run), with its evidence.
	Debris []ContainerVerdict
	// Kept is every container left alone, with the reason it was left: a live
	// suite, an unknowable age, or age-only debris with a suite holding the
	// gate (HeldSlots).
	Kept []ContainerVerdict
	// Removed names the containers actually deleted; empty on a dry run.
	Removed []string
	// Failed is one entry per container whose removal errored.
	Failed []string
	// OwnerFiles is every stale owner file, removed unless DryRun.
	OwnerFiles []StaleOwnerFile
	// HeldSlots are the slots a live holder held when Reap ran, empty when
	// every slot was free. While one is held, age-only debris is Kept instead
	// of removed (gt-c115n).
	HeldSlots []int
}

// Reap removes the gate's debris: gate containers past the staleness window
// with no live ryuk reaper for their session, and owner metadata files whose
// slot nobody holds. A container named in ReapOptions.Protected, the one
// mayor/town.json dolt.external_container names, or carrying KeepLabel is left
// out of the report as well as the removal, because it is the operator's to
// keep whatever its age (gt-awq73, gt-1j7zv).
//
// Protection of the town's Dolt container lives here rather than in each
// caller, so no caller can forget it. A town.json that exists but cannot be
// read or parsed makes Reap return an error before anything is removed.
//
// While any slot is held, age-only debris is left in Kept rather than removed
// (gt-c115n): past the staleness window this side of the gate cannot tell a
// leak from a suite that is merely slow, and removing a container out from
// under a live suite breaks it. Owner-gone debris is removed either way — the
// owner process is provably dead, so nothing can be using the container.
//
// A removed container's anonymous volumes go with it only when its owner
// labels proved it is this gate's debris (gt-lsxps, see RemoveOptions).
//
// A non-nil error means the container half could not run — the list came back
// unreadable, so nothing was classified and nothing removed. The owner-file
// half is local and has already run by then, which is why the report comes
// back alongside the error rather than being discarded.
//
// Reaping is the operator's and the doctor's call. A polecat's slot token is the
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

	// The town's Dolt container is read before anything is removed, owner
	// files included: an unreadable town.json refuses the whole reap.
	townDolt, err := townDoltContainer(townRoot)
	if err != nil {
		return report, fmt.Errorf("reading the town's dolt.external_container: %w", err)
	}
	protected := opts.Protected
	if townDolt != "" {
		protected = append(slices.Clip(protected), townDolt)
	}

	report.OwnerFiles = reapStaleOwnerFiles(townRoot, opts.DryRun)
	report.HeldSlots = heldSlots(townRoot)

	containers, err := g.gateContainers()
	if err != nil {
		return report, fmt.Errorf("listing gate containers: %w", err)
	}
	containers = exemptFromReap(containers, protected)

	for _, verdict := range g.scratchVerdicts(g.owner.classify(containers, g.clock.Now(), report.OlderThan)) {
		// The scratch project's containers are the documented L1/L2 town's,
		// which no reap removes: removing one pulls the Dolt server out from
		// under a window that is still running, and a container the project is
		// restarting is the moment a reap would do the most damage (gt-4gixm).
		if verdict.Verdict == VerdictScratch || verdict.Verdict == VerdictScratchBuild {
			report.Kept = append(report.Kept, verdict)
			continue
		}
		if verdict.Blocks() {
			report.Kept = append(report.Kept, verdict)
			continue
		}
		if len(report.HeldSlots) > 0 && !verdict.OwnerGone {
			verdict.Reason += "; left alone while a suite holds the gate"
			report.Kept = append(report.Kept, verdict)
			continue
		}
		report.Debris = append(report.Debris, verdict)
		if opts.DryRun {
			continue
		}
		if verdict.Container.ID == "" {
			report.Failed = append(report.Failed,
				fmt.Sprintf("%s: no container id in the docker listing, so it cannot be removed", verdict.Container.Display()))
			continue
		}
		// Only OwnerGone is proved debris, so only OwnerGone takes the
		// container's volume with it (see RemoveOptions, gt-lsxps).
		if removeErr := g.runtime.Remove(verdict.Container.ID, RemoveOptions{Volumes: verdict.OwnerGone}); removeErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", verdict.Container.Display(), removeErr))
			continue
		}
		report.Removed = append(report.Removed, verdict.Container.Display())
	}
	return report, nil
}

// exemptFromReap drops the containers a reap must never touch: one whose name
// the caller listed in Protected, and one carrying KeepLabel. They leave
// before classification, so a reap holds no verdict on them at all: a
// protected container is in neither Debris nor Kept, and Remove never sees it.
func exemptFromReap(containers []GateContainer, protected []string) []GateContainer {
	names := make(map[string]struct{}, len(protected))
	for _, name := range protected {
		names[name] = struct{}{}
	}
	kept := containers[:0]
	for _, c := range containers {
		if _, ok := names[c.Name]; ok || c.HasKeepLabel() {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// townDoltContainer returns the Docker container the town's Dolt server runs in
// (mayor/town.json dolt.external_container), "" when the town names none. A
// missing town.json is a host-managed town and names nothing. Any other read or
// parse failure is an error, and so is a set name that is not a Docker
// container name: reading either as "no container" would leave the town's
// server open to a reap.
//
// It reads the file with config.LoadTownConfig rather than doltserver's
// DefaultConfig, which drops the read error and falls back to the managed
// config, so a malformed town.json would read as no container (gt-8ibrs).
func townDoltContainer(townRoot string) (string, error) {
	town, err := config.LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json"))
	if errors.Is(err, config.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return town.Dolt.ParseExternalContainer()
}

// reapStaleOwnerFiles removes the owner file of every slot that is not held.
//
// Holding the flock is what makes an owner file live, so a slot whose flock is
// free has no holder left to describe and its file is decoration (see the
// package doc). That test is strictly stronger than asking whether the
// recorded pid is still running: a live holder always holds the flock, and
// pids come back around.
//
// The read and the removal both happen while the flock is held. Releasing it
// first opens a window in which the next acquirer takes the slot and writes its
// own owner file — the file this would then delete, leaving a live holder with
// no owner file for the gate and the full-suite cap to read (gt-u0zq0).
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

		file := StaleOwnerFile{Slot: i, Path: path}
		if owner := readSlotOwner(townRoot, i); owner != nil {
			file.PID = owner.PID
			file.AcquiredAt = owner.AcquiredAt
		}
		if !dryRun {
			_ = os.Remove(path)
		}
		unlock()
		stale = append(stale, file)
	}
	return stale
}

// heldSlots returns every slot a live holder holds in townRoot, by the same
// non-blocking flock probe reapStaleOwnerFiles uses: the kernel releases an
// flock when its owner dies, so a held flock is a live holder.
//
// A flock that cannot be probed counts as held. Reading an unreadable lock as
// free would license removing a live suite's containers; reading it as held
// only postpones removal to the next reap.
func heldSlots(townRoot string) []int {
	var held []int
	for _, i := range discoverSlots(townRoot) {
		unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(townRoot, i))
		if err != nil || !ok {
			held = append(held, i)
			continue
		}
		unlock()
	}
	return held
}
