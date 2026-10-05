// Package townhealth computes the town's one health signal (wayfinder D8,
// gt-s3rec.1): a list of fields, each with a verdict and a tag saying how it
// was observed, and an overall verdict that is the worst field's.
//
//	LIVE      measured now, by this computation (a Dolt ping, a bd query)
//	RECORDED  read from a record another process wrote (a state file)
//	UNKNOWN   the question could not be answered
//
// UNKNOWN is never rendered as zero and never as green: a failed query is an
// UNKNOWN field, which is worse than a degraded one, because a question
// nobody can answer may be hiding anything. The verdicts, in order:
//
//	green < degraded < red < unknown
//
// and Verdict.ExitCode maps them to 0, 1, 2 and 3.
//
// Compute is pure: every input comes through one of the small source
// interfaces in Inputs, and the clock is Inputs.Now. A nil source is not
// wired and reads as UNKNOWN. The daemon wires the real sources and writes
// the report every tick to one file (file.go); gt status --line reads it
// and renders it with Line, treating a stale file as UNKNOWN (gt-s3rec.2).
package townhealth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Tag says how a field was observed.
type Tag string

const (
	Live     Tag = "LIVE"
	Recorded Tag = "RECORDED"
	Unknown  Tag = "UNKNOWN"
)

// Verdict is one field's health, or the town's.
type Verdict string

const (
	Green    Verdict = "green"
	Degraded Verdict = "degraded"
	Red      Verdict = "red"
	// VerdictUnknown is the verdict of a field whose tag is UNKNOWN.
	VerdictUnknown Verdict = "unknown"
)

// rank orders verdicts; anything unrecognized ranks as unknown.
func (v Verdict) rank() int {
	switch v {
	case Green:
		return 0
	case Degraded:
		return 1
	case Red:
		return 2
	default:
		return 3
	}
}

// ExitCode is the verdict as a process exit code: 0 green, 1 degraded,
// 2 red, 3 unknown.
func (v Verdict) ExitCode() int { return v.rank() }

// Worse returns the worse of v and w.
func (v Verdict) Worse(w Verdict) Verdict {
	if w.rank() > v.rank() {
		return w
	}
	return v
}

// Field names.
const (
	FieldDolt = "dolt"
	// FieldExecTax is the per-exec cost of a freshly written program in the
	// daemon's process tree (gt-2ycne.1).
	FieldExecTax    = "exec-tax"
	FieldDaemon     = "daemon"
	FieldTick       = "tick"
	FieldLanding    = "landing"
	FieldEscalation = "escalation"
	FieldSlot       = "slot"
	FieldBackup     = "backup"
	FieldMain       = "main"
	FieldConfig     = "config"
	FieldNeedsHuman = "needs-human"
	FieldSeat       = "seat"
	// FieldPromote is how far a promoting rig's GitHub main is behind its
	// green main (gt-fn9e6.39).
	FieldPromote = "promote"
	// FieldDispatch is the automatic dispatcher: whether it is filling the
	// town's free seats (gt-xiw7o).
	FieldDispatch = "dispatch"
)

// Field is one health fact.
type Field struct {
	Name string `json:"name"`
	// Rig is set on per-rig fields.
	Rig string `json:"rig,omitempty"`
	// Subject names the tick or seat a per-subject field is about.
	Subject string  `json:"subject,omitempty"`
	Tag     Tag     `json:"tag"`
	Verdict Verdict `json:"verdict"`
	// Value is the short display value; "?" when the tag is UNKNOWN.
	Value string `json:"value"`
	// Detail explains a non-green verdict or an UNKNOWN in one phrase.
	Detail string `json:"detail,omitempty"`
}

// Key is the field's identity: name, then rig and subject when set.
func (f Field) Key() string {
	k := f.Name
	if f.Rig != "" {
		k += "/" + f.Rig
	}
	if f.Subject != "" {
		k += ":" + f.Subject
	}
	return k
}

// Report is one computation of the town's health.
type Report struct {
	// At is when the report was computed: the daemon tick time.
	At      time.Time `json:"at"`
	Verdict Verdict   `json:"verdict"`
	// Landed counts landings across rigs in the last 24 hours; nil when it
	// could not be counted.
	Landed *int    `json:"landed_24h,omitempty"`
	Fields []Field `json:"fields"`
	// HeartbeatCount is the daemon heartbeat count this report saw, the
	// baseline the next report checks for advance. Zero when unread.
	HeartbeatCount int64 `json:"heartbeat_count,omitempty"`
	// ExecTaxMS is the median cost of exec'ing a freshly written program in
	// the daemon's process tree, in milliseconds; nil when the probe did not
	// answer. The exec-tax field carries its verdict; this is the number a
	// reader compares between reports (gt-2ycne.1).
	ExecTaxMS *float64 `json:"exec_tax_ms,omitempty"`
	// Steward is the steward job ledger over the last hour; nil when the town
	// runs no steward or its ledger could not be read (gt-9bioi.3).
	Steward *StewardCounters `json:"steward,omitempty"`
}

// Limits is a degraded and a red threshold. A zero limit never trips.
type Limits struct {
	Degraded time.Duration
	Red      time.Duration
}

// judge returns the verdict for v against l.
func (l Limits) judge(v time.Duration) Verdict {
	switch {
	case l.Red > 0 && v >= l.Red:
		return Red
	case l.Degraded > 0 && v >= l.Degraded:
		return Degraded
	default:
		return Green
	}
}

