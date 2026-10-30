package beads

// CensusBudget is the most bd calls per minute one caller may average before
// the census gate fails (gt-gojo0).
type CensusBudget struct {
	Caller         string
	CallsPerMinute float64
	// Target marks a caller another hot-path bead is cutting: the row is that
	// bead's target rate, so the gate holds the cut once the bead lands.
	Target bool
}

// censusCallerBudgets is the census gate's data: the sixteen busiest callers
// in the 60 one-minute windows ending 2026-10-30 12:36 CDT (58.2 calls per
// minute, 3492 in all). A caller the census names and this table does not is
// reported unbudgeted, and raising a row is an edit here that om reviews.
//
// The first nine rows are the baseline's ranking, each at its own average. A
// row marked Target is a caller another hot-path bead is cutting, so its value
// is 1.5 times that bead's target rate and never above the baseline it comes
// from: the gate holds the cut without charging the baseline twice.
//
// Below the ninth row the baseline's own ranking stops, so the rows under it
// hold the other bd callers these processes make at that ninth row's 1.0 a
// minute: a caller ranked below the ninth cannot average more than it, so a
// row here is a ceiling the ranking itself sets, not a guess at its rank.
var censusCallerBudgets = []CensusBudget{
	// Baseline 762 (12.7/min), one ready-to-land list per rig per metrics
	// sample. Target: the count the landing worker published, with bd only as
	// a read older than 3 minutes (gt-hw6ih).
	{Caller: "metrics.queueLandingColumn", CallsPerMinute: 3.0, Target: true},
	// Baseline 685 (11.5/min), the landing worker's re-read of every held
	// bead. Target: only a bead whose row moved since its last outcome
	// (gt-r9pj9).
	{Caller: "landworker.(*RetryBeads).Show", CallsPerMinute: 3.0, Target: true},
	// Baseline 414 (6.9/min), the per-rig poll of ready-to-land, which stays
	// (gt-r9pj9).
	{Caller: "landworker.(*RetryBeads).List", CallsPerMinute: 6.9},
	// Baseline 462 (7.7/min), each rig's identity bead once a minute. Target:
	// once per rig per five minutes, with gt rig dock and undock invalidating
	// it at once (gt-z0dcj).
	{Caller: "daemon.showRigBead", CallsPerMinute: 2.4, Target: true},
	// Baseline 198 (3.3/min), the held beads' children. Target: only a bead
	// whose row moved (gt-r9pj9).
	{Caller: "landworker.(*RetryBeads).Children", CallsPerMinute: 1.1, Target: true},
	// Baseline 207 (3.5/min), the sling contexts read three times a cycle.
	// Target: one read per cycle (gt-pjcpb).
	{Caller: "schedulerrun.listAllSlingContextRecords", CallsPerMinute: 1.8, Target: true},
	// Baseline 128 (2.2/min), the held beads' comments. Target: only a bead
	// whose row moved (gt-r9pj9).
	{Caller: "landworker.(*RetryBeads).Comments", CallsPerMinute: 0.8, Target: true},
	// Baseline 84 (1.4/min), one wisp list per plugin per tick. Target: one
	// list per tick for every plugin (gt-3bqfy).
	{Caller: "plugin.(*Recorder).queryRuns", CallsPerMinute: 0.5, Target: true},
	// Baseline 60 (1.0/min), the refusal annotation posted again each pass.
	// Target: only a refusal that was not annotated yet (gt-r9pj9).
	{Caller: "landworker.(*RetryBeads).AddComment", CallsPerMinute: 0.5, Target: true},
	// The ready note on each landed bead, so a landing is one call.
	{Caller: "landworker.(*RetryBeads).AppendNotes", CallsPerMinute: 1.0},
	// The label write on each landing, and the rework label's removal.
	{Caller: "landworker.(*RetryBeads).Update", CallsPerMinute: 1.0},
	// The close that ends each landing.
	{Caller: "landworker.(*RetryBeads).CloseWithReason", CallsPerMinute: 1.0},
	// One list of a scheduled sling's run beads per entry per pass.
	{Caller: "daemon.(*execScheduledSlingRunner).listBeads", CallsPerMinute: 1.0},
	// A revert bead, created when a landing is reverted rather than landed.
	{Caller: "landworker.(*RetryBeads).Create", CallsPerMinute: 1.0},
	// The receipt a plugin run writes when it records, one per run.
	{Caller: "plugin.(*Recorder).RecordRun", CallsPerMinute: 1.0},
	// The run bead a scheduled sling creates when its entry is due.
	{Caller: "daemon.(*execScheduledSlingRunner).createBead", CallsPerMinute: 1.0},
}

// CensusBudgets returns the allowed average bd calls per minute for each
// caller the census baseline named.
func CensusBudgets() []CensusBudget {
	return append([]CensusBudget(nil), censusCallerBudgets...)
}
