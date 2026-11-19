package daemon

// disk_maintenance_growth keeps the samples the disk growth trend is fitted
// from. After each read the patrol appends one sample per kept disk to that
// disk's ring under the town's runtime directory (gt-gj8zbw). The samples are
// the bytes the patrol already read, so nothing here reads a disk; a failed
// write logs one line and changes no alert, keep or plan.

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/diskmaint/growth"
)

const (
	// diskGrowthSampleEvery is the least spacing between two samples of one
	// disk. A pass that comes sooner appends nothing for that disk.
	diskGrowthSampleEvery = 5 * time.Minute

	// diskGrowthDir is the directory under the town's runtime directory that
	// holds the rings, one JSON-lines file per kept disk.
	diskGrowthDir = "disk-growth"
)

// diskGrowthRings maps each kept disk to the name of its ring's file. The
// control entry is the busiest node of the control cluster, the reading the
// patrol alerts on: a multi-node cluster hides the growth of its other nodes.
var diskGrowthRings = map[string]string{
	diskMaintDiskColima:  "sandbox",
	diskMaintDiskControl: "control",
}

// diskGrowthState is when each kept disk's ring last took a sample, and the
// lock that serializes the ring writes. A ring is read and rewritten whole, so
// two passes appending at once could drop a line.
type diskGrowthState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// recordDiskGrowth appends one sample for each kept disk whose reading is in
// hand, unless that disk's ring took one less than diskGrowthSampleEvery ago.
// A reading that carries an error, or names no byte count, appends nothing for
// its disk, and so does a disk the patrol does not keep.
func (d *Daemon) recordDiskGrowth(now time.Time, readings []disks.Reading) {
	d.diskGrowth.mu.Lock()
	defer d.diskGrowth.mu.Unlock()

	var due []disks.Reading
	for _, r := range readings {
		if _, kept := diskGrowthRings[r.Name]; !kept || r.Err != nil || r.TotalBytes <= 0 {
			continue
		}
		if last, ok := d.diskGrowth.last[r.Name]; ok && now.Sub(last) < diskGrowthSampleEvery {
			continue
		}
		due = append(due, r)
	}
	if len(due) == 0 {
		return
	}
	dir := filepath.Join(d.config.TownRoot, ".runtime", diskGrowthDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.logger.Printf("disk_maintenance: growth samples not recorded: %v", err)
		return
	}
	for _, r := range due {
		path := filepath.Join(dir, diskGrowthRings[r.Name]+".jsonl")
		sample := growth.Sample{At: now, UsedBytes: r.UsedBytes, TotalBytes: r.TotalBytes}
		if err := growth.Append(path, sample, growth.DefaultKeep); err != nil {
			d.logger.Printf("disk_maintenance: growth sample for %s not recorded: %v", r.Name, err)
			continue
		}
		if d.diskGrowth.last == nil {
			d.diskGrowth.last = map[string]time.Time{}
		}
		d.diskGrowth.last[r.Name] = now
	}
}