// Thresholds are every limit the computation applies. The operator config
// carries them; DefaultThresholds are the values written there.
type Thresholds struct {
	// DoltSamples is how many pings make up the p50; at least 1.
	DoltSamples int
	// DoltLatency judges the p50.
	DoltLatency Limits
	// ExecTax judges the median cost of exec'ing a freshly written program.
	ExecTax Limits
	// Heartbeat judges the age of the daemon's last completed heartbeat,
	// and how long a count that stopped advancing is tolerated.
	Heartbeat Limits
	// TickDegradedFactor and TickRedFactor judge a tick's last-fired age as
	// a multiple of its interval.
	TickDegradedFactor float64
	TickRedFactor      float64
	// Landing judges the age of a rig's oldest pending ready-to-land
	// submission, so a waiting bead is measured from when it was submitted
	// rather than from the rig's last landing (gt-m36as).
	Landing Limits
	// Escalation judges the age of the oldest open escalation.
	Escalation Limits
	// SlotHolder judges the age of the oldest container-slot holder.
	SlotHolder Limits
	// Backup judges the age of the newest complete Dolt backup.
	Backup Limits
	// NeedsHuman judges the age of the oldest bead waiting on the operator;
	// any such bead is at least degraded.
	NeedsHuman Limits
	// SeatStall judges how long a running seat's progress evidence has not
	// changed.
	SeatStall Limits
	// SeatEvidence is how old a seat's last sample may be before its stall
	// evidence is UNKNOWN; zero never expires it.
	SeatEvidence time.Duration
	// StewardErrorRate is the share of the last hour's attempted steward jobs
	// that may end in error or timeout before the steward field is degraded;
	// zero never trips. StewardMinJobs is how many attempts the hour needs
	// before the share means anything.
	StewardErrorRate float64
	StewardMinJobs   int
	// DispatchWindow is how far back the dispatcher's ticks are judged. The
	// dispatcher ticks every 60s, so a window this long sees many ticks: one
	// slow tick, a transient hold, or a single skipped candidate does not
	// carry a verdict, while a dispatcher that has stopped for the whole
	// window does.
	DispatchWindow time.Duration
	// PromoteWait is how long a green candidate may sit unpromoted before the
	// promote field degrades; zero never trips it.
	PromoteWait time.Duration
}

// DefaultPromoteWait is how long a green candidate may sit unpromoted before
// the promote field degrades. A promotion rides the verdict that called the
// commit green, so six hours is slack for a slow verdict, not a schedule: a
// candidate still unpromoted after it is a promotion that is not running
// (gt-fn9e6.39).
const DefaultPromoteWait = 6 * time.Hour

// DefaultDispatchWindow is how far back the dispatcher's ticks are judged.
// The ticker's interval is 60s, so ten minutes is ten ticks: long enough
// that no single tick's stutter reads as a stall, short enough that an
// operator learns within minutes that nothing is filling the free seats
// (gt-xiw7o).
const DefaultDispatchWindow = 10 * time.Minute

// The landing wait bounds. They measure a ready-to-land submission's own age
// — how long it has waited — not the time since the rig last landed, so a
// first submission after an idle night is not instantly an alarm (gt-m36as).
const (
	// LandingStageBudget is the sum of the landing gate's stage timeouts,
	// the time one submission may legitimately spend in the pipeline: lint
	// (2m, the daemon's defaultLandLintTimeout), then the gate (6m,
	// defaultLandTestTimeout), then om review (5m, land.DefaultOMTimeout).
	LandingStageBudget = 13 * time.Minute
	// LandingPassInterval is one landing worker pass interval (the daemon's
	// defaultLandingWorkerInterval): the slack a waiting submission gets
	// before the next pass can pick it up.
	LandingPassInterval = time.Minute
	// LandingWaitBudget is how long one submission may wait before the queue,
	// not the gate, is the problem: the stage budget plus one pass interval.
	// It is the depth-one limit; a deeper queue is judged by
	// LandingWaitLimits, and the landing field and the daemon's queue-stuck
	// item both judge by that, so the health line and the alert agree.
	LandingWaitBudget = LandingStageBudget + LandingPassInterval
)

// LandingWaitLimits scales the per-submission landing wait limits by the depth
// of a rig's ready-to-land queue, treating a depth below one as one. Landing
// is serial, so the oldest of n waiting submissions may legitimately spend n
// passes in the pipeline; judging it by one submission's budget reads a
// healthy backlog of two or three as a stuck queue (gt-cpefw). The health
// field and the daemon's queue-stuck item both judge by this, so they cannot
// disagree.
func LandingWaitLimits(base Limits, pending int) Limits {
	if pending < 1 {
		pending = 1
	}
	n := time.Duration(pending)
	return Limits{Degraded: base.Degraded * n, Red: base.Red * n}
}

// DefaultThresholds are the compiled defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		DoltSamples:        3,
		DoltLatency:        Limits{Degraded: time.Second, Red: 5 * time.Second},
		ExecTax:            Limits{Red: 50 * time.Millisecond},
		Heartbeat:          Limits{Degraded: 10 * time.Minute, Red: 30 * time.Minute},
		TickDegradedFactor: 2,
		TickRedFactor:      4,
		Landing:            Limits{Degraded: LandingStageBudget, Red: LandingWaitBudget},
		Escalation:         Limits{Degraded: time.Hour, Red: 4 * time.Hour},
		SlotHolder:         Limits{Degraded: 30 * time.Minute, Red: 2 * time.Hour},
		Backup:             Limits{Degraded: 36 * time.Hour, Red: 72 * time.Hour},
		NeedsHuman:         Limits{Red: 24 * time.Hour},
		SeatStall:          Limits{Degraded: 30 * time.Minute, Red: 2 * time.Hour},
		SeatEvidence:       30 * time.Minute,
		StewardErrorRate:   0.5,
		StewardMinJobs:     3,
		DispatchWindow:     DefaultDispatchWindow,
		PromoteWait:        DefaultPromoteWait,
	}
}

// Dolt pings the Dolt server once and returns the round trip.
type Dolt interface {
	Ping(ctx context.Context) (time.Duration, error)
}

// ExecTax measures what one exec of a freshly written program costs in the
// caller's process tree (internal/exectax).
type ExecTax interface {
	ExecTax(ctx context.Context) (time.Duration, error)
}

// HeartbeatRecord is the daemon's last completed heartbeat.
type HeartbeatRecord struct {
	At    time.Time
	Count int64
}

// Heartbeat reads the daemon's heartbeat record.
type Heartbeat interface {
	Heartbeat() (HeartbeatRecord, error)
}

