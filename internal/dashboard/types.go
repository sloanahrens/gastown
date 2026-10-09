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
	// Causes are the report's non-green fields, worst first, capped: the
	// verdict's why without a gt status --line run (gt-70aa6).
	Causes []HealthCause `json:"causes,omitempty"`
}

// HealthCause is one field that held the verdict back: the townhealth field's
// identity, plus the phrase that explains it.
type HealthCause struct {
	Name    string `json:"name"`
	Rig     string `json:"rig,omitempty"`
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail,omitempty"`
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

// LandingRow is one landing the page's Landings table shows: a recent landing
// or rejection, with what it cost — the time each landing stage took, and om's
// verdict and score — or a landing the worker is running right now, which has
// none of those yet.
type LandingRow struct {
	At      time.Time `json:"at"`
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig,omitempty"`
	Title   string    `json:"title,omitempty"`
	Polecat string    `json:"polecat,omitempty"`
	// Outcome is "landed", "rejected", "running" for a landing the worker
	// holds right now, or "backoff" for one that keeps failing and is waiting
	// for a retry. A running row's At is the merge, and it carries no verdict,
	// score or stage time, because those are logged only once the landing
	// ends. Verdict is om's reading of a landing: "approved", "skipped"
	// (landed with no review of its own) or "error". Kind is why a rejection
	// was refused: review, gate, conflict, policy or empty.
	Outcome  string   `json:"outcome"`
	Verdict  string   `json:"verdict,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Score    *float64 `json:"score,omitempty"`
	LintSecs *float64 `json:"lint_secs,omitempty"`
	GateSecs *float64 `json:"gate_secs,omitempty"`
	OMSecs   *float64 `json:"om_secs,omitempty"`
	// ShipSecs is the landing's total ship time, dispatched to shipped in
	// seconds: the number the Ship time tile reports as a median. It is nil for
	// a landing with no dispatch line (hand-slung), a rejection, and a landing
	// still waiting for the deploy that ships it.
	ShipSecs *float64 `json:"ship_secs,omitempty"`
	// ShipVia names what ships this rig's landings, so the cell's title can say
	// what the number means: "deploy" for the daemon restart that installs
	// gastown's commit, "staging" for the app rig whose staging workflow
	// deploys main. It is empty, with the other ship fields, for a landing in a
	// rig with no ship definition.
	ShipVia string `json:"ship_via,omitempty"`
	// ShipPending marks a landing that has a dispatch line and has not shipped
	// yet. It is false where ShipSecs is set and where the rig has no ship
	// definition.
	ShipPending bool `json:"ship_pending,omitempty"`
	// ShipFailed marks a pending landing whose newest covering staging run
	// failed, and ShipRunState carries that run's own state for the cell's
	// title. Both are absent on every other row.
	ShipFailed   bool   `json:"ship_failed,omitempty"`
	ShipRunState string `json:"ship_run_state,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Route        string `json:"route,omitempty"`
	Risk         bool   `json:"risk,omitempty"`
	Detail       string `json:"detail,omitempty"`
	// Stage is the landing stage a "backoff" row's last attempt failed at,
	// the text after "landing failed at". Failures is how many attempts in a
	// row have failed there, and NextTry when the worker tries again. The
	// three are absent on every other outcome; Detail is a backoff row's
	// error and a rejected row's refusal (gt-fn9e6.44).
	Stage    string     `json:"stage,omitempty"`
	Failures int        `json:"failures,omitempty"`
	NextTry  *time.Time `json:"next_try,omitempty"`
}

// Trend is the last 24 hours as the page draws it: landings and rejections by
// local hour, the newest landings' stage times, and the host load per hour.
type Trend struct {
	Hours  []TrendHour  `json:"hours"`
	Stages []TrendPoint `json:"stages,omitempty"`
	// Recent is the newest landings and rejections, newest first.
	Recent []LandingRow `json:"recent,omitempty"`
}

// TierSweepStage is one tier's verdict inside a sweep row. Failed names the
// failing units the sweep's own record kept, and is empty for every row but the
// rig's latest: the record holds only the last run (internal/daemon/tier_sweep.go).
type TierSweepStage struct {
	Tier    string   `json:"tier"`
	Verdict string   `json:"verdict"` // GREEN or RED
	Failed  []string `json:"failed,omitempty"`
}

// TierSweepRun is a sweep in flight: the stages the cycle runs and how long it
// has been at them. Tiers is the cycle's whole stage list, off the daemon's
// "sweep started" line, and is empty on the fallback reading of a log from a
// daemon that logs no start (gt-rntre); there Tier names the one stage still to
// run. Since is the cycle's start on the first reading, the stage's on the
// fallback.
type TierSweepRun struct {
	Rig        string    `json:"rig"`
	Tier       string    `json:"tier"`
	Tiers      []string  `json:"tiers,omitempty"`
	Since      time.Time `json:"since"`
	ElapsedSec int64     `json:"elapsed_sec"`
}

// TierSweepRow is one completed sweep, off its "swept" log line. Secs is nil
// for a line that predates the duration the daemon logs since gt-iqzr0.
type TierSweepRow struct {
	At     time.Time        `json:"at"`
	Rig    string           `json:"rig"`
	SHA    string           `json:"sha"`
	Stages []TierSweepStage `json:"stages"`
	Secs   *float64         `json:"secs,omitempty"`
}

// TierSweep is the daemon's tier sweeps: whether one is running now, and the
// last few that finished. Unavailable says the daemon log could not be read,
// which is not the same as a log that holds no sweeps.
type TierSweep struct {
	Running     *TierSweepRun  `json:"running,omitempty"`
	Sweeps      []TierSweepRow `json:"sweeps"`
	Unavailable bool           `json:"unavailable,omitempty"`
}

// EscalationRow is one open escalation as the pane draws it. At is when it was
// raised, off the bead's own escalated_at (its creation time when that line is
// missing); AgeSec is that time read against the reader's clock, and is nil
// when the bead records no time it can be measured from.
type EscalationRow struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Severity    string    `json:"severity"` // critical, high, medium, low
	EscalatedBy string    `json:"escalated_by,omitempty"`
	At          time.Time `json:"at"`
	AgeSec      *int64    `json:"age_sec,omitempty"`
}

