package web

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/activity"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/slot"
)

// Gate role classes: what a slot holder's role string means to an operator.
// The first four are the classes the panel lists before any om-review
// marker exists (gt-1ln0); om-review joins them once editorial.Run takes its
// marker (plan Task 14a), and every other holder is a polecat's own suite.
const (
	gateClassRefinery = "refinery"
	gateClassBatch    = "refinery-batch"
	gateClassMainTest = "main-branch-test"
	gateClassOMReview = "om-review"
	gateClassPolecats = "polecats"
)

// GateStatus is the dashboard's container-gate panel: the pool's live slots,
// the gate role classes derived from their holders, and the one-line
// summaries an operator scans first. Its CLI counterpart is `gt slot status`,
// which prints the same pool in prose.
type GateStatus struct {
	Slots     []GateSlotRow
	Roles     []GateRoleRow
	Summaries []string
	HeldCount int
	Total     int
	Reserved  int
	// DockerUnknown means the container cross-check could not run, so an
	// empty Unwrapped list must not be read as "no suite is running".
	DockerUnknown bool
	// Unwrapped lists container suites running without holding a slot.
	Unwrapped []string
	// Busy is true when no new suite could be admitted right now: every
	// slot held, an unwrapped suite occupying the Docker VM, or the
	// container check unverifiable.
	Busy bool
}

// GateSlotRow is one pool slot. State is "free", "held", or "stale" — stale
// meaning the flock is held but the recorded owner's PID is gone, which a
// bare flock probe cannot tell apart from a live holder.
type GateSlotRow struct {
	Index    int
	Reserved bool
	State    string
	Role     string
	PID      int
	Since    string // local time the holder acquired the slot
	Age      string // short hold age, e.g. "12m"
}

// GateRoleRow is one gate role class and what it is doing with the pool.
type GateRoleRow struct {
	Class  string
	Label  string
	State  string // free, held, stale
	Detail string
}

// processAlive reports whether a PID is still running. Var so tests can pin
// liveness without depending on real processes.
var processAlive = processAliveOS

// gateSlotReport reads the town's container-gate pool state. It is a var so
// tests can drive the panel from a fabricated report instead of a real lock
// directory (gt-1ln0).
var gateSlotReport = func(townRoot string) (slot.Report, error) {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	return slot.StatusPool(townRoot, slot.Pool{Slots: cg.SlotsV(), ReservedForGate: cg.ReservedForGateV()})
}

// FetchGate reads the container-gate pool for the dashboard's Gate panel.
// It spawns no bd subprocess, unlike the panels backed by runBdCmd — but the
// panel does render the container half (DockerUnknown / Unwrapped), and that
// half is only knowable from a `docker ps` cross-check, so a report with no
// slot held costs one bounded (dockerPSTimeout) docker call per poll tick.
// Callers that need only the held/owner picture want StatusPoolLocksOnly
// instead (gt-a8kx).
func (f *LiveConvoyFetcher) FetchGate() (*GateStatus, error) {
	rep, err := gateSlotReport(f.townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading container-gate pool: %w", err)
	}
	return buildGateStatus(rep), nil
}

// buildGateStatus renders a pool report into the panel's rows.
func buildGateStatus(rep slot.Report) *GateStatus {
	gs := &GateStatus{
		HeldCount:     rep.HeldCount,
		Total:         rep.Total,
		Reserved:      rep.Reserved,
		DockerUnknown: rep.DockerUnknown,
		Unwrapped:     rep.UnwrappedContainers,
		Busy:          rep.Busy(),
	}

	// holders[class] holds every slot that class holds (polecats especially
	// can hold several); unowned holds are kept apart because a missing owner
	// file means the role is unknown, not that a polecat owns the slot.
	holders := map[string][]GateSlotRow{}
	var unowned []GateSlotRow

	for _, st := range rep.Slots {
		row := GateSlotRow{Index: st.Index, Reserved: st.Index < rep.Reserved, State: "free"}
		if st.Held {
			row.State = "held"
			if st.Owner != nil {
				row.Role = st.Owner.Role
				row.PID = st.Owner.PID
				if !st.Owner.AcquiredAt.IsZero() {
					row.Since = formatTimestamp(st.Owner.AcquiredAt)
					row.Age = activity.Calculate(st.Owner.AcquiredAt).FormattedAge
				}
				if st.Owner.PID > 0 && !processAlive(st.Owner.PID) {
					row.State = "stale"
				}
			}
			if row.Role == "" {
				unowned = append(unowned, row)
			} else {
				class := gateRoleClass(row.Role)
				holders[class] = append(holders[class], row)
			}
		}
		gs.Slots = append(gs.Slots, row)
	}

	// The class list is fixed until om-review markers exist: the om-review
	// row appears only for a holder that names it.
	for _, class := range []string{gateClassRefinery, gateClassBatch, gateClassMainTest} {
		gs.Roles = append(gs.Roles, gateRoleRow(class, holders[class]))
	}
	if rows := holders[gateClassOMReview]; len(rows) > 0 {
		gs.Roles = append(gs.Roles, gateRoleRow(gateClassOMReview, rows))
	}
	gs.Roles = append(gs.Roles, gateRoleRow(gateClassPolecats, holders[gateClassPolecats]))

	gs.Summaries = gateSummaries(holders, unowned, rep)
	return gs
}

