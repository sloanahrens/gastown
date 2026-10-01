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
	"time"
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
	// Landing judges the age of a rig's last landing while it has landings
	// pending.
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
		Landing:            Limits{Degraded: 2 * time.Hour, Red: 8 * time.Hour},
		Escalation:         Limits{Degraded: time.Hour, Red: 4 * time.Hour},
		SlotHolder:         Limits{Degraded: 30 * time.Minute, Red: 2 * time.Hour},
		Backup:             Limits{Degraded: 36 * time.Hour, Red: 72 * time.Hour},
		NeedsHuman:         Limits{Red: 24 * time.Hour},
		SeatStall:          Limits{Degraded: 30 * time.Minute, Red: 2 * time.Hour},
		SeatEvidence:       30 * time.Minute,
		StewardErrorRate:   0.5,
		StewardMinJobs:     3,
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
	// Last is the newest landing on record; zero for none.
	Last time.Time
	// Landed counts landings at or after the since time Landings was given.
	Landed int
	// Pending counts work waiting for the landing worker.
	Pending int
	// Err is a failed read for this rig; the rest is ignored.
	Err error
}

// Landings reports every landing rig.
type Landings interface {
	Landings(ctx context.Context, since time.Time) ([]RigLandings, error)
}

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
	Err       error
}

// Mains reports every landing rig's main state.
type Mains interface {
	Mains() ([]RigMain, error)
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

// Inputs are one computation's sources, clock, thresholds and previous
// report.
type Inputs struct {
	Now        time.Time
	Thresholds Thresholds
	// Prev is the previous report, for the heartbeat advance check; nil for
	// none.
	Prev *Report

	Dolt        Dolt
	ExecTax     ExecTax
	Heartbeat   Heartbeat
	Ticks       Ticks
	Landings    Landings
	Escalations Escalations
	Slots       Slots
	Backups     Backups
	Mains       Mains
	Config      Config
	NeedsHuman  NeedsHuman
	Seats       Seats
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
	r.Fields = append(r.Fields, config(in), needsHuman(ctx, in))
	r.Fields = append(r.Fields, seats(in)...)
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

// landings judges each rig's last-landing age against its pending queue
// and counts the day's landings. The count is nil when any rig is unknown.
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
		// The queue is a live query; the last landing is the landings file.
		f := Field{Name: FieldLanding, Rig: rl.Rig, Tag: Recorded, Verdict: Green, Value: fmt.Sprintf("%d pending", rl.Pending)}
		switch {
		case rl.Pending == 0:
		case rl.Last.IsZero():
			f.Verdict = Degraded
			f.Detail = fmt.Sprintf("%d pending, no landing recorded", rl.Pending)
		default:
			age := in.Now.Sub(rl.Last)
			f.Value = fmt.Sprintf("%d pending, last %s ago", rl.Pending, Short(age))
			if f.Verdict = in.Thresholds.Landing.judge(age); f.Verdict != Green {
				f.Detail = fmt.Sprintf("%d pending, last landing %s ago", rl.Pending, Short(age))
			}
		}
		fs = append(fs, f)
	}
	if !counted {
		return nil, fs
	}
	return &total, fs
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