// Escalations is the open escalation beads, newest first, capped with the rest
// counted in More. Unavailable says the read failed, which is not the same as a
// town holding none: a pane must never show an empty list for a read it could
// not make.
type Escalations struct {
	Rows        []EscalationRow `json:"rows"`
	More        int             `json:"more,omitempty"`
	Unavailable bool            `json:"unavailable,omitempty"`
}

// State is everything the page draws apart from the feed.
type State struct {
	Now       time.Time `json:"now"`
	Viewers   int       `json:"viewers"`
	Health    Health    `json:"health"`
	Summary   *Summary  `json:"summary,omitempty"`
	SummaryAt time.Time `json:"summary_at,omitempty"`
	Polecats  []Polecat `json:"polecats"`
	// Rigs is one row per known rig, joined from the queue and the polecats
	// (RigRows). It is nil until both readers have reported.
	Rigs    []Rig       `json:"rigs,omitempty"`
	Machine Machine     `json:"machine"`
	Loads   []LoadPoint `json:"loads"`
	// Cloud is the cloud patrol's latest report, read from the directory it
	// writes. It is nil until the reader first reports.
	Cloud *Cloud `json:"cloud,omitempty"`
	// Deploys is the deploy and staging workflows' runs, drawn inside the Cloud
	// section under the patrol's findings. It is nil until the reader first
	// reports.
	Deploys *Deploys `json:"deploys,omitempty"`
	// Reports is the overseer's latest hourly report, read from the directory it
	// writes under the town root. It is nil until the reader first reports.
	// Questions is the overseer's open questions for Sloan, read from the town's
	// own beads database. It is nil until the reader first reports.
	Reports   *Reports        `json:"reports,omitempty"`
	Questions *Questions      `json:"questions,omitempty"`
	Spend     json.RawMessage `json:"spend,omitempty"`
	OM        *OM             `json:"om,omitempty"`
	TierSweep *TierSweep      `json:"tiersweep,omitempty"`
	// Forgejo is the viewer's recent-activity feed, oldest first. It is nil for
	// a town with no viewer token, and until the reader first reports.
	Forgejo  *ForgejoFeed `json:"forgejo,omitempty"`
	Dispatch *Dispatch    `json:"dispatch,omitempty"`
	// Escalations is the town's open escalation beads, the same set the
	// escalations tile counts. It is nil until the reader first reports.
	Escalations *Escalations `json:"escalations,omitempty"`
	Queue       *Queue       `json:"queue,omitempty"`
	Trend       *Trend       `json:"trend,omitempty"`
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
	// Cloud reads the cloud patrol's latest report from its reports directory.
	Cloud func() *Cloud
	// Spend returns the DeepSeek spend report as JSON, nil when unavailable.
	Spend func() json.RawMessage
	// OM reads the reviewer's record from disk.
	OM func() *OM
	// TierSweep reads the daemon's tier sweeps from the daemon log.
	TierSweep func() *TierSweep
	// Forgejo reads the viewer's Forgejo recent-activity feed.
	Forgejo func() *ForgejoFeed
	// Deploys reads the deploy and staging workflows' runs from the same viewer.
	Deploys func() *Deploys
	// Reports reads the overseer's latest hourly report from the directory it
	// writes, and ReportsDir is that directory: the /api/report route reads an
	// earlier report from inside it, by the timestamp its name carries.
	Reports    func() *Reports
	ReportsDir string
	// Questions reads the overseer's open questions for Sloan.
	Questions func() *Questions
	// Escalation reads the town's open escalation beads.
	Escalation func() *Escalations
	// Dispatch reads the spec dispatcher's last tick from the daemon log.
	Dispatch func() *Dispatch
	// Queue reads the work queue lists; Bead reads one bead's text on request.
	Queue func() *Queue
	Bead  func(rig, id string) (*BeadDetail, error)
	// RigTheme reads one rig's effective name theme and up to five sample names
	// from it, for the Rigs panel's Names column. Nil leaves every row's theme
	// empty.
	RigTheme func(rig string) (theme string, names []string)
	// Trend reads the last 24 hours of landings, rejections, stage times and
	// host load.
	Trend func() *Trend
	// LoadSample records one machine sample for the trend's load history. The
	// hub calls it from its machine poll, which runs only while a page is open.
	LoadSample func(at time.Time, load float64)

	Now func() time.Time

	FeedEvery       time.Duration
	SummaryEvery    time.Duration
	HealthEvery     time.Duration
	MachineEvery    time.Duration
	CloudEvery      time.Duration
	SpendEvery      time.Duration
	OMEvery         time.Duration
	TierSweepEvery  time.Duration
	ForgejoEvery    time.Duration
	DeploysEvery    time.Duration
	ReportsEvery    time.Duration
	QuestionsEvery  time.Duration
	EscalationEvery time.Duration
	DispatchEvery   time.Duration
	QueueEvery      time.Duration
	TrendEvery      time.Duration

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
	def(&c.CloudEvery, 60*time.Second)
	def(&c.SpendEvery, 5*time.Minute)
	def(&c.OMEvery, 60*time.Second)
	def(&c.TierSweepEvery, 60*time.Second)
	def(&c.ForgejoEvery, 3*time.Minute)
	// The deploys block is read on its own clock rather than the feed's three
	// minutes: a stage of a release that is running changes within seconds,
	// and a run waiting on a runner is the one thing here worth catching
	// promptly.
	def(&c.DeploysEvery, 60*time.Second)
	// The overseer's report is written hourly, so its own minute is a page's
	// worth of slack: nothing there changes between one minute and the next.
	def(&c.ReportsEvery, 60*time.Second)
	// A question waits on a person, so it changes on the minute at the
	// soonest: the reader runs bd per open question, and a page asks nothing
	// faster than that.
	def(&c.QuestionsEvery, 60*time.Second)
	def(&c.EscalationEvery, 60*time.Second)
	def(&c.DispatchEvery, 10*time.Second)
	def(&c.QueueEvery, 60*time.Second)
	def(&c.TrendEvery, 60*time.Second)
	if c.RingSize <= 0 {
		c.RingSize = 500
	}
}
