package daemon

// The sweep-runs prune is the disk maintenance patrol's target for the tier
// sweep's output. The sweep writes one directory per run under
// .runtime/sweep-runs/<rig> of the town root. The patrol plans those directories
// with planSweepRunsPrune on sweep_runs_prune_interval, in any window and off the
// quiet guard, and in apply mode removes what the plan lists. Dry run is the
// patrol's default: it logs what an apply would remove and removes nothing.
//
// Only a real directory that is a direct child of a rig directory is ever
// planned or removed. A file, a symlink, or a rig entry that is not a directory
// is never named, and each listed directory is checked again on disk before it is
// removed. A listing or size failure refuses the whole run before anything is
// removed; a check or remove that fails during the apply stops it there.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

const (
	// sweepRunsPruneName is the schedule key the sweep-runs prune's last run is
	// recorded under. It is not a target name, so it cannot collide with one.
	sweepRunsPruneName = "sweep_runs_prune"

	// sweepRunsRootRel is the sweep's output root, relative to the town root.
	sweepRunsRootRel = ".runtime/sweep-runs"
)

// sweepRunsFileSystem is the filesystem the prune lists and removes through. A
// test replaces it through diskMaintSeams.sweepRunsFS.
type sweepRunsFileSystem interface {
	// ReadDir lists dir's direct children, sorted by name.
	ReadDir(dir string) ([]fs.DirEntry, error)
	// Lstat describes path without following a symlink.
	Lstat(path string) (fs.FileInfo, error)
	// RemoveAll removes path and everything under it.
	RemoveAll(path string) error
}

// osSweepRunsFS is the production filesystem.
type osSweepRunsFS struct{}

func (osSweepRunsFS) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }

