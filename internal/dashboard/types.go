// Package dashboard serves a read-only, localhost-only page that shows the
// town the way gt tail -f does: from the files and readers the town already
// has, polled only while someone is watching.
//
// The old web dashboard forked a dozen bd, tmux, gh and git children per
// browser tab every 30 seconds. This one has one hub. The hub polls its
// readers on its own clock, keeps the result, and pushes it to every open page
// over server-sent events, so a second tab costs nothing. When no page is open
// the hub polls nothing at all.
package dashboard

import (
	"encoding/json"
	"time"
)

// Entry is one line of the feed: what gt tail's default view would print.
type Entry struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Rig   string    `json:"rig"`
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	Class string    `json:"class"` // failure, warning, success, landing, dispatch, restart, plain
}

// Health is the town's one health line and its verdict.
type Health struct {
	Line    string    `json:"line"`
	Verdict string    `json:"verdict"` // green, degraded, red, unknown
	ReadAt  time.Time `json:"read_at"`
}

// Polecat states. They are the town's own inventory states (what gt polecat
// list shows and the dispatcher's capacity counts), plus gating and quiet,
// which only the dashboard can see.
const (
	StateWorking      = "working"       // assigned work and a live session
	StateQuiet        = "quiet"         // working, but the session has been silent past QuietAfter
	StateGating       = "gating"        // its bead is being gated and om-reviewed by the landing worker
	StateQueued       = "queued"        // its work is submitted and waits to land
	StateSpawning     = "spawning"      // dispatched seconds ago, session not up yet
	StateStalled      = "stalled"       // assigned work but no session: nothing is doing it
	StateReviewNeeded = "review-needed" // a live session holding uncommitted or unpushed state
	StateRecovery     = "recovery"      // idle, but its worktree needs recovery before reuse
	StateNeedsHuman   = "needs-human"   // its bead is waiting on an operator
	StateParked       = "parked"        // parked by an operator
	StateIdle         = "idle"
	// StateUnknown is a polecat the dashboard cannot classify because tmux could
	// not be read: without the session list "working" and "stalled" look alike.
	StateUnknown = "unknown"
)

// QuietAfter is how long a working session may stay silent before it reads as quiet.
const QuietAfter = 10 * time.Minute

// Polecat is one polecat: what it holds, what state that is in, and its record.
type Polecat struct {
	Rig    string `json:"rig"`
	Name   string `json:"name"`
	Bead   string `json:"bead,omitempty"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"` // why the town's inventory put it in this state
	// CountsTowardCapacity is the dispatcher's own accounting: this polecat
	// occupies one of the roster's seats.
	CountsTowardCapacity bool `json:"counts_toward_capacity"`

	Priority *int     `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`

	Slung      *time.Time `json:"slung,omitempty"` // when a dispatch line named the bead
	ElapsedSec int64      `json:"elapsed_sec,omitempty"`
	HasSession bool       `json:"has_session"`
	LastActive *time.Time `json:"last_active,omitempty"`
	QuietSec   int64      `json:"quiet_sec,omitempty"`
	// RigParked marks a polecat whose rig is parked. A parked rig is stood down
	// on purpose: its polecats are not dispatch targets, and a stalled one is
	// the expected condition, not an alarm.
	RigParked bool `json:"rig_parked,omitempty"`

	// Hints are read-only commands an operator would run to look into this
	// polecat. The page shows them with a copy button and never runs them.
	Hints []string `json:"hints,omitempty"`

	Landed24h   int      `json:"landed_24h"`
	Approved24h int      `json:"approved_24h"`
	AvgScore24h *float64 `json:"avg_score_24h,omitempty"`
}

// Summary is the slow-changing state of the town. A pointer or zero field the
// reader could not fill is left out of the page rather than shown as zero.
type Summary struct {
	SeatsCap *int `json:"seats_cap,omitempty"`
	// Polecats is the summary reader's picture; the hub adds the live parts
	// (dispatch time, silence, the gating phase) before the page sees it.
	Polecats []Polecat `json:"-"`

	ReadyToLand *int       `json:"ready_to_land,omitempty"`
	OldestReady *time.Time `json:"oldest_ready,omitempty"`

	MainTip     string `json:"main_tip,omitempty"`
	InstalledGT string `json:"installed_gt,omitempty"`
	Behind      *int   `json:"behind,omitempty"`

	Escalations *int `json:"escalations,omitempty"`

	MedianDeployMin *float64 `json:"median_deploy_min,omitempty"`
	DeployWaiting   *int     `json:"deploy_waiting,omitempty"`
	OldestDeploySec *int64   `json:"oldest_deploy_waiting_sec,omitempty"`
}

// Proc is one busy process.
type Proc struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"`
}

