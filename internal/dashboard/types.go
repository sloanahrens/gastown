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

	"github.com/steveyegge/gastown/internal/attention"
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
	StateWorking       = "working"        // assigned work and a live session
	StateQuiet         = "quiet"          // working, but the session has been silent past QuietAfter
	StateGating        = "gating"         // its bead is being gated and om-reviewed by the landing worker
	StateQueued        = "queued"         // its work is submitted and waits to land
	StateSpawning      = "spawning"       // dispatched seconds ago, session not up yet
	StateStalled       = "stalled"        // assigned work but no session: nothing is doing it
	StateReviewNeeded  = "review-needed"  // a live session holding uncommitted or unpushed state
	StateRecovery      = "recovery"       // idle, but its worktree needs recovery before reuse
	StateNeedsHuman    = "needs-human"    // its bead carries the old needs-human label
	StateNeedsOverseer = "needs-overseer" // its bead is labeled needs-overseer: the overseer acts or vets first
	StateNeedsOperator = "needs-operator" // its bead is labeled needs-operator: only the operator can settle it
	StateParked        = "parked"         // parked by an operator
	StateIdle          = "idle"
	// StateUnknown is a polecat the dashboard cannot classify because tmux could
	// not be read: without the session list "working" and "stalled" look alike.
	StateUnknown = "unknown"
)

// DescriptionCap is how many characters of a bead's description a seat row
// carries: enough for the hover to say what the bead is, short enough that a
// poll of twenty seats stays small.
const DescriptionCap = 600

// QuietAfter is how long a working session may stay silent before it reads as quiet.
const QuietAfter = 10 * time.Minute

// Polecat is one polecat: what it holds, what state that is in, and its record.
type Polecat struct {
	Rig   string `json:"rig"`
	Name  string `json:"name"`
	Bead  string `json:"bead,omitempty"`
	Title string `json:"title,omitempty"`
	// Description is the bead's description cut at DescriptionCap, for the
	// Work panel's hover on the bead (gt-kk6gb). It rides the seat's issue,
	// read once per rig per poll, so it costs no read of its own.
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"` // why the town's inventory put it in this state
	// Agent is the agent the polecat's live session was started with (its
	// recorded GT_AGENT), so a session left over from before a model switch
	// shows its own model. Empty when it has no session or none was recorded.
	Agent string `json:"agent,omitempty"`
	// CountsTowardCapacity is the dispatcher's own accounting: this polecat
	// occupies one of the roster's seats.
	CountsTowardCapacity bool `json:"counts_toward_capacity"`

	Priority *int     `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`

	// Slung is when the seat's bead was dispatched (Hooked, when the dispatch
	// record dates it) and ElapsedSec is that time read against the hub's
	// clock. It is the overrun guard's clock for the bead, so the Age column
	// and the guard agree; the feed's dispatch lines are not used, since that
	// window cannot tell one run of a bead from the next (gt-tzbtf). The hub
	// fills both.
	Slung      *time.Time `json:"slung,omitempty"`
	ElapsedSec int64      `json:"elapsed_sec,omitempty"`
	// StateSince is when the seat entered the state State names, for the one
	// state whose clock is not the hook: a bead submitted for landing entered
	// queued at its last write, which is the wait the Landings panel ages
	// (gt-iv7jf). Empty for every other state, whose clock is Slung.
	StateSince *time.Time `json:"state_since,omitempty"`
	// Hooked is when the seat's bead was dispatched: the bead's first sling
	// since its last run-end event (gt done, a hand close through gt bead
	// close, or the overrun guard's stop), the overrun guard's clock for the
	// bead. A bead the events log does not
	// dispatch falls back to the agent bead's last write, which gt sling makes;
	// a later write on that bead — a submit, a patrol clearing the hook — dates
	// the seat rather than the dispatch, so the fallback can only read too
	// short (gt-jwmxo, gt-tzbtf).
	Hooked     *time.Time `json:"hooked,omitempty"`
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

// Read sources, the keys of State.Reads. They name the panes whose reader keeps
// its last good value when a read fails.
const (
	SourceSeats    = "seats"
	SourceReady    = "ready"
	SourceMachine  = "machine"
	SourceSpend    = "spend"
	SourceJevSpend = "jev_spend"
	// SourceJevScorecard is the Jev batteries' scorecard lines, read apart
	// from the spend row beside them.
	SourceJevScorecard = "jev_scorecard"
	SourceOM           = "om"
	SourceQueue        = "queue"
)

// readStaleFactor is how many of a poll's own intervals a reading may age
// before it reads as stale.
const readStaleFactor = 3

// ReadStatus is one poller's last read, so a pane can tell a value that is
// current from one whose read failed or went old. A failed read keeps the last
// good value and leaves At at the read that produced it, without which a
// dropped read is indistinguishable from an unchanged one (gt-q6h8e).
type ReadStatus struct {
	At    time.Time `json:"at,omitempty"`
	Error bool      `json:"error,omitempty"`
	Every float64   `json:"every_sec,omitempty"`
	Stale bool      `json:"stale,omitempty"`
}

// StaleNow reports whether the value is not current, which is true of a read
// that failed, was never taken, or has aged past readStaleFactor of the poll's
// intervals.
func (r ReadStatus) StaleNow(now time.Time) bool {
	if r.Error || r.At.IsZero() {
		return true
	}
	return r.Every > 0 && now.Sub(r.At) > time.Duration(r.Every*float64(readStaleFactor)*float64(time.Second))
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

	// SeatsError says the seats reader failed on this poll: Polecats holds no
	// reading, which the hub reads as "keep the previous seats" rather than as a
	// town with none. ReadyError says the same of the ready-to-land reader, so a
	// dropped read is never read as a drained queue (gt-q6h8e).
	SeatsError bool `json:"-"`
	ReadyError bool `json:"-"`
}

// Proc is one busy process.
type Proc struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"`
}

