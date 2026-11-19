package dashboard

// The Disk panel's reading: each node disk's used share, taken from the last
// sample of the ring the disk maintenance patrol appends to. This file only
// reads the rings; the growth rates and the page block are built on the rows
// it returns (gt-a4bhxc).

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/growth"
)

const (
	// diskGrowthDir is the directory under the town's runtime directory that
	// holds one JSON-lines ring per kept disk, written by the disk maintenance
	// patrol.
	diskGrowthDir = "disk-growth"

	// diskRingControl is the control cluster's busiest node, the reading the
	// patrol alerts on, and diskRingSandbox the Colima VM's disk. They are the
	// two rings ReadDisk reports, in this order.
	diskRingControl = "control"
	diskRingSandbox = "sandbox"
)

// Disk is the dashboard's disk reading: one row per kept disk, from the last
// sample of its ring, the control node first.
type Disk struct {
	Rows []DiskRow `json:"rows"`
}

// DiskRow is one disk's last reading. NoSamples says the ring holds nothing yet
// or is not there; Err says the ring could not be read, and the row then carries
// no share rather than a zero one. UsedPct is the whole-percent used share and
// is always sent, so a reader can tell a real 0% from a row with no reading by
// the two flags beside it.
type DiskRow struct {
	Name       string    `json:"name"`
	UsedBytes  int64     `json:"used_bytes,omitempty"`
	TotalBytes int64     `json:"total_bytes,omitempty"`
	UsedPct    int       `json:"used_pct"`
	SampledAt  time.Time `json:"sampled_at,omitempty"`
	NoSamples  bool      `json:"no_samples,omitempty"`
	Err        string    `json:"err,omitempty"`
}

// ReadDisk returns the last sample of each kept disk's ring under runtimeDir,
// the control node first. A ring that is not there or holds no sample is a row
// with NoSamples set, and a ring the reader cannot read is a row with Err set:
// a failed read never passes as an empty disk.
func ReadDisk(runtimeDir string) *Disk {
	dir := filepath.Join(runtimeDir, diskGrowthDir)
	return &Disk{Rows: []DiskRow{
		readDiskRing(dir, diskRingControl),
		readDiskRing(dir, diskRingSandbox),
	}}
}

// readDiskRing reads one ring's last sample as a row; the ring is the file the
// disk maintenance patrol's recordDiskGrowth appends to.
func readDiskRing(dir, name string) DiskRow {
	row := DiskRow{Name: name}
	samples, err := growth.Read(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		row.Err = err.Error()
		return row
	}
	if len(samples) == 0 {
		row.NoSamples = true
		return row
	}
	last := samples[len(samples)-1]
	row.UsedBytes, row.TotalBytes, row.SampledAt = last.UsedBytes, last.TotalBytes, last.At
	if last.TotalBytes <= 0 {
		// growth.Read keeps a sample whose total is zero, so a damaged or
		// hand-written ring can hold one. A share of a zero total is not a
		// number; say so rather than draw it as 0% used.
		row.Err = fmt.Sprintf("disk: %s ring's last sample has no total capacity", name)
		return row
	}
	row.UsedPct = wholePercent(last.UsedBytes, last.TotalBytes)
	return row
}

// wholePercent is used's share of total as a whole percentage, rounded to the
// nearest number: 82.5 rounds up to 83. A caller passes a positive total.
func wholePercent(used, total int64) int {
	return int((used*100 + total/2) / total)
}