// Machine is the host's load and what is burning it.
type Machine struct {
	Load1  float64   `json:"load1"`
	Load5  float64   `json:"load5"`
	Load15 float64   `json:"load15"`
	Top    []Proc    `json:"top"`
	At     time.Time `json:"at"`
}

// LoadPoint is one load sample for the sparkline.
type LoadPoint struct {
	At   time.Time `json:"at"`
	Load float64   `json:"load"`
}

// GatePoint is one landing gate's wall time and the load when it finished.
type GatePoint struct {
	At   time.Time `json:"at"`
	Secs float64   `json:"secs"`
	Load *float64  `json:"load,omitempty"` // nil when no sample covers the gate
	Text string    `json:"text"`
}

// OMWindow is the om review's numbers over one span of time.
type OMWindow struct {
	Label      string   `json:"label"`
	Landed     int      `json:"landed"`
	Approved   int      `json:"approved"`
	Skipped    int      `json:"skipped"` // landed without a review (an operator route)
	Errors     int      `json:"errors"`  // review did not run or failed
	Rejected   int      `json:"rejected"`
	AvgScore   *float64 `json:"avg_score,omitempty"`
	MedianSecs *float64 `json:"median_secs,omitempty"`
	P95Secs    *float64 `json:"p95_secs,omitempty"`
}

// OMDay is one local day of landings and rejections.
type OMDay struct {
	Day      string   `json:"day"`
	Approved int      `json:"approved"`
	Skipped  int      `json:"skipped"`
	Errors   int      `json:"errors"`
	Rejected int      `json:"rejected"`
	AvgSecs  *float64 `json:"avg_secs,omitempty"`
}

