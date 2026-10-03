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

// Polecat states, in the order the page sorts them.
const (
	StateWorking    = "working"     // holds a bead and its session is producing output
	StateGating     = "gating"      // its bead is being gated and reviewed by the landing worker
	StateQueued     = "queued"      // its bead is submitted and waits to land
	StateNeedsHuman = "needs-human" // its bead is waiting on an operator
	StateQuiet      = "quiet"       // holds a bead; its session has been silent past QuietAfter
	StateStale      = "stale"       // holds a bead but has no session: a hook nothing is working
	StateIdle       = "idle"        // holds nothing
)

// QuietAfter is how long a working session may stay silent before it reads as quiet.
const QuietAfter = 10 * time.Minute

// Polecat is one polecat: what it holds, what state that is in, and its record.
type Polecat struct {
	Rig   string `json:"rig"`
	Name  string `json:"name"`
	Bead  string `json:"bead,omitempty"`
	Title string `json:"title,omitempty"`
	State string `json:"state"`

	Priority   *int     `json:"priority,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	BeadStatus string   `json:"bead_status,omitempty"` // the held bead's status; "closed" on a hook that should have been cleared

	Slung      *time.Time `json:"slung,omitempty"` // when a dispatch line named the bead
	ElapsedSec int64      `json:"elapsed_sec,omitempty"`
	HasSession bool       `json:"has_session"`
	LastActive *time.Time `json:"last_active,omitempty"`
	QuietSec   int64      `json:"quiet_sec,omitempty"`
	AlsoHeldBy []string   `json:"also_held_by,omitempty"` // other polecats holding the same bead

	Landed24h   int      `json:"landed_24h"`
	Approved24h int      `json:"approved_24h"`
	AvgScore24h *float64 `json:"avg_score_24h,omitempty"`
}

// Summary is the slow-changing state of the town. A pointer or zero field the
// reader could not fill is left out of the page rather than shown as zero.
type Summary struct {
	SeatsUsed *int `json:"seats_used,omitempty"`
	SeatsCap  *int `json:"seats_cap,omitempty"`
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

	Now func() time.Time

	FeedEvery    time.Duration
	SummaryEvery time.Duration
	HealthEvery  time.Duration
	MachineEvery time.Duration
	SpendEvery   time.Duration
	OMEvery      time.Duration

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
	if c.RingSize <= 0 {
		c.RingSize = 500
	}
}
