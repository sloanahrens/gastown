// Package export computes one month's metering figures per workspace as a pure
// function of that month's sample rows: seat-hours by profile, storage
// GB-months, the split of the fixed monthly overhead, unpriced token counts and
// the customer-zero price flag.
//
// Its callers are the operator console's read views (plan row V2), the customer
// dashboard (plan row O6) and the invoice renderer (plan row I1). It reads no
// record and keeps no clock: the adapters from the event, usage, sampler,
// tracker-size and Forgejo-size records to Input are later beads, and the
// prices and monthly floor of plan row I1 are applied downstream.
package export

import (
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// bytesPerGiB is the unit storage is reported in.
const bytesPerGiB = 1 << 30

// CalendarMonth is a calendar month in UTC, the period one export covers.
type CalendarMonth struct {
	Year  int
	Month time.Month
}

// Start is the month's first instant, UTC.
func (m CalendarMonth) Start() time.Time {
	return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC)
}

// End is the first instant of the month after m, UTC.
func (m CalendarMonth) End() time.Time {
	return m.Start().AddDate(0, 1, 0)
}

// Days is the number of days in m.
func (m CalendarMonth) Days() int {
	return int(m.End().Sub(m.Start()) / (24 * time.Hour))
}

// Input is the month's sample rows: everything Month reads.
type Input struct {
	// Month is the calendar month the export covers. A Seat or Storage row whose
	// interval or day lies outside it still names its workspace but contributes
	// no figures; a TokenCount row carries no date, so it is read as the month's
	// total whatever Month is.
	Month CalendarMonth
	// OverheadUSD is the town's fixed monthly overhead, split equally over the
	// workspaces active in the month.
	OverheadUSD float64
	Seats       []Seat
	Storage     []StorageSample
	Tokens      []TokenCount
}

// Seat is one seat interval, the time a workspace held a seat on a profile.
// An interval crossing a month's edge counts only its part inside the month,
// and an interval with no length inside it contributes nothing.
type Seat struct {
	Workspace string
	Profile   string
	Start     time.Time
	End       time.Time
}

// StorageSample is one nightly measure of stored bytes for a workspace, kind
// and UTC day: one row per repository or issue-id prefix (plan rows M6, M7).
type StorageSample struct {
	Workspace string
	Kind      string
	Bytes     int64
	// Day is when the measure was taken; only its UTC date is read.
	Day time.Time
}