// Tick is one daemon tick's interval and last-fired time; a zero LastFired
// means none is recorded.
type Tick struct {
	Name      string
	Interval  time.Duration
	LastFired time.Time
}

// Ticks lists the daemon's enabled ticks.
type Ticks interface {
	Ticks() ([]Tick, error)
}

// RigLandings is one rig's landing record and queue.
type RigLandings struct {
	Rig string
	// Landed counts landings at or after the since time Landings was given.
	Landed int
	// Pending counts work waiting for the landing worker.
	Pending int
	// Oldest is when the oldest pending submission was submitted for
	// landing, the age the landing field judges. Zero when none is pending,
	// and on a pending bead whose submission time cannot be read.
	Oldest time.Time
	// OldestBead is the bead Oldest belongs to; "" when Oldest is zero.
	OldestBead string
	// Failing are the rig's landings that keep failing and are backing off,
	// from the landing worker's own snapshot. Empty when none is failing,
	// which is also what a rig the worker does not serve reports.
	Failing []FailingLanding
	// Err is a failed read for this rig; the rest is ignored.
	Err error
}

// Landings reports every landing rig.
type Landings interface {
	Landings(ctx context.Context, since time.Time) ([]RigLandings, error)
}

// FailingLanding is one bead whose landing is failing in backoff: the stage
// that failed, the run of failures behind the worker's backoff, when the next
// try comes due, and the last error's first line (gt-fn9e6.44).
type FailingLanding struct {
	Bead     string
	Stage    string
	Failures int
	NextTry  time.Time
	Error    string
}

// LandingFailDegraded is the run of consecutive landing failures on one bead
// that makes its rig's landing field degraded. One failure is an incident the
// worker backs off and retries on its own; two in a row is a landing that is
// not clearing.
const LandingFailDegraded = 2

// maxFailingNamed bounds how many failing landings a field's detail names
// before it counts the rest.
const maxFailingNamed = 3

// Escalations finds the oldest open escalation; ok is false when none is
// open.
type Escalations interface {
	OldestEscalation(ctx context.Context) (opened time.Time, ok bool, err error)
}

// SlotHolder is one held container slot.
type SlotHolder struct {
	Name  string
	Since time.Time
}

// Slots lists the held container slots.
type Slots interface {
	SlotHolders() ([]SlotHolder, error)
}

// Backups finds the newest complete Dolt backup; ok is false when none
// exists.
type Backups interface {
	NewestBackup() (finished time.Time, ok bool, err error)
}

// RigMain is one rig's post-landing main state: the newest commit a run
// reached a verdict at and the newest one that passed.
type RigMain struct {
	Rig       string
	LastRun   string
	LastGreen string
	// PostLand is whether the rig configures a merge_queue.post_land_command.
	// A rig without one never runs a post-landing check, so having no verdict
	// is its normal state rather than an open question (gt-fn9e6.34).
	PostLand bool
	Err      error
}

// Mains reports every landing rig's main state.
type Mains interface {
	Mains() ([]RigMain, error)
}

// RigPromotion is one promoting rig's GitHub main as its record stands: the
// commits the target is behind the rig's green main by, and why it has not
// advanced (gt-fn9e6.39).
type RigPromotion struct {
	Rig string
	// LastPromoted is the commit the target's main was last advanced to; ""
	// when the rig has never promoted.
	LastPromoted string
	// Green is the commit the newest green verdict called good: the promotion
	// candidate, and the commit the lag is counted to.
	Green string
	// GreenAt is when that verdict was recorded, the clock a pending
	// candidate's wait is judged by, so a candidate that appeared just after
	// a stale promotion is not read as an old one.
	GreenAt time.Time
	// Behind is the commits between LastPromoted and Green, counted by the
	// source in the rig's repository, which is why townhealth itself reads no
	// git (gt-fn9e6.39).
	Behind int
	// LastError is the newest recorded failure to promote
	// (last_promote_error); "" when the newest attempt succeeded.
	LastError string
	// Diverged is true when the target's main is not an ancestor of Green
	// (github_diverged): the two mains diverged and a push could only rewrite
	// history.
	Diverged bool
	// DivergedTip is the target's main tip the divergence was recorded at,
	// the commit an operator has to reconcile by hand; "" when unknown.
	DivergedTip string
	// Err is a failed read of the rig's promotion record; the rest is
	// ignored.
	Err error
}

// Promotions lists the rigs that promote a green main to a target. A rig
// without a promote target has no record and is not listed.
type Promotions interface {
	Promotions() ([]RigPromotion, error)
}

// Config validates the town config; nil means it loads.
type Config interface {
	Validate() error
}

// NeedsHuman counts beads waiting on the operator and finds the oldest.
type NeedsHuman interface {
	NeedsHuman(ctx context.Context) (count int, oldest time.Time, err error)
}

// Seat is one seat's intent record as stall evidence.
type Seat struct {
	Rig  string
	Name string
	// Run is true when the town wants the seat running; only running and
	// frozen seats are judged.
	Run    bool
	Frozen bool
	// Sampled is when the evidence was last sampled; zero for never.
	Sampled time.Time
	// Changed is when the evidence last changed.
	Changed time.Time
	// DeadSamples counts consecutive samples that found the seat dead.
	DeadSamples int
}

// Seats lists the seats' intent records.
type Seats interface {
	Seats() ([]Seat, error)
}

// DispatchSeat is one seat of a dispatcher tick's roster: how many agents
// were live on it and the cap it allows.
type DispatchSeat struct {
	Live int
	Cap  int
}

