package beads

// CensusBudget is one row of the census gate's data: how many bd calls per
// minute a caller may average before the check fails (gt-gojo0).
type CensusBudget struct {
	Caller         string
	CallsPerMinute float64
}

// censusCallerBudgets is the census gate's data: the callers the census ranked
// highest in its 60-window baseline and each one's allowed average calls per
// minute (gt-gojo0). It is data rather than code on purpose: raising a budget
// is an edit here that om reviews.
//
// A caller a hot-path bead cuts is budgeted at its baseline rate, so the gate
// fails on any growth and never on the rate it measured; its budget is
// tightened to the landed rate times 1.25 by a later edit, made against a
// post-install census and not against a target in a bead. A caller no bead
// cuts is budgeted at its baseline rate times 1.25, rounded up to the next
// hundredth: its rate is what it will keep. Each row's comment names its
// cutting bead, and the test that reads testdata/census-baseline-20261030.txt
// pins both rules.
//
// Rows are in baseline order. The first nine carry the rate the baseline
// recorded for that caller (a cut caller's row is that rate; List, which no bead
// cuts, carries 1.25 times it). A census window logs only its eight busiest rows,
// so a caller fast enough to rank was measured and a slower one was not: the
// rows below the ninth sit at 1.0 a minute, the rate every caller the log left
// out is bounded by. The split and the rounding are pinned by the test that
// reads testdata/census-baseline-20261030.txt.
var censusCallerBudgets = []CensusBudget{
	// Baseline 762 calls, 12.7 a minute: one ready-to-land list per rig per
	// metrics sample (gt-hw6ih moves the column onto the landing worker's
	// published count).
	{Caller: "metrics.queueLandingColumn", CallsPerMinute: 12.70},
	// Baseline 685 calls, 11.4 a minute: the landing worker re-reading every
	// held bead each pass (gt-r9pj9).
	{Caller: "landworker.RetryBeads.Show", CallsPerMinute: 11.40},
	// Baseline 462 calls, 7.7 a minute: each rig's identity bead, once a
	// minute (gt-z0dcj).
	{Caller: "daemon.(*Daemon).showRigBead", CallsPerMinute: 7.70},
	// Baseline 414 calls, 6.9 a minute: the per-rig poll of ready-to-land,
	// which stays (gt-r9pj9 leaves it alone).
	{Caller: "landworker.RetryBeads.List", CallsPerMinute: 8.63},
	// Baseline 207 calls, 3.5 a minute in bursts: the sling contexts read
	// three times a dispatch cycle (gt-pjcpb).
	{Caller: "schedulerrun.listAllSlingContextRecords", CallsPerMinute: 3.50},
	// Baseline 198 calls, 3.3 a minute: the held beads' children (gt-r9pj9).
	{Caller: "landworker.RetryBeads.Children", CallsPerMinute: 3.30},
	// Baseline 128 calls, 2.1 a minute: the held beads' comments (gt-r9pj9).
	{Caller: "landworker.RetryBeads.Comments", CallsPerMinute: 2.10},
	// Baseline 84 calls, 1.4 a minute: one plugin-run list per plugin per
	// tick (gt-3bqfy).
	{Caller: "plugin.(*Recorder).queryRuns", CallsPerMinute: 1.40},
	// Baseline 60 calls, 1.0 a minute: the refusal annotation posted again
	// each pass (gt-r9pj9).
	{Caller: "landworker.RetryBeads.AddComment", CallsPerMinute: 1.00},
	// The ready note each landing writes, one call per landed bead.
	{Caller: "landworker.RetryBeads.AppendNotes", CallsPerMinute: 1.25},
	// The label write a landing makes, and the rework label's removal.
	{Caller: "landworker.RetryBeads.Update", CallsPerMinute: 1.25},
	// The close that ends a landing.
	{Caller: "landworker.RetryBeads.CloseWithReason", CallsPerMinute: 1.25},
	// A revert bead, created when a landing is reverted rather than landed.
	{Caller: "landworker.RetryBeads.Create", CallsPerMinute: 1.25},
	// A scheduled sling's own run beads, one list per entry per pass.
	{Caller: "daemon.(*execScheduledSlingRunner).listBeads", CallsPerMinute: 1.25},
	// The run bead a scheduled sling creates when its entry is due.
	{Caller: "daemon.(*execScheduledSlingRunner).createBead", CallsPerMinute: 1.25},
	// The receipt a plugin run writes, one per run.
	{Caller: "plugin.(*Recorder).RecordRun", CallsPerMinute: 1.25},
}

// CensusBudgets returns the allowed average bd calls per minute for each
// caller the census baseline named.
func CensusBudgets() []CensusBudget {
	return append([]CensusBudget(nil), censusCallerBudgets...)
}
