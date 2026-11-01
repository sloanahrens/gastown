// Package custview builds the customer dashboard's view of one hosted workspace
// (plan row O6): its state and health, its beads counted by state, its spend
// against the daily cap, the month's metering totals, and the actions it may
// take.
//
// Nothing calls it yet. Its caller is the dashboard handler (a later bead),
// which supplies a Source over the admin plane's records. The package holds no
// HTTP and does no I/O, so a view is testable from a fake Source.
package custview

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/metering/export"
	"github.com/steveyegge/gastown/internal/spendcap"
)

// View is what the dashboard shows for one workspace. No field it holds can
// carry a tool's code, an API key, a transcript or a bead body, so the refusal
// is structural: Build copies only the fields named here out of the record it
// reads.
type View struct {
	Status   Status   `json:"status"`
	Progress Progress `json:"progress"`
	Spend    Spend    `json:"spend"`
	Usage    Usage    `json:"usage"`
	Actions  []Action `json:"actions"`
}

// Status is where the workspace stands: the state the registry moves it
// through, and its health as the operator console reports it.
type Status struct {
	State  registry.State `json:"state"`
	Health string         `json:"health"`
}

// Progress is the workspace's beads counted by state. It counts them and never
// carries one: a bead's body stays in the record.
type Progress struct {
	ByState []StateCount `json:"by_state"`
}

// StateCount is one bead state's tally.
type StateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// Spend is the workspace's money at the cap: the month's spend so far, the
// daily cap, and the K4 decision for the UTC day of the record's Now. The
// month's spend runs from the first of Now's UTC month through the end of its
// day, so it never reads below the decision's spend. A cap of zero or less
// means none is set and the decision is Under.
type Spend struct {
	MonthToDateUSD float64           `json:"month_to_date_usd"`
	DailyCapUSD    float64           `json:"daily_cap_usd"`
	Decision       spendcap.Decision `json:"decision"`
}

// Usage is the month's metering totals, shaped as the M8 export computes them:
// seat hours by profile, storage GB-months, and unpriced token counts.
type Usage struct {
	SeatHours       []export.ProfileHours `json:"seat_hours"`
	StorageGBMonths float64               `json:"storage_gb_months"`
	Tokens          export.Tokens         `json:"tokens"`
}

// Action is one dashboard button a workspace's state offers.
type Action string

const (
	// Pause stops an active workspace (plan row V4).
	Pause Action = "pause"
	// Resume restarts a paused workspace (the owner's action, a later bead).
	Resume Action = "resume"
)

// Bead is one of the workspace's beads as a Source holds it: the state it is
// counted by, and the body the view never copies.
type Bead struct {
	State string
	Body  string
}

// Record is everything a Source holds for one workspace: the fields a view
// copies and whatever else the admin plane's records hold. Code, APIKey and
// Transcript are here so a record source and its fake carry the fields a view
// refuses, and a test can prove none of them reaches the view. The month's
// spend is not a field: a view sums SpendRecords, so it cannot disagree with
// them.
type Record struct {
	ID           string
	State        registry.State
	Health       string
	Beads        []Bead
	Cap          spendcap.Cap
	SpendRecords []spendcap.Record
	Now          time.Time
	Usage        export.WorkspaceExport

	Code       string
	APIKey     string
	Transcript string
}

// Source is what the view reads: one workspace's record, as the dashboard
// handler's adapter supplies it. An error means the view cannot be built, so no
// error may read as an empty record.
type Source interface {
	// Workspace returns the record of the workspace the dashboard shows.
	Workspace() (Record, error)
}

var (
	// ErrNoSource is Build's refusal of a nil Source, which has no record to
	// read.
	ErrNoSource = errors.New("custview: a source is required")
	// ErrUnknownState is Build's refusal of a state the registry does not
	// define. Unknown denies: an empty action list would read as "nothing to
	// do" for a state the dashboard cannot describe.
	ErrUnknownState = errors.New("custview: workspace state is not one the registry defines")
	// ErrNoNow is Build's refusal of a zero Now. The cap decision is the UTC
	// day of Now, so a record with no clock would report Under for a day that
	// holds no usage.
	ErrNoNow = errors.New("custview: the record carries no time for the cap decision")
	// ErrEmptyBeadState is Build's refusal of a bead with no state, which it
	// could not count under any row the dashboard shows.
	ErrEmptyBeadState = errors.New("custview: a bead has an empty state")
	// ErrCapWorkspace is Build's refusal of a cap that names another workspace.
	// Evaluate sums the spend of the cap's workspace, so a cap for another
	// workspace would show that workspace's spend on this one's dashboard, even
	// when the cap sets no daily limit.
	ErrCapWorkspace = errors.New("custview: the daily cap names a different workspace")
	// ErrUsageWorkspace is Build's refusal of a usage export that names another
	// workspace, or none. Its totals would be copied into this workspace's view,
	// so an export that cannot name this workspace is refused.
	ErrUsageWorkspace = errors.New("custview: the usage export does not name this workspace")
	// ErrUnknownBeadState is Build's refusal of a bead state the bead store does
	// not define. A state is copied into the view as a label, so a value outside
	// the store's set could carry a bead's body into it.
	ErrUnknownBeadState = errors.New("custview: a bead has a state the bead store does not define")
)