// DispatchTick is one dispatcher tick's decision: what it saw and what it did.
type DispatchTick struct {
	At time.Time
	// Candidates counts the ready beads the tick considered.
	Candidates int
	// Seats is the roster the tick decided against.
	Seats []DispatchSeat
	// RosterUnreadable marks a tick whose roster could not be read. Its
	// Seats are meaningless, so a judgement that needs a free seat cannot
	// be made from it (gt-xiw7o).
	RosterUnreadable bool
	// Dispatched counts the beads the tick slung.
	Dispatched int
	// Refused, Planning and Skipped count the candidates the tick turned
	// away on purpose: the lint refused the bead's shape, the bead needs
	// planning, or the seat, the rig hold or the pool declined it for now.
	// A tick that did any of this was working, not stalled (gt-xiw7o).
	Refused  int
	Planning int
	Skipped  int
	// Failed counts the slings the tick attempted and lost. A failure is
	// not a deliberate declination: a tick that only failed is stalled
	// (gt-xiw7o).
	Failed int
	// LabeledFailed counts the ready beads the tick found carrying the
	// spec-dispatch-failed label. They are out of the queue until the label
	// is cleared, so a non-zero count is the operator's signal that a bead
	// is stuck there (gt-q6zoo).
	LabeledFailed int
}

// declined reports whether the tick turned candidates away on purpose rather
// than dispatching none by inaction. Failed is deliberately not a
// declination: a tick that only lost its slings is stalled (gt-xiw7o).
func (t DispatchTick) declined() bool {
	return t.Refused > 0 || t.Planning > 0 || t.Skipped > 0
}

// DispatchRecord is the automatic dispatcher as it stands: the patrol switch,
// the operator's hold reason, and the ticks it has run recently, oldest
// first.
type DispatchRecord struct {
	Active bool
	Hold   string
	Ticks  []DispatchTick
}

// Dispatcher reads the automatic dispatcher's state.
type Dispatcher interface {
	Dispatch() (DispatchRecord, error)
}

// Inputs are one computation's sources, clock, thresholds and previous
// report.
type Inputs struct {
	Now        time.Time
	Thresholds Thresholds
	// Prev is the previous report, for the heartbeat advance check; nil for
	// none.
	Prev *Report
	// DaemonStarted is when the daemon process started. The dispatch field
	// reads it to tell a daemon too young to have ticked from one that has
	// been up long enough to tick and has not (gt-xiw7o). Zero is an
	// unknown start.
	DaemonStarted time.Time

	Dolt        Dolt
	ExecTax     ExecTax
	Heartbeat   Heartbeat
	Ticks       Ticks
	Landings    Landings
	Escalations Escalations
	Slots       Slots
	Backups     Backups
	Mains       Mains
	Promotions  Promotions
	Config      Config
	NeedsHuman  NeedsHuman
	Seats       Seats
	Dispatch    Dispatcher
	// Steward is optional: nil is a town with no steward, and adds no field.
	Steward Steward
}

// errNotWired is the detail of a field whose source is nil.
var errNotWired = errors.New("no source wired")

// Compute builds the report. It runs every source once, in order, on the
// calling goroutine.
func Compute(ctx context.Context, in Inputs) Report {
	r := Report{At: in.Now}
	r.Fields = append(r.Fields, dolt(ctx, in))
	et, ms := execTax(ctx, in)
	r.Fields = append(r.Fields, et)
	r.ExecTaxMS = ms
	hb, count := heartbeat(in)
	r.Fields = append(r.Fields, hb)
	r.HeartbeatCount = count
	r.Fields = append(r.Fields, ticks(in)...)
	landed, fs := landings(ctx, in)
	r.Landed = landed
	r.Fields = append(r.Fields, fs...)
	r.Fields = append(r.Fields, escalation(ctx, in), slot(in), backup(in))
	r.Fields = append(r.Fields, mains(in)...)
	r.Fields = append(r.Fields, promotions(in)...)
	r.Fields = append(r.Fields, config(in), needsHuman(ctx, in))
	r.Fields = append(r.Fields, seats(in)...)
	r.Fields = append(r.Fields, dispatch(in))
	if f, c := steward(ctx, in); f != nil {
		r.Fields = append(r.Fields, *f)
		r.Steward = c
	}
	r.Verdict = Green
	for _, f := range r.Fields {
		r.Verdict = r.Verdict.Worse(f.Verdict)
	}
	return r
}

// unknown is an UNKNOWN field.
func unknown(name, rig, subject string, err error) Field {
	return Field{Name: name, Rig: rig, Subject: subject, Tag: Unknown, Verdict: VerdictUnknown, Value: "?", Detail: err.Error()}
}

func dolt(ctx context.Context, in Inputs) Field {
	if in.Dolt == nil {
		return unknown(FieldDolt, "", "", errNotWired)
	}
	n := in.Thresholds.DoltSamples
	if n < 1 {
		n = 1
	}
	var ok []time.Duration
	var lastErr error
	for i := 0; i < n; i++ {
		d, err := in.Dolt.Ping(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		ok = append(ok, d)
	}
	if len(ok) == 0 {
		// A refused or timed-out ping is a live observation of an
		// unreachable server, not an unanswered question.
		return Field{Name: FieldDolt, Tag: Live, Verdict: Red, Value: "unreachable", Detail: lastErr.Error()}
	}
	p50 := median(ok)
	f := Field{Name: FieldDolt, Tag: Live, Verdict: in.Thresholds.DoltLatency.judge(p50), Value: "p50 " + Short(p50)}
	if f.Verdict != Green {
		f.Detail = "p50 latency " + Short(p50)
	}
	if failed := n - len(ok); failed > 0 {
		f.Verdict = f.Verdict.Worse(Degraded)
		f.Detail = fmt.Sprintf("%d of %d pings failed: %v", failed, n, lastErr)
	}
	return f
}

// execTax judges the cost of exec'ing a freshly written program in this
// process tree, which is what the landing gate pays once per test binary.
// The millisecond count is the report's to publish; the field is the verdict
// on it.
func execTax(ctx context.Context, in Inputs) (Field, *float64) {
	if in.ExecTax == nil {
		return unknown(FieldExecTax, "", "", errNotWired), nil
	}
	d, err := in.ExecTax.ExecTax(ctx)
	if err != nil {
		return unknown(FieldExecTax, "", "", err), nil
	}
	ms := float64(d) / float64(time.Millisecond)
	f := Field{Name: FieldExecTax, Tag: Live, Verdict: in.Thresholds.ExecTax.judge(d), Value: Short(d) + "/exec"}
	if f.Verdict != Green {
		f.Detail = "every fresh executable in this tree pays " + Short(d)
	}
	return f, &ms
}

// median returns the middle sample (the lower middle for an even count).
func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)/2]
}

