// Package disks turns df output and disk readings into keyed alerts and the
// registry keep count for one run.
//
// Both thresholds live here so the usage alert and the tightened keep turn at
// the same reading: a disk the patrol alerts on is a disk it stops keeping
// old images for (gt-dl4nv.3).
package disks

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// RaiseAt is the used share at which a disk raises its usage alert.
	RaiseAt = 80.0
	// ClearAt is the used share a disk must fall under to clear its usage
	// alert; the gap below RaiseAt keeps a disk at the threshold from
	// flapping (gt-dl4nv.3).
	ClearAt = 70.0
)

// tightenedKeep is the registry keep count used while a readable disk is at or
// over RaiseAt. A caller whose base is already this low stays there.
const tightenedKeep = 3

// Reading is one disk's used share, or the error that stopped it being read.
type Reading struct {
	Name    string // "mac", "colima", "docker-desktop"
	UsedPct float64
	Err     error // non-nil: the disk could not be read
}

// Alert is one keyed change for the daemon's alert store to apply.
type Alert struct {
	Key    string // "diskmaint:<name>" or "diskmaint:<name>:unreadable"
	Raise  bool
	Clear  bool
	Detail string
}

// ParseDF reads the used share from df -P output: the first field carrying a
// percent sign is the capacity column, whether or not the header line is
// present. A share outside 0-100 is an error, not a reading.
func ParseDF(out string) (float64, error) {
	for _, line := range strings.Split(out, "\n") {
		for _, field := range strings.Fields(line) {
			if !strings.HasSuffix(field, "%") {
				continue
			}
			pct, err := strconv.ParseFloat(strings.TrimSuffix(field, "%"), 64)
			if err != nil {
				return 0, fmt.Errorf("df capacity %q: %w", field, err)
			}
			if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
				return 0, fmt.Errorf("df capacity %q is outside 0-100", field)
			}
			return pct, nil
		}
	}
	return 0, fmt.Errorf("df output names no filesystem capacity")
}

// Evaluate compares the readings with the active alert keys and returns the
// alerts to apply: a disk at or over RaiseAt raises its usage alert, a disk
// under ClearAt with that alert active clears it, and a disk that cannot be
// read raises its own unreadable alert while leaving the usage alert it
// raised earlier alone.
func Evaluate(readings []Reading, active map[string]bool) []Alert {
	var alerts []Alert
	for _, r := range readings {
		usage := usageAlertKey(r.Name)
		unreadable := unreadableAlertKey(r.Name)
		if r.Err != nil {
			if !active[unreadable] {
				alerts = append(alerts, Alert{
					Key:    unreadable,
					Raise:  true,
					Detail: fmt.Sprintf("disk %s cannot be read: %v", r.Name, r.Err),
				})
			}
			continue
		}
		if active[unreadable] {
			alerts = append(alerts, Alert{
				Key:    unreadable,
				Clear:  true,
				Detail: fmt.Sprintf("disk %s reads again at %.0f%% used", r.Name, r.UsedPct),
			})
		}
		switch {
		case r.UsedPct >= RaiseAt && !active[usage]:
			alerts = append(alerts, Alert{
				Key:    usage,
				Raise:  true,
				Detail: fmt.Sprintf("disk %s is %.0f%% used (alert at %.0f%%)", r.Name, r.UsedPct, RaiseAt),
			})
		case r.UsedPct < ClearAt && active[usage]:
			alerts = append(alerts, Alert{
				Key:    usage,
				Clear:  true,
				Detail: fmt.Sprintf("disk %s is %.0f%% used (clears below %.0f%%)", r.Name, r.UsedPct, ClearAt),
			})
		}
	}
	return alerts
}

// KeepFor returns the registry keep count for this run: the tightened count
// while a readable disk is at or over RaiseAt, and base otherwise. A disk that
// cannot be read leaves the keep alone, because nothing was judged.
func KeepFor(readings []Reading, base int) int {
	for _, r := range readings {
		if r.Err == nil && r.UsedPct >= RaiseAt && base > tightenedKeep {
			return tightenedKeep
		}
	}
	return base
}

func usageAlertKey(name string) string { return "diskmaint:" + name }

func unreadableAlertKey(name string) string { return usageAlertKey(name) + ":unreadable" }
