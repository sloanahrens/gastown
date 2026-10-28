// Package disks turns df output and disk readings into keyed alerts and the
// registry keep count for one run.
//
// The usage alert raises at RaiseAt and clears only below ClearAt, while the
// keep tightens at RaiseAt only when every disk was read, so an alert can
// outlive the tightened keep (gt-dl4nv.3).
package disks

import (
	"errors"
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
// alerts to apply: a readable disk raises its usage alert at or over RaiseAt and
// clears it below ClearAt, an unreadable disk raises its unreadable alert and
// leaves the usage alert alone, and no alert is raised for a key already active
// (gt-dl4nv.3).
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

// Keep is the number of newest items a prune keeps, or the refusal to name one.
// It is read through Count, which returns the refusal as an error, so a keep that
// was refused cannot be applied as a number by a caller that drops the error
// (gt-dl4nv.3).
type Keep struct {
	count   int
	decided bool
	refusal error
}

// Count returns the keep count, or the error naming every disk that could not be
// read. The count is meaningless with an error: apply nothing.
func (k Keep) Count() (int, error) {
	switch {
	case k.refusal != nil:
		return 0, k.refusal
	case !k.decided:
		return 0, errors.New("keep was never decided: the zero Keep is not a count")
	}
	return k.count, nil
}

// KeepFor returns base tightened when a readable disk is at or over RaiseAt, or a
// refusal when any disk could not be read, whatever the other disks say.
func KeepFor(readings []Reading, base int) Keep {
	var failures []error
	for _, r := range readings {
		if r.Err != nil {
			failures = append(failures, fmt.Errorf("disk %s cannot be read: %w", r.Name, r.Err))
		}
	}
	if len(failures) > 0 {
		return Keep{refusal: errors.Join(failures...)}
	}
	for _, r := range readings {
		if r.UsedPct >= RaiseAt && base > tightenedKeep {
			return Keep{count: tightenedKeep, decided: true}
		}
	}
	return Keep{count: base, decided: true}
}

func usageAlertKey(name string) string { return "diskmaint:" + name }

func unreadableAlertKey(name string) string { return usageAlertKey(name) + ":unreadable" }