// heartbeat judges the daemon's liveness: fresh AND advancing. Advance is
// checked against the previous report's count, which makes the field LIVE;
// with no baseline only the record's freshness is known, so it is RECORDED.
func heartbeat(in Inputs) (Field, int64) {
	if in.Heartbeat == nil {
		return unknown(FieldDaemon, "", "", errNotWired), 0
	}
	rec, err := in.Heartbeat.Heartbeat()
	if err != nil {
		return unknown(FieldDaemon, "", "", err), 0
	}
	if rec.At.IsZero() {
		return unknown(FieldDaemon, "", "", errors.New("no heartbeat recorded")), rec.Count
	}
	age := in.Now.Sub(rec.At)
	f := Field{Name: FieldDaemon, Tag: Recorded, Verdict: in.Thresholds.Heartbeat.judge(age), Value: "heartbeat " + Short(age) + " ago"}
	if f.Verdict != Green {
		f.Detail = "last heartbeat " + Short(age) + " ago"
	}
	if p := in.Prev; p != nil && p.HeartbeatCount > 0 && !p.At.IsZero() {
		f.Tag = Live
		if rec.Count == p.HeartbeatCount {
			since := in.Now.Sub(p.At)
			if v := in.Thresholds.Heartbeat.judge(since); v != Green {
				f.Verdict = f.Verdict.Worse(v)
				f.Detail = fmt.Sprintf("heartbeat count stuck at %d for %s", rec.Count, Short(since))
			}
		}
	}
	return f, rec.Count
}

func ticks(in Inputs) []Field {
	if in.Ticks == nil {
		return []Field{unknown(FieldTick, "", "", errNotWired)}
	}
	ts, err := in.Ticks.Ticks()
	if err != nil {
		return []Field{unknown(FieldTick, "", "", err)}
	}
	fs := make([]Field, 0, len(ts))
	for _, t := range ts {
		switch {
		case t.Interval <= 0:
			fs = append(fs, unknown(FieldTick, "", t.Name, errors.New("no interval")))
			continue
		case t.LastFired.IsZero():
			fs = append(fs, unknown(FieldTick, "", t.Name, errors.New("never recorded firing")))
			continue
		}
		age := in.Now.Sub(t.LastFired)
		f := Field{Name: FieldTick, Subject: t.Name, Tag: Recorded, Verdict: Green, Value: Short(age) + "/" + Short(t.Interval)}
		switch th := in.Thresholds; {
		case th.TickRedFactor > 0 && float64(age) >= th.TickRedFactor*float64(t.Interval):
			f.Verdict = Red
		case th.TickDegradedFactor > 0 && float64(age) >= th.TickDegradedFactor*float64(t.Interval):
			f.Verdict = Degraded
		}
		if f.Verdict != Green {
			f.Detail = fmt.Sprintf("last fired %s ago, interval %s", Short(age), Short(t.Interval))
		}
		fs = append(fs, f)
	}
	return fs
}

// landings judges each rig's oldest pending submission age and counts the
// day's landings. The age is judged against LandingWaitLimits, so the queue's
// depth buys the wait it can legitimately take (gt-cpefw). A rig with nothing
// waiting is green however long since its last landing: the wait, not the
// town's quiet, is what the field measures (gt-m36as). The count is nil when
// any rig is unknown.
func landings(ctx context.Context, in Inputs) (*int, []Field) {
	if in.Landings == nil {
		return nil, []Field{unknown(FieldLanding, "", "", errNotWired)}
	}
	rigs, err := in.Landings.Landings(ctx, in.Now.Add(-24*time.Hour))
	if err != nil {
		return nil, []Field{unknown(FieldLanding, "", "", err)}
	}
	total, counted := 0, true
	fs := make([]Field, 0, len(rigs))
	for _, rl := range rigs {
		if rl.Err != nil {
			counted = false
			fs = append(fs, unknown(FieldLanding, rl.Rig, "", rl.Err))
			continue
		}
		total += rl.Landed
		f := Field{Name: FieldLanding, Rig: rl.Rig, Tag: Recorded, Verdict: Green, Value: fmt.Sprintf("%d pending", rl.Pending)}
		if rl.Pending > 0 {
			switch {
			case rl.Oldest.IsZero():
				// A pending bead with no submission time is a question this
				// field cannot answer; it says so rather than calling it
				// green.
				f.Verdict = Degraded
				f.Detail = fmt.Sprintf("%d pending, no submission time recorded", rl.Pending)
			default:
				age := in.Now.Sub(rl.Oldest)
				f.Value = fmt.Sprintf("%d pending, oldest %s", rl.Pending, Short(age))
				if f.Verdict = LandingWaitLimits(in.Thresholds.Landing, rl.Pending).judge(age); f.Verdict != Green {
					f.Detail = fmt.Sprintf("%s submitted %s ago", rl.OldestBead, Short(age))
				}
			}
		}
		if n := failingAtLeast(rl.Failing, LandingFailDegraded); n > 0 {
			// A landing that keeps failing is a queue the wait limits cannot
			// see: the bead is not waiting for its turn, it is being retried
			// and losing (gt-fn9e6.44).
			f.Value = fmt.Sprintf("%s, %d failing", f.Value, n)
			f.Verdict = f.Verdict.Worse(Degraded)
			f.Detail = joinDetail(f.Detail, failingDetail(rl.Failing, in.Now))
		}
		fs = append(fs, f)
	}
	if !counted {
		return nil, fs
	}
	return &total, fs
}

// failingAtLeast counts the landings that have failed n or more times in a
// row.
func failingAtLeast(fs []FailingLanding, n int) int {
	total := 0
	for _, f := range fs {
		if f.Failures >= n {
			total++
		}
	}
	return total
}