// Machine is the host's busyness and what is burning it.
type Machine struct {
	// Load1, Load5 and Load15 are the load averages. macOS counts running
	// threads in them, so they run far past the core count on a half-idle
	// host: they are the secondary number, and CPUIdle is the reading of
	// record (gt-auiji).
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	// CPUIdle is the percent of the host's cores idle over the sample
	// window, nil when it could not be read.
	CPUIdle *float64 `json:"cpu_idle,omitempty"`
	// DoltCores is the Dolt server's CPU in cores, nil when it could not be
	// read.
	DoltCores *float64 `json:"dolt_cores,omitempty"`
	// Cores is the host's logical CPU count, the number the load average is
	// read against. The browser's own count is another machine's.
	Cores int `json:"cores,omitempty"`
	// HostState is the color the strip's host reading wears, decided here
	// from CPUIdle so the page draws a verdict rather than comparing
	// thresholds of its own. Absent for a host with CPU to spare and for one
	// that reported no idle reading, which the strip shows as a dash.
	HostState string    `json:"host_state,omitempty"`
	Top       []Proc    `json:"top"`
	At        time.Time `json:"at"`
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

// OMRunning is one om review running right now, read off the process that runs
// it. Bead, Rig and Polecat name the landing it reviews; a review whose merge
// the daemon has not logged yet is still listed, with no bead.
type OMRunning struct {
	Bead    string    `json:"bead,omitempty"`
	Rig     string    `json:"rig,omitempty"`
	Polecat string    `json:"polecat,omitempty"`
	Started time.Time `json:"started"`
	Base    string    `json:"base,omitempty"`
	Head    string    `json:"head,omitempty"`
	PID     int       `json:"pid,omitempty"`
}

// OM is the reviewer's record, read from the landings files and the daemon log.
type OM struct {
	// Model is what the reviewer runs, not the command it runs: the model its
	// backend configures ("sonnet"), or, for a backend that configures none,
	// the backend's own name. The raw command line is never what the page shows.
	Model     string         `json:"model,omitempty"`
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
	// Running is the om reviews running right now and Last the newest five om
	// actually ran: the two lists the panel opens with, over Recent's longer
	// tail of every landing and rejection. Both are left out when empty.
	Running []OMRunning `json:"running,omitempty"`
	Last    []OMReview  `json:"last,omitempty"`
	Recent  []OMReview  `json:"recent"`
	// ThresholdSource names where Threshold, Depth and TimeoutS came from: the
	// rig's .om.json the landing worker enforces, or the user's om config when
	// the rig has none. With no Threshold it says why there is none.
	ThresholdSource string `json:"threshold_source,omitempty"`
}

// Models is the header strip: what the town runs its work on, read from the
// configs the town already keeps. Every field re-reads on each poll, so a mode
// switched in settings shows on the page within one. A config that cannot be
// read leaves its field empty and the page says unknown, because a town whose
// model is unreadable is not a town running nothing.
type Models struct {
	// Unrecorded is the agent a polecat that records none is counted on: the
	// seat map's only seat, none when the map has several (config.UnrecordedAgent).
	// Empty when the settings cannot be read.
	Unrecorded string `json:"unrecorded,omitempty"`
	// SeatCap is the cap on live polecats the strip shows the count against:
	// polecat_pool.max_total, else the open seats' caps added up. Zero is a
	// town that declares neither, which is shown as the live count alone
	// rather than as a cap of zero.
	SeatCap int `json:"seat_cap,omitempty"`
	// OM is the model the reviewer runs, shortened to the name a person uses
	// for it, empty when the reviewer's config cannot be read or names none.
	OM string `json:"om,omitempty"`
	// Seats is the live polecat count against SeatCap, from the dispatcher's
	// roster. It is nil until the seats have been read once: a strip that has
	// not read them says so instead of showing an empty town.
	Seats *ModelsSeats `json:"seats,omitempty"`
	// SeatsUnreadable says the roster read failed on this poll: Seats and the
	// rows' Live counts are nil for a reason, which the page says apart from a
	// read it has not made yet.
	SeatsUnreadable bool `json:"seats_unreadable,omitempty"`
	// SeatRows is the models panel's seat table: every polecat seat, an open
	// one with its agent, cap and live count and a closed one marked closed.
	SeatRows []ModelSeat `json:"seat_rows,omitempty"`
	// Roles is the panel's model-per-role table, each row naming the config
	// key it was read from.
	Roles []ModelRole `json:"roles,omitempty"`
	// Activity is the panel's last 24 hours per agent: the landings each
	// agent's seat landed and the landings it was refused, off the records
	// the landing worker writes with the agent it ran on (gt-mukgk.6).
	Activity []AgentActivity `json:"activity,omitempty"`
	// ActivityUnreadable names the landings and review sources the activity
	// count could not read, by the name the panel shows. A source that is
	// missing is a window with nothing to count and is not named; one that is
	// there and cannot be read is, because its records are missing from a
	// table that would otherwise read as an empty window (gt-h3cwd).
	ActivityUnreadable []string `json:"activity_unreadable,omitempty"`
}

// AgentActivity is one agent's seat over the last 24 hours: the landings it
// completed and the landings the landing worker refused. A record that names
// no agent — a crew landing, or one written before the records carried an
// agent — is counted for nobody rather than against a guessed seat.
type AgentActivity struct {
	Agent    string `json:"agent"`
	Landed   int    `json:"landed"`
	Rejected int    `json:"rejected"`
}

// ModelSeat is one entry of the town's seat map (polecat_pool.seats).
type ModelSeat struct {
	Agent string `json:"agent,omitempty"`
	// Cap is the seat's cap on live polecats.
	Cap int `json:"cap"`
	// State is "open", "closed (cap 0)", or "closed until HH:MM" for a seat
	// inside its daily closed window.
	State string `json:"state,omitempty"`
	// Closed is a seat nothing may fill now: a cap of 0, or inside its window.
	Closed bool `json:"closed,omitempty"`
	// Unknown is a seat map whose config could not be read: neither open nor
	// closed, as far as the page can tell. It is a single row with no agent.
	Unknown bool `json:"unknown,omitempty"`
	// Live is the live seats the agent holds, filled by the hub from the
	// dispatcher's roster; nil until that read has succeeded.
	Live *int `json:"live,omitempty"`
	// Key is the config the seat was read from.
	Key string `json:"key"`
}

// ModelRole is the model one role runs on.
type ModelRole struct {
	// Role is polecat, om-review or crew, or a deep-review schedule's name
	// (deep-review-slice, deep-review-risk, deep-review): the review gets one
	// row per schedule, each with its own agent.
	Role string `json:"role"`
	// Model is empty when the config could not be read or names none: the page
	// says unknown, never a value nobody read.
	Model string `json:"model,omitempty"`
	// Key is the config key the model was read from.
	Key string `json:"key"`
	// Note says why Model is empty, when the key alone does not.
	Note string `json:"note,omitempty"`
	// Drift is what the town model table (settings/models.json) says this
	// role should run, set only when the live setting differs from it; empty
	// when they agree, or when the table says nothing about the role or
	// cannot be read.
	Drift string `json:"drift,omitempty"`
}

// ModelsSeats is the pool's occupancy: the polecats holding a seat, and the
// cap they are held against.
type ModelsSeats struct {
	Live int `json:"live"`
	Cap  int `json:"cap,omitempty"`
}

// SeatRoster is the pool occupancy the spec dispatcher counts: the live working
// seats per agent and their total, which the header strip shows so its count is
// the one a sling is refused against.
type SeatRoster struct {
	// Live is the number of live seats each agent holds.
	Live map[string]int
	// Total is the live seats across every agent, including any counted under
	// config.UnknownSeatAgent, which fills no row.
	Total int
	// Read is false when the occupancy could not be read: the counts are then
	// unknown, which is not the same as not yet read.
	Read bool
}

// SkippedBead is a bead the dispatcher considered and did not sling, with the
// reason it logged.
type SkippedBead struct {
	Bead   string `json:"bead"`
	Reason string `json:"reason"`
}

// Dispatch is the spec dispatcher's most recent tick.
type Dispatch struct {
	At         time.Time `json:"at"`
	Candidates int       `json:"candidates"`
	Roster     string    `json:"roster"` // "deepseek-flash 2/3"
	Dispatched int       `json:"dispatched"`
	Refused    int       `json:"refused"`
	Planning   int       `json:"planning"`
	Skipped    int       `json:"skipped"`
	Failed     int       `json:"failed"`
	Held       int       `json:"held"` // held by the failed label
	// Bypassed counts the P0 and P1 candidates dispatched despite overlapping
	// work in flight (gt-iydx1).
	Bypassed int           `json:"bypassed,omitempty"`
	Named    []SkippedBead `json:"named,omitempty"`
	More     int           `json:"more,omitempty"` // skipped beads the line did not name
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
	// Crew marks a row built on a crew session's branch, landed or still
	// running: no polecat built it, so the Polecat cell reads crew rather than
	// a dash, which would look like missing time (gt-kmp7c).
	Crew bool `json:"crew,omitempty"`
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
	// PolecatSecs is the time the bead's polecat seats spent hooked on it, as
	// the landing worker read it from the spans log; PolecatAttempts is each
	// seat's share of it, which the cell's title lists, PolecatLowerBound
	// marks a total that may understate the time, and PolecatUpperBound one
	// read from the bead's dispatch because no seat wrote a start, which may
	// overstate it. A crew landing and a record written before the fields
	// existed carry none of them, and the page draws a dash (gt-yxl22,
	// gt-90wi1, gt-v7ptp).
	PolecatSecs       *float64         `json:"polecat_secs,omitempty"`
	PolecatAttempts   []PolecatAttempt `json:"polecat_attempts,omitempty"`
	PolecatLowerBound bool             `json:"polecat_lower_bound,omitempty"`
	PolecatUpperBound bool             `json:"polecat_upper_bound,omitempty"`
	Commit            string           `json:"commit,omitempty"`
	Route             string           `json:"route,omitempty"`
	Risk              bool             `json:"risk,omitempty"`
	Detail            string           `json:"detail,omitempty"`
	// Stage is the landing stage a "backoff" row's last attempt failed at,
	// the text after "landing failed at". Failures is how many attempts in a
	// row have failed there, and NextTry when the worker tries again. The
	// three are absent on every other outcome; Detail is a backoff row's
	// error and a rejected row's refusal (gt-fn9e6.44).
	Stage    string     `json:"stage,omitempty"`
	Failures int        `json:"failures,omitempty"`
	NextTry  *time.Time `json:"next_try,omitempty"`
}

// PolecatAttempt is one seat's time on a landing's bead, an entry of a row's
// polecat_attempts: what the Landings pane's Polecat cell lists on hover. Secs
// is the attempt's duration, LowerBound marks it a floor, and UpperBound one
// read from the bead's dispatch rather than the seat's start (gt-90wi1,
// gt-v7ptp).
type PolecatAttempt struct {
	Seat       string  `json:"seat"`
	Secs       float64 `json:"secs"`
	LowerBound bool    `json:"lower_bound,omitempty"`
	UpperBound bool    `json:"upper_bound,omitempty"`
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
// for a line that predates the duration the daemon logs since gt-iqzr0. Walls
// is the wall time each tier took, keyed by tier name, off the stage lines that
// precede the swept line; a tier whose stage line the log lost is absent.
type TierSweepRow struct {
	At     time.Time          `json:"at"`
	Rig    string             `json:"rig"`
	SHA    string             `json:"sha"`
	Stages []TierSweepStage   `json:"stages"`
	Secs   *float64           `json:"secs,omitempty"`
	Walls  map[string]float64 `json:"walls,omitempty"`
}

// TierSweepTrendPoint is one sweep in a rig's wall trend: the sha, each tier's
// wall time in seconds, and the cycle's total. TotalSecs is nil for a swept
// line that carries no duration.
type TierSweepTrendPoint struct {
	At        time.Time          `json:"at"`
	SHA       string             `json:"sha"`
	Walls     map[string]float64 `json:"walls,omitempty"`
	TotalSecs *float64           `json:"total_secs,omitempty"`
}

// TierSweepTrendRig is one rig's recent sweeps, oldest first so the page draws
// them left to right; Sweeps is empty for a rig that has not swept yet.
type TierSweepTrendRig struct {
	Rig    string                `json:"rig"`
	Sweeps []TierSweepTrendPoint `json:"sweeps"`
}

// TierSweep is the daemon's tier sweeps: whether one is running now, the last
// few that finished, and one wall trend per rig over the last 30 sweeps.
// Unavailable says the daemon log could not be read, which is not the same as a
// log that holds no sweeps.
type TierSweep struct {
	Running     *TierSweepRun       `json:"running,omitempty"`
	Sweeps      []TierSweepRow      `json:"sweeps"`
	Trends      []TierSweepTrendRig `json:"trends,omitempty"`
	Unavailable bool                `json:"unavailable,omitempty"`
	// TrendsUnavailable says the town's rig list could not be read, so Trends
	// shows only the rigs the log names: a rig missing from it may be a rig the
	// registry lost, not one that has not swept.
	TrendsUnavailable bool `json:"trends_unavailable,omitempty"`
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
	SummaryAt time.Time `json:"summary_at,omitzero"`
	Polecats  []Polecat `json:"polecats"`
	// Rigs is one row per known rig, joined from the queue and the polecats
	// (RigRows). It is nil until both readers have reported.
	Rigs    []Rig       `json:"rigs,omitempty"`
	Machine Machine     `json:"machine"`
	Loads   []LoadPoint `json:"loads"`
	// Cloud is the cloud patrol's latest report, read from the directory it
	// writes. It is nil until the reader first reports.
	Cloud *Cloud `json:"cloud,omitempty"`
	// Mango is the town-metrics panel read from the directory the metrics
	// writer fills. It is nil until the reader first reports.
	Mango *Mango `json:"mango,omitempty"`
	// Deploys is the deploy and staging workflows' runs, drawn inside the Cloud
	// section under the patrol's findings. It is nil until the reader first
	// reports.
	Deploys *Deploys `json:"deploys,omitempty"`
	// Reports is the overseer's latest hourly report, read from the directory it
	// writes under the town root. It is nil until the reader first reports.
	// Questions is the overseer's open questions for Sloan, read from the town's
	// own beads database. It is nil until the reader first reports.
	Reports   *Reports   `json:"reports,omitempty"`
	Questions *Questions `json:"questions,omitempty"`
	// Overseer is the overseer's heartbeat, journal and open-question count,
	// read from the files it writes under the town root. It is nil until the
	// reader first reports.
	Overseer *Overseer `json:"overseer,omitempty"`
	// DeepReview is the scheduled deep review's next run and last ten runs,
	// read from the gastown rig's beads database. It is nil until the reader
	// first reports.
	DeepReview *DeepReview `json:"deepreview,omitempty"`
	// Usage is the town's spend over the last day, folded from the records the
	// usage scan writes under the town root. It is nil until the reader first
	// reports.
	Usage *Usage `json:"usage,omitempty"`
	// ClaudePlan is the Claude subscription's limit meters, folded from the
	// samples the status line appends under the town root. It is nil until the
	// reader first reports.
	ClaudePlan *ClaudePlan `json:"claude_plan,omitempty"`
	// Rework is what each om review bar costs in rework, from the landing
	// worker's review log. It is nil until the reader first reports.
	Rework *Rework         `json:"rework,omitempty"`
	Spend  json.RawMessage `json:"spend,omitempty"`
	// SpendHours is the last day's DeepSeek spend by hour, from the balance
	// ledger. The page does not read it; the charts below are built from it.
	SpendHours []SpendHour `json:"-"`
	// SpendSkipped is how many malformed ledger lines the last read skipped.
	SpendSkipped int `json:"-"`
	// PlanSamples is the status line's series as the last Claude poll read it,
	// which the charts' Claude dollars are folded from. It stays off the wire:
	// the charts carry what the page draws.
	PlanSamples PlanSamples `json:"-"`
	// CostCharts is what the Cost panel draws its two charts from, built from
	// the spend report, the hours, the Claude plan and the samples its dollars
	// come from. Nil until one of them has reported.
	CostCharts *CostCharts `json:"cost_charts,omitempty"`
	// CostProviders is one row per provider the Cost panel's table draws, in
	// the order costProviderBuilders lists them. It is built with each snapshot
	// against the reads' freshness, so a row whose read has failed or aged says
	// so; a provider whose reader is not configured has no row. It is always
	// sent, an empty list when there are no providers, so the page can tell none
	// from a reading that did not arrive.
	CostProviders []CostProvider `json:"cost_providers"`
	// JevSpend is the Jev ledger's calls and dollars over the spend command's
	// window, read beside the DeepSeek spend. It is nil until the reader first
	// reports; a failed read keeps the last good value and says so in Reads.
	JevSpend *JevSpend `json:"jev_spend,omitempty"`
	// JevScorecard is each Jev battery's lines, labeled count and question
	// verdicts, read beside the spend row. It is nil until the reader first
	// reports; a failed read keeps the last good value and says so in Reads.
	JevScorecard *JevScorecard `json:"jev_scorecard,omitempty"`
	OM           *OM           `json:"om,omitempty"`
	TierSweep    *TierSweep    `json:"tiersweep,omitempty"`
	// Models is the header strip: the polecat model, the pool's seats against
	// its cap, and the reviewer's model. It is nil until the reader reports.
	Models *Models `json:"models,omitempty"`
	// Forgejo is the viewer's recent-activity feed, oldest first. It is nil for
	// a town with no viewer token, and until the reader first reports.
	Forgejo  *ForgejoFeed `json:"forgejo,omitempty"`
	Dispatch *Dispatch    `json:"dispatch,omitempty"`
	// News is the top Hacker News stories, read about every five minutes. It is
	// nil while the pane is switched off and until the reader first reports.
	News *NewsFeed `json:"news,omitempty"`
	// Escalations is the town's open escalation beads, the same set the
	// escalations tile counts. It is nil until the reader first reports.
	Escalations *Escalations `json:"escalations,omitempty"`
	Queue       *Queue       `json:"queue,omitempty"`
	Trend       *Trend       `json:"trend,omitempty"`
	// Alerts is the most recent alerts the alerter raised, newest last, capped
	// at alertsKept.
	Alerts []Alert `json:"alerts,omitempty"`
	// Attention is the one list of what is waiting on a decision or an action,
	// merged from the escalations, the questions and the attention queue's
	// standing items, each row tagged with its owner. It is nil until the
	// readers behind it have reported (buildAttention).
	Attention *Attention `json:"attention,omitempty"`
	// Reads is one entry per poller that keeps a last good value, keyed by
	// source, so the page can tell a reading that failed or went old from one
	// that is merely unchanged. A source missing from it has not reported.
	Reads map[string]ReadStatus `json:"reads,omitempty"`
	// Strip is the verdict on each of the status strip's alarming readings,
	// decided from everything above (StripAlarms).
	Strip *StripAlarms `json:"strip,omitempty"`
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
	// Mango reads the town-metrics panel from its directory.
	Mango func() *Mango
	// Spend returns the DeepSeek spend report as JSON, nil when unavailable.
	Spend func() json.RawMessage
	// SpendHours returns the last day's spend by hour from the balance ledger,
	// with the malformed lines it skipped. It is read with the spend report.
	SpendHours func() SpendRead
	// JevSpend returns the Jev spend row, nil when the read failed.
	JevSpend func() *JevSpend
	// JevScorecard returns the Jev batteries' scorecard lines, nil when the
	// read failed.
	JevScorecard func() *JevScorecard
	// OM reads the reviewer's record from disk.
	OM func() *OM
	// TierSweep reads the daemon's tier sweeps from the daemon log.
	TierSweep func() *TierSweep
	// Forgejo reads the viewer's Forgejo recent-activity feed.
	Forgejo func() *ForgejoFeed
	// News reads the top Hacker News stories for the News pane.
	News func() *NewsFeed
	// Deploys reads the deploy and staging workflows' runs from the same viewer.
	Deploys func() *Deploys
	// Reports reads the overseer's latest hourly report from the directory it
	// writes, and ReportsDir is that directory: the /api/report route reads an
	// earlier report from inside it, by the timestamp its name carries.
	Reports    func() *Reports
	ReportsDir string
	// Questions reads the overseer's open questions for Sloan.
	Questions func() *Questions
	// Overseer reads the overseer's heartbeat and journal.
	Overseer func() *Overseer
	// DeepReview reads the scheduled deep review's next run and last runs.
	DeepReview func() *DeepReview
	// Usage reads the town's spend over the last day from the usage scan's
	// records.
	Usage func() *Usage
	// ClaudePlan reads the Claude plan meter from the status line's samples; the
	// payload carries the series it read, which the Cost charts fold their Claude
	// dollars from.
	ClaudePlan func() *ClaudePlan
	// Rework reads the om rework table from the landing worker's review log.
	Rework func() *Rework
	// Escalation reads the town's open escalation beads.
	Escalation func() *Escalations
	// Dispatch reads the spec dispatcher's last tick from the daemon log.
	Dispatch func() *Dispatch
	// Attention returns the attention queue's transitions that appeared since
	// its last call, and the acks as they stand (NewAttentionFollower).
	Attention func() ([]attention.Event, attention.Acks)
	// AttentionItems returns the queue's items as the daemon's state.json holds
	// them, which is where a standing row's class, owner and waiting-on come
	// from (NewAttentionFollower.Items). An error names the read among the ones
	// the panel says failed; nil leaves every queue row act.
	AttentionItems func() ([]attention.Item, error)
	// NeedsHuman returns Jev's needs_human answers by item life, keyed as
	// attention.LifeKey names them (jev/attention.NewScoreReader), and the
	// error of a log that exists but could not be read: the panel then says
	// the scores are unreadable rather than drawing them as unanswered. A nil
	// function, or a nil error with no answers, leaves every item unscored.
	NeedsHuman func() (attention.Scores, error)
	// LandingRisk returns the landing battery's risk level per risk-path item
	// key (attention.NewLandingRiskReader), and the error of a log that exists
	// but could not be read: the panel then keeps the last good answers, marks
	// them stale and names the failed read. A nil function leaves every row
	// without a landing risk.
	LandingRisk func() (attention.LandingRisks, error)
	// Models reads the header strip's models from the configs the town keeps.
	Models func() *Models
	// SeatRoster reads the occupancy the spec dispatcher counts, for the
	// strip's seat counts. Nil leaves them unread.
	SeatRoster func() SeatRoster
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

	FeedEvery         time.Duration
	SummaryEvery      time.Duration
	HealthEvery       time.Duration
	MachineEvery      time.Duration
	CloudEvery        time.Duration
	MangoEvery        time.Duration
	SpendEvery        time.Duration
	JevSpendEvery     time.Duration
	JevScorecardEvery time.Duration
	OMEvery           time.Duration
	TierSweepEvery    time.Duration
	ForgejoEvery      time.Duration
	NewsEvery         time.Duration
	DeploysEvery      time.Duration
	ReportsEvery      time.Duration
	QuestionsEvery    time.Duration
	OverseerEvery     time.Duration
	DeepReviewEvery   time.Duration
	UsageEvery        time.Duration
	ClaudePlanEvery   time.Duration
	ReworkEvery       time.Duration
	EscalationEvery   time.Duration
	DispatchEvery     time.Duration
	AttentionEvery    time.Duration
	ModelsEvery       time.Duration
	QueueEvery        time.Duration
	TrendEvery        time.Duration

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
	// The panel is rebuilt daily, so a read every five minutes picks up a new
	// one within minutes of the writer finishing it.
	def(&c.MangoEvery, 5*time.Minute)
	def(&c.SpendEvery, 5*time.Minute)
	def(&c.JevSpendEvery, 5*time.Minute)
	// A scorecard run reads each battery's log and, for some, its beads, so it
	// is asked less often than the spend row.
	def(&c.JevScorecardEvery, 15*time.Minute)
	def(&c.OMEvery, 60*time.Second)
	def(&c.TierSweepEvery, 60*time.Second)
	def(&c.ForgejoEvery, 3*time.Minute)
	// The front page turns over in hours and the reader holds what it read for
	// five minutes, so the poll is a fraction of that: it only publishes a
	// snapshot the moment it comes due, and the reader stays the pane's clock
	// (gt-9ktrr).
	def(&c.NewsEvery, newsPoll)
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
	// The heartbeat and journal are two small files and one bd list for the
	// question count; the silence window is minutes, so a minute is slack.
	def(&c.OverseerEvery, 60*time.Second)
	// A deep review runs about daily, so the minute is slack to spare; the
	// reader lists beads and runs bd per listed run for its comment count.
	def(&c.DeepReviewEvery, 60*time.Second)
	// The scan writes a record when a run ends, so the payload changes on the
	// hour at its fastest, and the reader re-decodes only the files whose mtime
	// moved: a poll is little more than a directory listing.
	def(&c.UsageEvery, 60*time.Second)
	// The status line appends a sample at most every ten minutes, and a read is
	// the tail of one small file.
	def(&c.ClaudePlanEvery, 60*time.Second)
	// The log gains a line per om verdict, a few an hour; a read is one small
	// file.
	def(&c.ReworkEvery, 60*time.Second)
	def(&c.EscalationEvery, 60*time.Second)
	def(&c.DispatchEvery, 10*time.Second)
	// The daemon appends to events.jsonl once a heartbeat; a read is a stat and
	// the new lines.
	def(&c.AttentionEvery, 5*time.Second)
	// The strip's two configs are files an operator edits by hand, so it reads
	// them on the same minute the other slow readers use: a mode switched in
	// settings shows within one poll, and a poll is a page's worth of slack.
	def(&c.ModelsEvery, 60*time.Second)
	def(&c.QueueEvery, 60*time.Second)
	def(&c.TrendEvery, 60*time.Second)
	if c.RingSize <= 0 {
		c.RingSize = 500
	}
}

// SpendRead is one read of the balance ledger: the last day's hours, and how many
// malformed lines the read skipped. A read that failed has no hours (nil), so
// the hourly chart is absent rather than showing hours that stopped moving.
type SpendRead struct {
	Hours   []SpendHour
	Skipped int
}
