package daemon

// disk_maintenance_disks is the disk half of the patrol: it reads the three
// disks the town writes to, raises the 80/70 usage alerts and the unreadable
// alert, and tightens the registry keep while a disk is nearly full
// (gt-dl4nv.16, docs/plans/2026-10-28-disk-maintenance-design.md).
//
// A disk the patrol cannot measure is not a quiet disk: a failed read or
// output it cannot parse raises diskmaint:<disk>:unreadable and refuses the
// plan, rather than reading as an empty disk that keeps everything.
//
// The active alert keys live in memory, keyed by alert rather than rebuilt from
// the escalation store at boot: a restart forgets them and re-raises an open
// alert once, which costs a repeat rather than a silence.

import (
	"context"
	"sync"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

const (
	// diskMaintAlertSource is the source every disk alert carries: the patrol
	// that read the disk.
	diskMaintAlertSource = "disk_maintenance"

	// The disks the patrol reads. The names are the alert keys' middle part,
	// so they are part of the alert identity: rename a disk and its open alert
	// is orphaned under the old name.
	diskMaintDiskMac           = "mac"
	diskMaintDiskColima        = "colima"
	diskMaintDiskDockerDesktop = "docker-desktop"

	// diskMaintRunnerContainer is the container whose root filesystem is the
	// Docker Desktop disk the CI runner fills; deploy creates it.
	diskMaintRunnerContainer = "forgejo-runner-daemon-1"

	// diskMaintColimaProfile is the sandbox profile whose data disk the
	// runner's images land on.
	diskMaintColimaProfile = "gt-sbx"
)

// DFReader runs one disk's df and returns its output. A failed read is an
// error, never an empty reading: the patrol must not mistake a disk it could
// not reach for a disk with room.
type DFReader func(ctx context.Context) ([]byte, error)

// diskMaintDisk is one watched disk: the name its alerts are keyed by and the
// df command that measures it.
type diskMaintDisk struct {
	name string
	df   DFReader
}

// diskMaintDisks returns the disks the patrol reads, in report order: the Mac
// volume, Colima's sandbox data disk, and the Docker Desktop disk the CI
// runner writes to.
func (d *Daemon) diskMaintDisks() []diskMaintDisk {
	return []diskMaintDisk{
		{name: diskMaintDiskMac, df: d.diskMaintDF("df", "-P", "/")},
		{name: diskMaintDiskColima, df: d.diskMaintDF("colima", "ssh", "-p", diskMaintColimaProfile,
			"--", "df", "-P", "/mnt/lima-colima-gt-sbx")},
		{name: diskMaintDiskDockerDesktop, df: d.diskMaintDF("docker", "exec",
			diskMaintRunnerContainer, "df", "-P", "/")},
	}
}

// diskMaintDF runs one disk's df through the daemon's exec seam, so the unit
// tier never launches df, colima or docker.
func (d *Daemon) diskMaintDF(name string, args ...string) DFReader {
	return func(ctx context.Context) ([]byte, error) {
		return d.diskMaintRunner(ctx, nil, name, args...)
	}
}

// diskMaintenanceReadings reads every watched disk once, through the injected
// reader a disk has in tests and its own df command otherwise. A failed read
// or output that names no capacity becomes that disk's reading error.
func (d *Daemon) diskMaintenanceReadings(ctx context.Context, injected map[string]DFReader) []disks.Reading {
	watched := d.diskMaintDisks()
	readings := make([]disks.Reading, 0, len(watched))
	for _, disk := range watched {
		read := disk.df
		if fn, ok := injected[disk.name]; ok {
			read = fn
		}
		out, err := read(ctx)
		if err != nil {
			readings = append(readings, disks.Reading{Name: disk.name, Err: err})
			continue
		}
		pct, err := disks.ParseDF(string(out))
		if err != nil {
			readings = append(readings, disks.Reading{Name: disk.name, Err: err})
			continue
		}
		readings = append(readings, disks.Reading{Name: disk.name, UsedPct: pct})
	}
	return readings
}

// diskMaintAlerts is the patrol's memory of the disk alert keys it has open.
// A pass writes it and the next pass reads it, one pass at a time, but the
// mutex keeps the read off a pass that has not settled yet.
type diskMaintAlerts struct {
	mu     sync.Mutex
	active map[string]bool
}

// snapshot returns a copy of the open keys, which is what disks.Evaluate
// compares the readings against.
func (a *diskMaintAlerts) snapshot() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	open := make(map[string]bool, len(a.active))
	for key, active := range a.active {
		open[key] = active
	}
	return open
}

// mark records a key as open, or forgets it once its alert is cleared.
func (a *diskMaintAlerts) mark(key string, open bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !open {
		delete(a.active, key)
		return
	}
	if a.active == nil {
		a.active = map[string]bool{}
	}
	a.active[key] = true
}

// applyDiskAlerts raises and clears the alerts the readings call for, and
// records only the ones that landed. A raise the notifier refused stays
// unrecorded, so the next pass raises it again rather than treating a silence
// as an open alert; a clear that failed leaves its key open for the same
// reason.
func (d *Daemon) applyDiskAlerts(readings []disks.Reading) {
	for _, alert := range disks.Evaluate(readings, d.diskMaintAlerts.snapshot()) {
		switch {
		case alert.Raise:
			if err := d.escalateAlertErr(alert.Key, diskMaintAlertSource, alert.Detail); err != nil {
				d.logger.Printf("disk_maintenance: alert %s was not delivered: %v", alert.Key, err)
				continue
			}
			d.diskMaintAlerts.mark(alert.Key, true)
		case alert.Clear:
			if err := d.clearAlertsErr(alert.Detail, alert.Key); err != nil {
				d.logger.Printf("disk_maintenance: alert %s was not cleared: %v", alert.Key, err)
				continue // still open, and the next pass clears it again
			}
			d.diskMaintAlerts.mark(alert.Key, false)
		}
	}
}