// failingDetail names the failing landings the way an operator needs them to
// act: which bead, at which stage, how many times in a row, when the next try
// comes due, and the last error's first line. Past maxFailingNamed it counts
// the rest rather than naming them.
func failingDetail(fs []FailingLanding, now time.Time) string {
	var parts []string
	named := 0
	for _, f := range fs {
		if f.Failures < LandingFailDegraded {
			continue
		}
		if named == maxFailingNamed {
			parts = append(parts, fmt.Sprintf("and %d more", failingAtLeast(fs, LandingFailDegraded)-named))
			break
		}
		named++
		parts = append(parts, fmt.Sprintf("%s failed %d times in a row at %s; %s; last: %s",
			f.Bead, f.Failures, f.Stage, nextTry(f.NextTry, now), f.Error))
	}
	return strings.Join(parts, "; ")
}

// nextTry says when a backing-off landing tries again, in the reader's terms
// rather than as a timestamp.
func nextTry(at, now time.Time) string {
	switch {
	case at.IsZero():
		return "next try unknown"
	case !at.After(now):
		return "retrying now"
	default:
		return "next try in " + Short(at.Sub(now))
	}
}

// joinDetail joins two field details, keeping whichever is set.
func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func escalation(ctx context.Context, in Inputs) Field {
	if in.Escalations == nil {
		return unknown(FieldEscalation, "", "", errNotWired)
	}
	opened, ok, err := in.Escalations.OldestEscalation(ctx)
	if err != nil {
		return unknown(FieldEscalation, "", "", err)
	}
	if !ok {
		return Field{Name: FieldEscalation, Tag: Live, Verdict: Green, Value: "none open"}
	}
	age := in.Now.Sub(opened)
	f := Field{Name: FieldEscalation, Tag: Live, Verdict: in.Thresholds.Escalation.judge(age), Value: "oldest " + Short(age)}
	if f.Verdict != Green {
		f.Detail = "oldest open escalation " + Short(age) + " old"
	}
	return f
}

func slot(in Inputs) Field {
	if in.Slots == nil {
		return unknown(FieldSlot, "", "", errNotWired)
	}
	hs, err := in.Slots.SlotHolders()
	if err != nil {
		return unknown(FieldSlot, "", "", err)
	}
	if len(hs) == 0 {
		return Field{Name: FieldSlot, Tag: Recorded, Verdict: Green, Value: "free"}
	}
	oldest := hs[0]
	for _, h := range hs[1:] {
		if h.Since.Before(oldest.Since) {
			oldest = h
		}
	}
	if oldest.Since.IsZero() {
		return unknown(FieldSlot, "", "", fmt.Errorf("holder %s has no start time", oldest.Name))
	}
	age := in.Now.Sub(oldest.Since)
	f := Field{Name: FieldSlot, Tag: Recorded, Verdict: in.Thresholds.SlotHolder.judge(age), Value: fmt.Sprintf("%s %s", oldest.Name, Short(age))}
	if f.Verdict != Green {
		f.Detail = fmt.Sprintf("%s has held a slot for %s", oldest.Name, Short(age))
	}
	return f
}

func backup(in Inputs) Field {
	if in.Backups == nil {
		return unknown(FieldBackup, "", "", errNotWired)
	}
	at, ok, err := in.Backups.NewestBackup()
	if err != nil {
		return unknown(FieldBackup, "", "", err)
	}
	if !ok {
		return Field{Name: FieldBackup, Tag: Recorded, Verdict: Red, Value: "none", Detail: "no complete backup"}
	}
	age := in.Now.Sub(at)
	f := Field{Name: FieldBackup, Tag: Recorded, Verdict: in.Thresholds.Backup.judge(age), Value: Short(age) + " old"}
	if f.Verdict != Green {
		f.Detail = "newest backup " + Short(age) + " old"
	}
	return f
}

func mains(in Inputs) []Field {
	if in.Mains == nil {
		return []Field{unknown(FieldMain, "", "", errNotWired)}
	}
	ms, err := in.Mains.Mains()
	if err != nil {
		return []Field{unknown(FieldMain, "", "", err)}
	}
	fs := make([]Field, 0, len(ms))
	for _, m := range ms {
		switch {
		case m.Err != nil:
			fs = append(fs, unknown(FieldMain, m.Rig, "", m.Err))
		case m.LastRun == "" && !m.PostLand:
			// No check is configured, so there is nothing to have a verdict
			// about: n/a rather than UNKNOWN, which would read as a check
			// that should have run (gt-fn9e6.34). A rig that once ran a check
			// keeps its recorded verdict either way.
			fs = append(fs, Field{Name: FieldMain, Rig: m.Rig, Tag: Recorded, Verdict: Green, Value: "n/a", Detail: "no post_land_command configured"})
		case m.LastRun == "":
			fs = append(fs, unknown(FieldMain, m.Rig, "", errors.New("no post-landing verdict recorded")))
		case m.LastRun != m.LastGreen:
			fs = append(fs, Field{Name: FieldMain, Rig: m.Rig, Tag: Recorded, Verdict: Red, Value: "red", Detail: "main red at " + short(m.LastRun)})
		default:
			fs = append(fs, Field{Name: FieldMain, Rig: m.Rig, Tag: Recorded, Verdict: Green, Value: "green"})
		}
	}
	return fs
}

// promotions judges each promoting rig's GitHub main beside its main field
// (gt-fn9e6.39): the lag behind the rig's green commit, the last failure to
// promote, and two mains that diverged. A rig that promotes nothing is not
// listed, so a town where no rig promotes grows no new field.
func promotions(in Inputs) []Field {
	if in.Promotions == nil {
		return []Field{unknown(FieldPromote, "", "", errNotWired)}
	}
	ps, err := in.Promotions.Promotions()
	if err != nil {
		return []Field{unknown(FieldPromote, "", "", err)}
	}
	fs := make([]Field, 0, len(ps))
	for _, p := range ps {
		fs = append(fs, promoteField(in, p))
	}
	return fs
}