// OMReview is one landing or rejection the review loop decided.
type OMReview struct {
	At      time.Time `json:"at"`
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig,omitempty"`
	Outcome string    `json:"outcome"`        // approved, skipped, error, rejected
	Kind    string    `json:"kind,omitempty"` // for a rejection: review, gate, conflict, policy, empty
	Score   *float64  `json:"score,omitempty"`
	Secs    *float64  `json:"secs,omitempty"` // om's own wall time
	Route   string    `json:"route,omitempty"`
	Gate    string    `json:"gate,omitempty"`
	Risk    bool      `json:"risk,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// OM is the reviewer's record, read from the landings files and the daemon log.
type OM struct {
	Backend   string         `json:"backend,omitempty"`
	Depth     string         `json:"depth,omitempty"`
	Threshold *float64       `json:"threshold,omitempty"`
	TimeoutS  int            `json:"timeout_s,omitempty"`
	Since     time.Time      `json:"since"`
	Windows   []OMWindow     `json:"windows"`
	Days      []OMDay        `json:"days"`
	Scores    [10]int        `json:"scores"` // decile counts: approvals and review rejections
	Rejects   map[string]int `json:"rejects"`
	Routes    map[string]int `json:"routes"`
	RiskPaths int            `json:"risk_paths"` // landings that touched risk paths
	Recent    []OMReview     `json:"recent"`
}

// SkippedBead is a bead the dispatcher considered and did not sling, with the
// reason it logged.
type SkippedBead struct {
	Bead   string `json:"bead"`
	Reason string `json:"reason"`
}

// Dispatch is the spec dispatcher's most recent tick.
type Dispatch struct {
	At         time.Time     `json:"at"`
	Candidates int           `json:"candidates"`
	Roster     string        `json:"roster"` // "deepseek-flash 2/3"
	Dispatched int           `json:"dispatched"`
	Refused    int           `json:"refused"`
	Planning   int           `json:"planning"`
	Skipped    int           `json:"skipped"`
	Failed     int           `json:"failed"`
	Held       int           `json:"held"` // held by the failed label
	Named      []SkippedBead `json:"named,omitempty"`
	More       int           `json:"more,omitempty"` // skipped beads the line did not name
}

// TrendHour is one local hour of the last 24: how many landings and rejections
// fell in it, and the host load seen while it ran. LoadAvg and LoadMax are nil
// for an hour no load sample covers.
type TrendHour struct {
	Hour     time.Time `json:"hour"`
	Landed   int       `json:"landed"`
	Rejected int       `json:"rejected"`
	LoadAvg  *float64  `json:"load_avg,omitempty"`
	LoadMax  *float64  `json:"load_max,omitempty"`
}

// TrendPoint is one landing's stage times, off its "stages: lint Ns, gate Ns,
// om Ns" line. OMSecs is nil when the review did not run.
type TrendPoint struct {
	At       time.Time `json:"at"`
	LintSecs float64   `json:"lint_secs"`
	GateSecs float64   `json:"gate_secs"`
	OMSecs   *float64  `json:"om_secs,omitempty"`
}

// LandingRow is one recent landing or rejection, with what it cost: the time
// each landing stage took, and om's verdict and score. The page's Landings
// table is made of these.
type LandingRow struct {
	At      time.Time `json:"at"`
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig,omitempty"`
	Title   string    `json:"title,omitempty"`
	Polecat string    `json:"polecat,omitempty"`
	// Outcome is "landed" or "rejected". Verdict is om's reading of a landing:
	// "approved", "skipped" (landed with no review of its own) or "error". Kind
	// is why a rejection was refused: review, gate, conflict, policy or empty.
	Outcome  string   `json:"outcome"`
	Verdict  string   `json:"verdict,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Score    *float64 `json:"score,omitempty"`
	LintSecs *float64 `json:"lint_secs,omitempty"`
	GateSecs *float64 `json:"gate_secs,omitempty"`
	OMSecs   *float64 `json:"om_secs,omitempty"`
	Commit   string   `json:"commit,omitempty"`
	Route    string   `json:"route,omitempty"`
	Risk     bool     `json:"risk,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// Trend is the last 24 hours as the page draws it: landings and rejections by
// local hour, the newest landings' stage times, and the host load per hour.
type Trend struct {
	Hours  []TrendHour  `json:"hours"`
	Stages []TrendPoint `json:"stages,omitempty"`
	// Recent is the newest landings and rejections, newest first.
	Recent []LandingRow `json:"recent,omitempty"`
}

// State is everything the page draws apart from the feed.
type State struct {
	Now       time.Time       `json:"now"`
	Viewers   int             `json:"viewers"`
	Health    Health          `json:"health"`
	Summary   *Summary        `json:"summary,omitempty"`
	SummaryAt time.Time       `json:"summary_at,omitempty"`
	Polecats  []Polecat       `json:"polecats"`
	Machine   Machine         `json:"machine"`
	Loads     []LoadPoint     `json:"loads"`
	Gates     []GatePoint     `json:"gates"`
	Spend     json.RawMessage `json:"spend,omitempty"`
	OM        *OM             `json:"om,omitempty"`
	Dispatch  *Dispatch       `json:"dispatch,omitempty"`
	Queue     *Queue          `json:"queue,omitempty"`
	Trend     *Trend          `json:"trend,omitempty"`
	// Alerts is the most recent alerts the alerter raised, newest last, capped
	// at alertsKept. The page lists them whether or not alerts are switched on.
	Alerts []Alert `json:"alerts,omitempty"`
}

// Config wires the hub to its readers. Every reader is optional; a nil reader
// leaves its part of the page empty. Readers are called from the hub's own
// goroutines, one call at a time per reader.
type Config struct {
	// Feed returns the entries that appeared since its last call.
	Feed func() []Entry
	// Summary reads the town's slow state (it may run bd).
	Summary func() Summary
	// Health reads the daemon's health report from disk.
	Health func() Health
	// Machine samples load and the busiest processes.
	Machine func() (Machine, error)
	// Spend returns the DeepSeek spend report as JSON, nil when unavailable.
	Spend func() json.RawMessage
	// OM reads the reviewer's record from disk.
	OM func() *OM
	// Dispatch reads the spec dispatcher's last tick from the daemon log.
	Dispatch func() *Dispatch
	// Queue reads the work queue lists; Bead reads one bead's text on request.
	Queue func() *Queue
	Bead  func(rig, id string) (*BeadDetail, error)
	// Trend reads the last 24 hours of landings, rejections, stage times and
	// host load.
	Trend func() *Trend
	// LoadSample records one machine sample for the trend's load history. The
	// hub calls it from its machine poll, which runs only while a page is open.
	LoadSample func(at time.Time, load float64)

	Now func() time.Time

	FeedEvery     time.Duration
	SummaryEvery  time.Duration
	HealthEvery   time.Duration
	MachineEvery  time.Duration
	SpendEvery    time.Duration
	OMEvery       time.Duration
	DispatchEvery time.Duration
	QueueEvery    time.Duration
	TrendEvery    time.Duration

	RingSize int // feed entries kept for a page that connects late
}

func (c *Config) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.FeedEvery, 3*time.Second)
	def(&c.SummaryEvery, 60*time.Second)
	def(&c.HealthEvery, 5*time.Second)
	def(&c.MachineEvery, 10*time.Second)
	def(&c.SpendEvery, 5*time.Minute)
	def(&c.OMEvery, 60*time.Second)
	def(&c.DispatchEvery, 10*time.Second)
	def(&c.QueueEvery, 60*time.Second)
	def(&c.TrendEvery, 60*time.Second)
	if c.RingSize <= 0 {
		c.RingSize = 500
	}
}