// sweepRunsTreeBytes is the total size of the regular files under dir, read
// through fsys. It does not follow symlinks, and an unreadable part of the tree
// is an error, so a run that cannot be sized is refused rather than counted as
// small.
func sweepRunsTreeBytes(fsys sweepRunsFileSystem, dir string) (int64, error) {
	children, err := fsys.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, child := range children {
		path := filepath.Join(dir, child.Name())
		if child.IsDir() {
			n, err := sweepRunsTreeBytes(fsys, path)
			if err != nil {
				return 0, err
			}
			total += n
			continue
		}
		if !child.Type().IsRegular() {
			continue
		}
		info, err := child.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (osSweepRunsFS) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func (osSweepRunsFS) RemoveAll(path string) error { return os.RemoveAll(path) }

// sweepRunsPruneSettings reads the prune's three settings from the patrol's block,
// returning the default for each that is empty. A value the patrol cannot act on
// is an error naming its key.
func sweepRunsPruneSettings(cfg *DaemonPatrolConfig) (time.Duration, int, time.Duration, error) {
	var settings agentconfig.DiskMaintenanceConfig
	if c := diskMaintenanceSettings(cfg); c != nil {
		settings = *c
	}
	every, err := settings.SweepRunsPruneEvery()
	if err != nil {
		return 0, 0, 0, err
	}
	keep, err := settings.SweepRunsKeepCount()
	if err != nil {
		return 0, 0, 0, err
	}
	minAge, err := settings.SweepRunsMinAgeDuration()
	if err != nil {
		return 0, 0, 0, err
	}
	return every, keep, minAge, nil
}

// runSweepRunsPruneTick is the prune's own tick. It runs before the quiet guard
// and on its own interval, as the engine prune does, and its single-flight guard
// keeps one pass in flight.
func (d *Daemon) runSweepRunsPruneTick() {
	pc := d.patrol()
	s := d.diskMaintenanceSeams()
	now := s.now()
	every, _, _, err := sweepRunsPruneSettings(pc)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(sweepRunsPruneName, now, every) {
		return
	}
	mode, err := diskMaintenanceMode(pc)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.sweepRunsPruneRunning.CompareAndSwap(false, true) {
		return // a pass is still running
	}
	s.dispatch(func() {
		defer d.sweepRunsPruneRunning.Store(false)
		d.runSweepRunsPrunePass(mode)
	})
}

// runSweepRunsPrunePass is the pass the tick dispatches. The due check is read
// again here, so a run that concluded after the tick's decision does not run
// twice. A run that reaches no verdict is tried again on the retry interval.
func (d *Daemon) runSweepRunsPrunePass(mode string) {
	s := d.diskMaintenanceSeams()
	every, keep, minAge, err := sweepRunsPruneSettings(d.patrol())
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(sweepRunsPruneName, s.now(), every) {
		return
	}
	verdict := d.sweepRunsPrune(mode, keep, minAge)
	concluded := s.now()
	if verdict {
		d.diskMaintSchedule.record(sweepRunsPruneName, concluded)
		return
	}
	d.diskMaintSchedule.recordRetry(sweepRunsPruneName, concluded.Add(cacheTrimRetryInterval))
}

// sweepRunsDirectory is one directory the plan lists, with the path it is removed
// by and the label it is logged under.
type sweepRunsDirectory struct {
	label string
	path  string
	bytes int64
}

// sweepRunsPrune plans every rig's run directories and, in apply mode, removes
// the planned ones. It reports whether the run reached a verdict. A listing, size
// or check that fails refuses the run before anything is removed; a remove error
// stops it at that directory, and the name is logged. Neither is a verdict.
func (d *Daemon) sweepRunsPrune(mode string, keep int, minAge time.Duration) bool {
	s := d.diskMaintenanceSeams()
	fsys := s.sweepRunsFS
	now := s.now()
	root := filepath.Join(d.config.TownRoot, filepath.FromSlash(sweepRunsRootRel))

	// A root that does not exist yet has nothing to plan, and no line is due for
	// it: the sweep has not run in this town.
	rigs, err := fsys.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	} else if err != nil {
		d.sweepRunsPruneLogf("list", "disk_maintenance: sweep runs prune: cannot list %s: %v; nothing removed", root, err)
		return false
	}

	// Every rig is planned before anything is removed, so a failure in any one
	// refuses the whole run.
	var removals []sweepRunsDirectory
	kept := 0
	for _, rig := range rigs {
		if !rig.IsDir() {
			continue // a file or a symlink under the root is not a rig
		}
		rigDir := filepath.Join(root, rig.Name())
		children, err := fsys.ReadDir(rigDir)
		if err != nil {
			d.sweepRunsPruneLogf("list", "disk_maintenance: sweep runs prune: cannot list %s: %v; nothing removed", rigDir, err)
			return false
		}
		var entries []sweepRunEntry
		for _, child := range children {
			if !child.IsDir() {
				continue // a file or a symlink is never a run directory
			}
			info, err := child.Info()
			if err != nil {
				d.sweepRunsPruneLogf("stat", "disk_maintenance: sweep runs prune: cannot read %s: %v; nothing removed", filepath.Join(rigDir, child.Name()), err)
				return false
			}
			bytes, err := sweepRunsTreeBytes(fsys, filepath.Join(rigDir, child.Name()))
			if err != nil {
				d.sweepRunsPruneLogf("size", "disk_maintenance: sweep runs prune: cannot size %s: %v; nothing removed", filepath.Join(rigDir, child.Name()), err)
				return false
			}
			entries = append(entries, sweepRunEntry{Name: child.Name(), ModTime: info.ModTime(), Bytes: bytes})
		}
		plan := planSweepRunsPrune(entries, now, keep, minAge)
		kept += len(entries) - len(plan.Skipped) - len(plan.Remove)
		for _, r := range plan.Remove {
			removals = append(removals, sweepRunsDirectory{
				label: rig.Name() + "/" + r.Name,
				path:  filepath.Join(rigDir, r.Name),
				bytes: r.Bytes,
			})
		}
	}

	var total int64
	for _, r := range removals {
		total += r.bytes
	}
	if mode != agentconfig.DiskMaintenanceModeApply {
		d.logger.Printf("disk_maintenance: sweep runs prune dry_run: would remove %s, keeping %d%s",
			sweepRunsCounts(len(removals), total), kept, sweepRunsLabels(removals))
		return true
	}

	removed := 0
	var freed int64
	for _, r := range removals {
		// The name is checked again on disk: it must still be a directory, not a
		// symlink or a file that took its place, when it is removed.
		info, err := fsys.Lstat(r.path)
		if err != nil {
			d.logger.Printf("disk_maintenance: sweep runs prune apply stopped: check %s: %v; removed %s, keeping %d",
				r.label, err, sweepRunsCounts(removed, freed), kept)
			return false
		}
		if !info.IsDir() {
			d.logger.Printf("disk_maintenance: sweep runs prune apply stopped: %s is no longer a directory; removed %s, keeping %d",
				r.label, sweepRunsCounts(removed, freed), kept)
			return false
		}
		if err := fsys.RemoveAll(r.path); err != nil {
			d.logger.Printf("disk_maintenance: sweep runs prune apply stopped: remove %s: %v; removed %s, keeping %d",
				r.label, err, sweepRunsCounts(removed, freed), kept)
			return false
		}
		removed++
		freed += r.bytes
	}
	d.logger.Printf("disk_maintenance: sweep runs prune apply: removed %s, keeping %d",
		sweepRunsCounts(removed, freed), kept)
	return true
}

// sweepRunsCounts is the count and size a sweep runs line states.
func sweepRunsCounts(directories int, bytes int64) string {
	return fmt.Sprintf("%d directories, %.1f GB", directories, float64(bytes)/1e9)
}

// sweepRunsLabels is the list of directories a dry run names, after a colon. A
// plan that removes nothing names nothing.
func sweepRunsLabels(removals []sweepRunsDirectory) string {
	if len(removals) == 0 {
		return ""
	}
	labels := make([]string, 0, len(removals))
	for _, r := range removals {
		labels = append(labels, r.label)
	}
	return ": " + strings.Join(labels, ", ")
}

// sweepRunsPruneLogf says a refusal of the prune once per refusal window, as the
// engine prune's refusals are: a listing that keeps failing is one line an hour,
// not one per retry.
func (d *Daemon) sweepRunsPruneLogf(kind, format string, args ...any) {
	if d.diskMaintSchedule.allowLog("sweep-runs-prune "+kind, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", fmt.Sprintf(format, args...))
	}
}