// TokenCount is one workspace's token counts for the month. It carries no date,
// so the adapter that reads the usage records totals them for the month before
// calling Month; rows for one workspace sum, and a price is applied downstream
// (plan row I1), never here.
type TokenCount struct {
	Workspace     string
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

// Export is one month's figures for every workspace the month's rows name,
// sorted by workspace id.
type Export struct {
	Workspaces []WorkspaceExport
}

// WorkspaceExport is one workspace's month.
type WorkspaceExport struct {
	Workspace string
	// SeatHours is the workspace's seat hours per profile, sorted by profile
	// name.
	SeatHours []ProfileHours
	// StorageGBMonths is the sum over kinds of each kind's mean bytes over the
	// month's days, in GiB. A day with no sample of a kind counts as zero: the
	// samplers write one line per existing prefix or repository (plan rows M6,
	// M7), so an absent line means the kind stored nothing that day.
	StorageGBMonths float64
	// OverheadShareUSD is the fixed monthly overhead split equally over the
	// workspaces active in the month; zero for an inactive one.
	OverheadShareUSD float64
	// Tokens is the workspace's token counts for the month, unpriced.
	Tokens Tokens
	// ZeroPrice is true for customer zero only.
	ZeroPrice bool
}

// ProfileHours is one profile's share of a workspace's seat hours.
type ProfileHours struct {
	Profile string
	Hours   float64
}

// Tokens is a workspace's token counts for the month.
type Tokens struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

// Month returns in's figures: seats clipped to the month, storage means over
// the month's days, the overhead split over the active workspaces, and customer
// zero's zero-price flag.
func Month(in Input) Export {
	start, end := in.Month.Start(), in.Month.End()

	named := map[string]struct{}{}
	seatTime := map[string]map[string]time.Duration{}
	storage := map[string]map[string]map[string]int64{}
	tokens := map[string]Tokens{}

	for _, s := range in.Seats {
		named[s.Workspace] = struct{}{}
		from, to := s.Start, s.End
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		if !to.After(from) {
			continue
		}
		profiles := seatTime[s.Workspace]
		if profiles == nil {
			profiles = map[string]time.Duration{}
			seatTime[s.Workspace] = profiles
		}
		profiles[s.Profile] += to.Sub(from)
	}

	for _, s := range in.Storage {
		named[s.Workspace] = struct{}{}
		day := s.Day.UTC()
		if day.Before(start) || !day.Before(end) {
			continue
		}
		// A kind's day total is every sample of that kind that day, across its
		// repositories or prefixes; storageGBMonths then means each kind over
		// the month's days.
		kinds := storage[s.Workspace]
		if kinds == nil {
			kinds = map[string]map[string]int64{}
			storage[s.Workspace] = kinds
		}
		days := kinds[s.Kind]
		if days == nil {
			days = map[string]int64{}
			kinds[s.Kind] = days
		}
		days[day.Format(time.DateOnly)] += s.Bytes
	}

	for _, c := range in.Tokens {
		named[c.Workspace] = struct{}{}
		t := tokens[c.Workspace]
		t.Input += c.Input
		t.Output += c.Output
		t.CacheCreation += c.CacheCreation
		t.CacheRead += c.CacheRead
		tokens[c.Workspace] = t
	}

	active := activeWorkspaces(seatTime, storage, tokens)
	share := 0.0
	if len(active) > 0 {
		share = in.OverheadUSD / float64(len(active))
	}

	ids := make([]string, 0, len(named))
	for id := range named {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := Export{Workspaces: make([]WorkspaceExport, 0, len(ids))}
	for _, id := range ids {
		we := WorkspaceExport{
			Workspace:       id,
			SeatHours:       profileHours(seatTime[id]),
			StorageGBMonths: storageGBMonths(storage[id], in.Month),
			Tokens:          tokens[id],
			ZeroPrice:       id == workspaceid.CustomerZero,
		}
		if active[id] {
			we.OverheadShareUSD = share
		}
		out.Workspaces = append(out.Workspaces, we)
	}
	return out
}

// activeWorkspaces is the set of workspaces the month holds usage for: seat
// time, stored bytes or tokens. A workspace the rows name but that used
// nothing pays no share of the overhead.
func activeWorkspaces(seatTime map[string]map[string]time.Duration, storage map[string]map[string]map[string]int64, tokens map[string]Tokens) map[string]bool {
	active := map[string]bool{}
	// Only intervals with a length inside the month reach seatTime.
	for id := range seatTime {
		active[id] = true
	}
	for id, kinds := range storage {
		for _, days := range kinds {
			for _, bytes := range days {
				if bytes > 0 {
					active[id] = true
				}
			}
		}
	}
	for id, t := range tokens {
		if t.Input > 0 || t.Output > 0 || t.CacheCreation > 0 || t.CacheRead > 0 {
			active[id] = true
		}
	}
	return active
}

// profileHours turns a workspace's per-profile seat time into hours sorted by
// profile name.
func profileHours(profiles map[string]time.Duration) []ProfileHours {
	if len(profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProfileHours, 0, len(names))
	for _, name := range names {
		out = append(out, ProfileHours{Profile: name, Hours: profiles[name].Hours()})
	}
	return out
}

// storageGBMonths is the sum over kinds of each kind's mean bytes over the
// month's days, in GiB. Kinds are summed in name order so the float result does
// not depend on map order, and a kind with no sample in the month adds nothing.
func storageGBMonths(kinds map[string]map[string]int64, month CalendarMonth) float64 {
	if len(kinds) == 0 {
		return 0
	}
	daysInMonth := float64(month.Days())
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	var meanBytes float64
	for _, name := range names {
		dailyTotals := kinds[name]
		if len(dailyTotals) == 0 {
			continue
		}
		var sum int64
		for _, bytes := range dailyTotals {
			sum += bytes
		}
		meanBytes += float64(sum) / daysInMonth
	}
	return meanBytes / bytesPerGiB
}
