package dashboard

// The System updates section is the feed's join of landed commits (rows, from
// system_updates.go) and the Now block, which names what runs at this moment: a
// gt update roll, a tier sweep, or both. The poll reads the update record and
// the tier sweep's running record itself, so the block names the same moment as
// the section it rides on.

// SystemUpdates is the feed's System updates section. Rows are the newest landed
// commits, newest first. Now names the roll or sweep running at the poll's
// instant and is nil when neither runs. Unavailable says a source the rows join
// could not be read, so a row missing its sweep or roll is not taken for a
// commit that never reached that stage.
type SystemUpdates struct {
	Now  *SystemUpdatesNow `json:"now,omitempty"`
	Rows []SystemUpdateRow `json:"rows"`
	// More is how many joined commits the row cap left out. It counts only the
	// commits the join knows (sweeps, pipeline runs, rolls), so the pane says
	// 'older not shown', never 'undeployed' (gt-71xpmh). Omitted at zero.
	More        int  `json:"more,omitempty"`
	Unavailable bool `json:"unavailable,omitempty"`
	// RollUnknown and SweepUnknown say the source behind one half of Now could
	// not be read, so a missing half is a reading that failed, never a town with
	// nothing running.
	RollUnknown  bool `json:"roll_unknown,omitempty"`
	SweepUnknown bool `json:"sweep_unknown,omitempty"`
	// RunsUnknown says the gastown pipeline read failed and held no earlier
	// good runs, so the rows' post-land and images stages are readings that
	// failed, never commits with no runs. Last good runs are shown, not flagged.
	RunsUnknown bool `json:"runs_unknown,omitempty"`
	// Sweeps are the gastown rig's newest sweeps, newest first, at most five.
	Sweeps []TierSweepRow `json:"sweeps,omitempty"`
	// LastGreen is the newest gastown sweep that was all GREEN, found among every
	// sweep the reader holds, not only the five Sweeps draws. Nil when none is.
	LastGreen *SweepGreen `json:"last_green,omitempty"`
}

// SystemUpdatesNow is what runs right now: a roll, a sweep, or both.
type SystemUpdatesNow struct {
	Roll  *NowRoll  `json:"roll,omitempty"`
	Sweep *NowSweep `json:"sweep,omitempty"`
}

// NowRoll is the gt update run in flight: its run id, the step it is on, its
// wall time so far and the builds it moves between, as the update section
// reports them off the run's target.
type NowRoll struct {
	RunID      string `json:"run_id"`
	Step       string `json:"step"`
	ElapsedSec int    `json:"elapsed_sec"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

// NowSweep is the tier sweep in flight: the rig it sweeps, the commit it sweeps,
// the tier it is at, and its wall time so far, as the tier sweep's running record
// reports them. SHA is empty when the record names no commit.
type NowSweep struct {
	Rig        string `json:"rig"`
	SHA        string `json:"sha,omitempty"`
	Tier       string `json:"tier"`
	ElapsedSec int64  `json:"elapsed_sec"`
}

// NewSystemUpdates builds the section from the sweeps, pipeline runs and rolls
// the readers read. The row cap is the section's own, so a caller never picks
// it. A nil pipeline joins no runs into the rows. Sweeps and LastGreen read only
// the gastown rig's sweeps.
func NewSystemUpdates(sweeps []TierSweepRow, pipeline *GastownPipeline, rolls []SystemUpdateRoll, unavailable bool) *SystemUpdates {
	rows, more := SystemUpdateRowsWithMore(sweeps, pipeline, rolls, systemUpdatesRowCap)
	rig := rigSweeps(sweeps)
	lastGreen := lastGreenSweep(rig)
	if len(rig) > systemUpdatesSweepCap {
		rig = rig[:systemUpdatesSweepCap]
	}
	return &SystemUpdates{
		Rows:        rows,
		More:        more,
		Unavailable: unavailable,
		Sweeps:      rig,
		LastGreen:   lastGreen,
	}
}

// systemUpdatesNow names the roll and the sweep running right now. A roll counts
// while the update record says it is running, and a sweep while the tier sweep
// holds a running record. Nil when neither runs.
func systemUpdatesNow(u *UpdateStatus, ts *TierSweep) *SystemUpdatesNow {
	var now SystemUpdatesNow
	if u != nil && u.Running {
		now.Roll = &NowRoll{RunID: u.RunID, Step: u.Current, ElapsedSec: u.ElapsedSec, From: u.From, To: u.To}
	}
	if ts != nil && ts.Running != nil {
		now.Sweep = &NowSweep{Rig: ts.Running.Rig, SHA: ts.Running.SHA, Tier: ts.Running.Tier, ElapsedSec: ts.Running.ElapsedSec}
	}
	if now.Roll == nil && now.Sweep == nil {
		return nil
	}
	return &now
}

// pollSystemUpdates reads the section into the feed. A nil read keeps the last
// section, the way pollTierSweep does. The update record and the tier sweep are
// read at this poll's own instant, so the Now block names the same moment as the
// section rather than whatever an older poll left in the feed. A sweep source
// that is missing or unreadable marks its half unknown, never idle.
func (h *Hub) pollSystemUpdates() {
	su := h.cfg.SystemUpdates()
	if su == nil {
		return
	}
	var update *UpdateStatus
	if h.cfg.Update != nil {
		update = h.cfg.Update()
	}
	var sweep *TierSweep
	if h.cfg.TierSweep != nil {
		sweep = h.cfg.TierSweep()
	}
	su.Now = systemUpdatesNow(update, sweep)
	su.SweepUnknown = sweep == nil || sweep.Unavailable
	h.mu.Lock()
	defer h.mu.Unlock()
	// The pair state joins the rows with the attention items, which are the
	// hub's own read rather than the section reader's (gt-clorl0). queueKnown
	// says those items are a reading; until then a row is unknown, never absent.
	classifySystemUpdatePairs(su.Rows, h.queueItems, h.queueKnown)
	h.state.SystemUpdates = su
	h.publishLocked()
}