// gateRoleClass maps a holder's role string to its panel class. Order
// matters: "<rig>/refinery-batch" contains "/refinery".
func gateRoleClass(role string) string {
	switch {
	case strings.HasSuffix(role, "/"+gateClassBatch):
		return gateClassBatch
	case strings.HasSuffix(role, "/"+gateClassMainTest):
		return gateClassMainTest
	case strings.HasSuffix(role, "/"+gateClassOMReview):
		return gateClassOMReview
	case strings.Contains(role, "/"+gateClassRefinery):
		return gateClassRefinery
	default:
		return gateClassPolecats
	}
}

// gateRoleRow summarizes one class: free, or what it holds.
func gateRoleRow(class string, rows []GateSlotRow) GateRoleRow {
	row := GateRoleRow{Class: class, State: "free", Detail: "free"}
	switch class {
	case gateClassRefinery:
		row.Label = "Refinery gate"
	case gateClassBatch:
		row.Label = "Batch gate"
	case gateClassMainTest:
		row.Label = "Main-branch test"
	case gateClassOMReview:
		row.Label = "om review"
	default:
		row.Label = "Polecat suites"
	}
	if len(rows) == 0 {
		return row
	}

	row.State = "held"
	var details []string
	for _, r := range rows {
		if r.State == "stale" {
			row.State = "stale"
		}
		details = append(details, gateHoldDetail(class, r))
	}
	row.Detail = strings.Join(details, "; ")
	return row
}

// gateHoldDetail is the class's own phrasing for one held slot.
func gateHoldDetail(class string, r GateSlotRow) string {
	if r.State == "stale" {
		return fmt.Sprintf("stale hold on slot %d (pid %d is gone)", r.Index, r.PID)
	}
	if class == gateClassRefinery {
		return fmt.Sprintf("gating %s since %s", gateHolderLabel(r.Role), r.Age)
	}
	return fmt.Sprintf("holding slot %d since %s", r.Index, r.Age)
}

// gateSummaryName is the class's name in a summary line. The batch gate
// reads as "batch run" because that is what it is, not a second refinery.
func gateSummaryName(class string) string {
	if class == gateClassBatch {
		return "batch run"
	}
	return class
}

// gateHolderLabel is the role string for display, standing in for a holder
// that wrote no owner metadata.
func gateHolderLabel(role string) string {
	if role == "" {
		return "unknown role"
	}
	return role
}

// gateSummaries renders the panel's one-line headlines, one per held slot
// plus a line for the free/unknown cases.
func gateSummaries(holders map[string][]GateSlotRow, unowned []GateSlotRow, rep slot.Report) []string {
	var lines []string
	for _, class := range []string{gateClassRefinery, gateClassBatch, gateClassMainTest, gateClassOMReview} {
		for _, r := range holders[class] {
			switch {
			case r.State == "stale":
				lines = append(lines, fmt.Sprintf("%s: stale hold on slot %d (owner pid %d is gone)",
					gateSummaryName(class), r.Index, r.PID))
			case class == gateClassRefinery:
				lines = append(lines, fmt.Sprintf("refinery: gating %s since %s", gateHolderLabel(r.Role), r.Age))
			default:
				lines = append(lines, fmt.Sprintf("%s holding slot %d", gateSummaryName(class), r.Index))
			}
		}
	}
	for _, r := range unowned {
		lines = append(lines, fmt.Sprintf("slot %d: held with no owner metadata (role unknown)", r.Index))
	}
	if rows := holders[gateClassPolecats]; len(rows) > 0 {
		var slots []string
		for _, r := range rows {
			slots = append(slots, fmt.Sprintf("%d", r.Index))
		}
		lines = append(lines, fmt.Sprintf("polecats: %d suite(s) holding slot(s) %s", len(rows), strings.Join(slots, ", ")))
	}

	switch {
	case len(rep.UnwrappedContainers) > 0:
		lines = append(lines, fmt.Sprintf("unwrapped container suite running without a slot: %s",
			strings.Join(rep.UnwrappedContainers, ", ")))
	case rep.HeldCount == 0 && rep.DockerUnknown:
		lines = append(lines, "docker unreachable — cannot verify no suite is running")
	case rep.HeldCount == 0:
		lines = append(lines, "no container-backed suite running")
	}
	return lines
}