// promoteField is one rig's verdict: RED when the two mains diverged, DEGRADED
// when the last promotion failed or a green candidate has waited past
// PromoteWait, and otherwise green with the lag GitHub is behind by. A question
// the record cannot answer — a rig that never promoted, a candidate with no
// verdict time — is UNKNOWN with its reason rather than red.
func promoteField(in Inputs, p RigPromotion) Field {
	switch {
	case p.Err != nil:
		return unknown(FieldPromote, p.Rig, "", p.Err)
	case p.Diverged:
		// A divergence is judged before the questions below it: the first
		// promotion attempt against a target nobody has reconciled records a
		// divergence with no promotion behind it, and that is the loudest
		// thing this field knows (gt-fn9e6.39).
		where := "the target's main"
		if p.DivergedTip != "" {
			where += " " + short(p.DivergedTip)
		}
		return Field{Name: FieldPromote, Rig: p.Rig, Tag: Recorded, Verdict: Red, Value: "diverged",
			Detail: fmt.Sprintf("%s is not an ancestor of the green commit %s; reconcile the two by hand", where, short(p.Green))}
	case p.LastPromoted == "":
		return unknown(FieldPromote, p.Rig, "", errors.New("never promoted to the target"))
	case p.Green == "":
		return unknown(FieldPromote, p.Rig, "", errors.New("no green verdict recorded"))
	}
	value := fmt.Sprintf("%d behind", p.Behind)
	if p.LastError != "" {
		return Field{Name: FieldPromote, Rig: p.Rig, Tag: Recorded, Verdict: Degraded, Value: value, Detail: p.LastError}
	}
	if p.Green == p.LastPromoted {
		// The target is at the green commit: nothing is waiting to be
		// promoted, and no age matters.
		return Field{Name: FieldPromote, Rig: p.Rig, Tag: Recorded, Verdict: Green, Value: value}
	}
	if p.GreenAt.IsZero() {
		// A candidate is pending and nobody recorded when it appeared, so the
		// wait — the whole question this rule asks — cannot be answered.
		return unknown(FieldPromote, p.Rig, "", fmt.Errorf("no verdict time for the green commit %s", short(p.Green)))
	}
	f := Field{Name: FieldPromote, Rig: p.Rig, Tag: Recorded, Verdict: Green, Value: value}
	if waited := in.Now.Sub(p.GreenAt); in.Thresholds.PromoteWait > 0 && waited >= in.Thresholds.PromoteWait {
		f.Verdict = Degraded
		f.Detail = fmt.Sprintf("green %s unpromoted for %s", short(p.Green), Short(waited))
	}
	return f
}

func config(in Inputs) Field {
	if in.Config == nil {
		return unknown(FieldConfig, "", "", errNotWired)
	}
	if err := in.Config.Validate(); err != nil {
		return Field{Name: FieldConfig, Tag: Live, Verdict: Red, Value: "invalid", Detail: err.Error()}
	}
	return Field{Name: FieldConfig, Tag: Live, Verdict: Green, Value: "valid"}
}

func needsHuman(ctx context.Context, in Inputs) Field {
	if in.NeedsHuman == nil {
		return unknown(FieldNeedsHuman, "", "", errNotWired)
	}
	n, oldest, err := in.NeedsHuman.NeedsHuman(ctx)
	if err != nil {
		return unknown(FieldNeedsHuman, "", "", err)
	}
	if n == 0 {
		return Field{Name: FieldNeedsHuman, Tag: Live, Verdict: Green, Value: "0"}
	}
	f := Field{Name: FieldNeedsHuman, Tag: Live, Verdict: Degraded, Value: fmt.Sprintf("%d", n), Detail: fmt.Sprintf("%d waiting on the operator", n)}
	if !oldest.IsZero() {
		age := in.Now.Sub(oldest)
		f.Verdict = f.Verdict.Worse(in.Thresholds.NeedsHuman.judge(age))
		f.Detail += ", oldest " + Short(age)
	}
	return f
}

// seats judges each running or frozen seat's stall evidence. Parked,
// stopped and submitted seats are where the town wants them and are not
// listed.
func seats(in Inputs) []Field {
	if in.Seats == nil {
		return []Field{unknown(FieldSeat, "", "", errNotWired)}
	}
	ss, err := in.Seats.Seats()
	if err != nil {
		return []Field{unknown(FieldSeat, "", "", err)}
	}
	var fs []Field
	for _, s := range ss {
		switch {
		case s.Frozen:
			fs = append(fs, Field{Name: FieldSeat, Rig: s.Rig, Subject: s.Name, Tag: Recorded, Verdict: Red, Value: "frozen", Detail: "frozen by the supervisor"})
		case !s.Run:
		case s.Sampled.IsZero():
			fs = append(fs, unknown(FieldSeat, s.Rig, s.Name, errors.New("no liveness sample")))
		case in.Thresholds.SeatEvidence > 0 && in.Now.Sub(s.Sampled) >= in.Thresholds.SeatEvidence:
			fs = append(fs, unknown(FieldSeat, s.Rig, s.Name, fmt.Errorf("last sample %s old", Short(in.Now.Sub(s.Sampled)))))
		case s.DeadSamples > 0:
			fs = append(fs, Field{Name: FieldSeat, Rig: s.Rig, Subject: s.Name, Tag: Recorded, Verdict: Degraded, Value: "dead",
				Detail: fmt.Sprintf("dead in %d consecutive samples", s.DeadSamples)})
		default:
			quiet := s.Sampled.Sub(s.Changed)
			if s.Changed.IsZero() {
				quiet = 0
			}
			f := Field{Name: FieldSeat, Rig: s.Rig, Subject: s.Name, Tag: Recorded, Verdict: in.Thresholds.SeatStall.judge(quiet), Value: "quiet " + Short(quiet)}
			if f.Verdict != Green {
				f.Value = "stalled " + Short(quiet)
				f.Detail = "no progress for " + Short(quiet)
			}
			fs = append(fs, f)
		}
	}
	return fs
}

