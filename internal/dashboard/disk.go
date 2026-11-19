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

	// diskRateWindow24h and diskRateWindow7d are the two windows the block
	// quotes each disk's growth over, the "(24h)" and "(7d)" of its line.
	diskRateWindow24h = 24 * time.Hour
	diskRateWindow7d  = 7 * 24 * time.Hour
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
//
// The growth figures are fitted from the same ring samples. GrowthOK says both
// windows' rates are numbers and is false on a NoSamples or Err row; DaysLeftOK
// says DaysLeft is a projection, which a flat or falling disk cannot give. The
// floats are always sent so the page never has to guess whether a zero is a
// reading (gt-u3ccue).
type DiskRow struct {
	Name       string    `json:"name"`
	UsedBytes  int64     `json:"used_bytes,omitempty"`
	TotalBytes int64     `json:"total_bytes,omitempty"`
	UsedPct    int       `json:"used_pct"`
	SampledAt  time.Time `json:"sampled_at,omitempty"`
	NoSamples  bool      `json:"no_samples,omitempty"`
	Err        string    `json:"err,omitempty"`

	GibPerDay24h float64 `json:"gib_per_day_24h"`
	GibPerDay7d  float64 `json:"gib_per_day_7d"`
	DaysLeft     float64 `json:"days_left"`
	GrowthOK     bool    `json:"growth_ok"`
	DaysLeftOK   bool    `json:"days_left_ok"`
}

// ReadDisk returns the last sample of each kept disk's ring under runtimeDir,
// the control node first, and the growth each ring's samples fit over the 24
// hours and the week ending at now. A ring that is not there or holds no sample
// is a row with NoSamples set, and a ring the reader cannot read is a row with
// Err set: a failed read never passes as an empty disk.
func ReadDisk(runtimeDir string, now time.Time) *Disk {
	dir := filepath.Join(runtimeDir, diskGrowthDir)
	return &Disk{Rows: []DiskRow{
		readDiskRing(dir, diskRingControl, now),
		readDiskRing(dir, diskRingSandbox, now),
	}}
}

// readDiskRing reads one ring's last sample and growth as a row; the ring is
// the file the disk maintenance patrol's recordDiskGrowth appends to.
func readDiskRing(dir, name string, now time.Time) DiskRow {
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

	gib24h, ok24 := growth.Rate(samples, now, diskRateWindow24h)
	gib7d, ok7 := growth.Rate(samples, now, diskRateWindow7d)
	row.GibPerDay24h, row.GibPerDay7d = gib24h, gib7d
	row.GrowthOK = ok24 && ok7
	if row.GrowthOK {
		// The projection runs at the week's pace, not the day's: one day's
		// burst or prune would move a day-rate forecast, and the 24-hour rate
		// is the line's detail rather than its forecast.
		row.DaysLeft, row.DaysLeftOK = growth.DaysLeft(last.TotalBytes, last.UsedBytes, row.GibPerDay7d)
	}
	return row
}

// wholePercent is used's share of total as a whole percentage, rounded to the
// nearest number: 82.5 rounds up to 83. A caller passes a positive total.
func wholePercent(used, total int64) int {
	return int((used*100 + total/2) / total)
}