// beadStates is the bead store's status set, as the bead CLI reference names
// it. A bead in any other state is refused.
var beadStates = map[string]bool{
	"open":        true,
	"in_progress": true,
	"blocked":     true,
	"deferred":    true,
	"closed":      true,
	"pinned":      true,
	"hooked":      true,
}

// Build returns the dashboard view of src's workspace.
func Build(src Source) (View, error) {
	if src == nil {
		return View{}, ErrNoSource
	}
	rec, err := src.Workspace()
	if err != nil {
		return View{}, fmt.Errorf("custview: workspace record: %w", err)
	}
	actions, ok := actionsFor(rec.State)
	if !ok {
		return View{}, fmt.Errorf("%w: %q", ErrUnknownState, rec.State)
	}
	if rec.Now.IsZero() {
		return View{}, ErrNoNow
	}
	// A cap with no workspace name belongs to the record it sits in. A cap that
	// names another workspace is refused whatever its daily limit, because
	// Evaluate would sum that workspace's spend here.
	if rec.Cap.Workspace != "" && rec.Cap.Workspace != rec.ID {
		return View{}, fmt.Errorf("%w: cap for %q, workspace %q", ErrCapWorkspace, rec.Cap.Workspace, rec.ID)
	}
	// The usage totals are copied as this workspace's, so an export that names
	// another workspace, or none, is refused.
	if rec.Usage.Workspace != rec.ID {
		return View{}, fmt.Errorf("%w: usage for %q, workspace %q", ErrUsageWorkspace, rec.Usage.Workspace, rec.ID)
	}
	counts, err := countsByState(rec.Beads)
	if err != nil {
		return View{}, err
	}
	// The cap is evaluated for rec.ID, not for rec.Cap.Workspace, which may be
	// empty when the record carries no cap.
	cap := spendcap.Cap{Workspace: rec.ID, DailyUSD: rec.Cap.DailyUSD}
	return View{
		Status:   Status{State: rec.State, Health: rec.Health},
		Progress: Progress{ByState: counts},
		Spend: Spend{
			MonthToDateUSD: monthToDate(rec.SpendRecords, rec.ID, rec.Now),
			DailyCapUSD:    rec.Cap.DailyUSD,
			Decision:       spendcap.Evaluate(cap, rec.SpendRecords, rec.Now),
		},
		Usage:   usage(rec.Usage),
		Actions: actions,
	}, nil
}

// actionsFor is the whole rule for the buttons a state offers: active may be
// paused, paused may be resumed, and suspended or closed offers nothing. A
// state with no row is not one the registry defines, and the second result is
// false.
func actionsFor(state registry.State) ([]Action, bool) {
	switch state {
	case registry.Active:
		return []Action{Pause}, true
	case registry.Paused:
		return []Action{Resume}, true
	case registry.Suspended, registry.Closed:
		return []Action{}, true
	}
	return nil, false
}

// countsByState tallies beads by state, in state-name order so the view does
// not depend on the order the source holds its beads in. A bead whose state is
// not the store's set is refused by its index alone: the state may hold text
// the view must not echo.
func countsByState(beads []Bead) ([]StateCount, error) {
	tally := make(map[string]int, len(beads))
	for i, b := range beads {
		if b.State == "" {
			return nil, fmt.Errorf("%w: bead %d", ErrEmptyBeadState, i)
		}
		if !beadStates[b.State] {
			return nil, fmt.Errorf("%w: bead %d", ErrUnknownBeadState, i)
		}
		tally[b.State]++
	}
	states := make([]string, 0, len(tally))
	for state := range tally {
		states = append(states, state)
	}
	sort.Strings(states)
	out := make([]StateCount, 0, len(states))
	for _, state := range states {
		out = append(out, StateCount{State: state, Count: tally[state]})
	}
	return out, nil
}

// monthToDate sums the workspace's spend from the first of now's UTC month
// through the end of now's UTC day, the window the day's decision reads, so the
// month figure includes the day's spend. Records for other workspaces are left
// out.
func monthToDate(recs []spendcap.Record, workspace string, now time.Time) float64 {
	local := now.UTC()
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, time.UTC)
	var total float64
	for _, r := range recs {
		if r.Workspace != workspace || r.EndedAt.Before(start) || !r.EndedAt.Before(end) {
			continue
		}
		total += r.CostUSD
	}
	return total
}

// usage copies the export's per-workspace figures and its seat-hours slice, so
// a later change to the source's record cannot change a view already returned.
// The seat hours are never nil, so a month with none serializes as an empty
// list, as Actions does.
func usage(w export.WorkspaceExport) Usage {
	return Usage{
		SeatHours:       append(make([]export.ProfileHours, 0, len(w.SeatHours)), w.SeatHours...),
		StorageGBMonths: w.StorageGBMonths,
		Tokens:          w.Tokens,
	}
}