// dispatch judges the automatic dispatcher. An operator hold is the pause the
// operator asked for and reads green; a dispatcher the town runs with no hold
// is red when the patrol is off, and red when it is stalled — every tick in
// the window saw candidates waiting and a free seat, dispatched none, and
// turned none away on purpose. A tick with no candidates, or a roster with
// every seat at its cap, is a town with nothing to fill and reports nothing.
//
// No tick in the window is green while the daemon is younger than the window:
// it has not had the chance to tick yet, and reporting it red would fire on
// every restart. A daemon that has been up longer than the window with the
// patrol on and no hold and still has no tick in it is silent, which is the
// failure this field exists to catch (gt-xiw7o).
//
// A tick whose roster could not be read is not a full town: the field cannot
// tell a free seat from a taken one, so it reads unknown rather than green
// (gt-xiw7o).
//
// Whatever the dispatcher's own state, the field carries the beads the
// spec-dispatch-failed label is holding out of the queue: a healthy dispatcher
// with three such beads is not a town an operator can leave alone, so the
// count degrades a green verdict and rides in the value (gt-q6zoo).
func dispatch(in Inputs) Field {
	if in.Dispatch == nil {
		return unknown(FieldDispatch, "", "", errNotWired)
	}
	rec, err := in.Dispatch.Dispatch()
	if err != nil {
		return unknown(FieldDispatch, "", "", err)
	}
	win := in.Thresholds.DispatchWindow
	if win <= 0 {
		win = DefaultDispatchWindow
	}
	since := in.Now.Add(-win)
	var ticks []DispatchTick
	for _, t := range rec.Ticks {
		if !t.At.Before(since) {
			ticks = append(ticks, t)
		}
	}
	f := dispatchField(in, rec, ticks, win)
	if n := failedLabelCount(ticks); n > 0 {
		f = withFailedLabels(f, n)
	}
	return f
}

// failedLabelCount is the newest tick's count of ready beads the
// spec-dispatch-failed label is holding out of the queue; zero when no tick in
// the window carried one (gt-q6zoo).
func failedLabelCount(ticks []DispatchTick) int {
	if len(ticks) == 0 {
		return 0
	}
	return ticks[len(ticks)-1].LabeledFailed
}

// withFailedLabels puts the beads the label is holding in front of the
// operator: a dispatcher that is otherwise healthy still leaves them out of
// the queue, so the field degrades and its value carries the count (gt-q6zoo).
//
// The count rides as a parenthesised clause after whatever state the field
// already reads ("ok (2 failed)", "off (2 failed)", "stalled (1 failed)"), so
// it composes with every state the dispatcher can be in without the state word
// and the number running together as one phrase (om, gt-q6zoo attempt 1).
func withFailedLabels(f Field, n int) Field {
	detail := fmt.Sprintf("%d bead(s) labeled %s are out of the queue until the label is cleared", n, constants.LabelSpecDispatchFailed)
	if f.Detail != "" {
		detail = f.Detail + "; " + detail
	}
	f.Detail = detail
	// UNKNOWN keeps its "?" value and carries the count in the detail alone:
	// the count is one thing the line cannot show for a field whose question
	// nobody could answer, and breaking the "?" convention to show it would
	// cost every reader of the line more than it tells them.
	if f.Verdict != VerdictUnknown {
		f.Value = fmt.Sprintf("%s (%d failed)", f.Value, n)
	}
	if f.Verdict == Green {
		f.Verdict = Degraded
	}
	return f
}

// dispatchField judges the dispatcher's own state; dispatch adds the beads the
// spec-dispatch-failed label holds out of the queue to whatever it returns.
func dispatchField(in Inputs, rec DispatchRecord, ticks []DispatchTick, win time.Duration) Field {
	if rec.Hold != "" {
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Green, Value: "held", Detail: rec.Hold}
	}
	if !rec.Active {
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Red, Value: "off", Detail: "spec_dispatch is off"}
	}
	if len(ticks) == 0 {
		if in.DaemonStarted.IsZero() {
			return unknown(FieldDispatch, "", "", errDaemonStartUnknown)
		}
		if in.Now.Sub(in.DaemonStarted) < win {
			return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Green, Value: "no ticks", Detail: "no tick in the last " + Short(win) + "; the daemon is younger than that"}
		}
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Red, Value: "silent",
			Detail: "no tick in the last " + Short(win) + " with the dispatcher on"}
	}
	for _, t := range ticks {
		if t.RosterUnreadable {
			return Field{Name: FieldDispatch, Tag: Unknown, Verdict: VerdictUnknown, Value: "?",
				Detail: "a tick in the last " + Short(win) + " reported a roster the daemon could not read"}
		}
	}
	stalled := true
	for _, t := range ticks {
		if t.Candidates == 0 || t.Dispatched > 0 || !freeDispatchSeat(t.Seats) || t.declined() {
			stalled = false
		}
	}
	if stalled {
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Red, Value: "stalled",
			Detail: fmt.Sprintf("%d tick(s) in %s with candidates waiting and a free seat, none dispatched", len(ticks), Short(win))}
	}
	last := ticks[len(ticks)-1]
	switch {
	case last.Candidates == 0:
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Green, Value: "idle", Detail: "no candidates"}
	case !freeDispatchSeat(last.Seats):
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Green, Value: "full", Detail: "every seat at its cap"}
	default:
		return Field{Name: FieldDispatch, Tag: Recorded, Verdict: Green, Value: "ok"}
	}
}

// errDaemonStartUnknown is the detail of a dispatch field whose daemon's start
// time is not known, so its age cannot be compared to the window.
var errDaemonStartUnknown = errors.New("the daemon's start time is unknown")

// freeDispatchSeat reports whether any seat of a roster has room under its
// cap. A roster the tick could not read has none, which is why such a tick is
// judged unknown rather than from this answer.
func freeDispatchSeat(seats []DispatchSeat) bool {
	for _, s := range seats {
		if s.Live < s.Cap {
			return true
		}
	}
	return false
}

// Short renders a duration in its largest whole unit: 850ms, 42s, 7m, 3h,
// 2d. Negative durations (clock skew) render as 0s.
func Short(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// short abbreviates a commit id.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
