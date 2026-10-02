package townhealth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

// fake answers every source from its fields.
type fake struct {
	pings    []time.Duration
	pingErrs []error
	pinged   int

	execTax    time.Duration
	execTaxErr error

	hb    HeartbeatRecord
	hbErr error

	ticks    []Tick
	ticksErr error

	landings    []RigLandings
	landingsErr error
	since       time.Time

	escOpened time.Time
	escOK     bool
	escErr    error

	holders    []SlotHolder
	holdersErr error

	backupAt  time.Time
	backupOK  bool
	backupErr error

	mains    []RigMain
	mainsErr error

	configErr error

	waiting    int
	waitOldest time.Time
	waitErr    error

	seats    []Seat
	seatsErr error

	dispatch    DispatchRecord
	dispatchErr error
}

func (f *fake) Ping(context.Context) (time.Duration, error) {
	i := f.pinged
	f.pinged++
	if i < len(f.pingErrs) && f.pingErrs[i] != nil {
		return 0, f.pingErrs[i]
	}
	if i < len(f.pings) {
		return f.pings[i], nil
	}
	return 10 * time.Millisecond, nil
}

func (f *fake) Heartbeat() (HeartbeatRecord, error) { return f.hb, f.hbErr }
func (f *fake) ExecTax(context.Context) (time.Duration, error) {
	return f.execTax, f.execTaxErr
}

func (f *fake) Ticks() ([]Tick, error) { return f.ticks, f.ticksErr }
func (f *fake) Landings(_ context.Context, since time.Time) ([]RigLandings, error) {
	f.since = since
	return f.landings, f.landingsErr
}
func (f *fake) OldestEscalation(context.Context) (time.Time, bool, error) {
	return f.escOpened, f.escOK, f.escErr
}
func (f *fake) SlotHolders() ([]SlotHolder, error)     { return f.holders, f.holdersErr }
func (f *fake) NewestBackup() (time.Time, bool, error) { return f.backupAt, f.backupOK, f.backupErr }
func (f *fake) Mains() ([]RigMain, error)              { return f.mains, f.mainsErr }
func (f *fake) Validate() error                        { return f.configErr }
func (f *fake) Seats() ([]Seat, error)                 { return f.seats, f.seatsErr }
func (f *fake) Dispatch() (DispatchRecord, error)      { return f.dispatch, f.dispatchErr }
func (f *fake) NeedsHuman(context.Context) (int, time.Time, error) {
	return f.waiting, f.waitOldest, f.waitErr
}

// healthy is a fake town with every field green.
func healthy() *fake {
	return &fake{
		execTax:  9 * time.Millisecond,
		hb:       HeartbeatRecord{At: ago(time.Minute), Count: 41},
		ticks:    []Tick{{Name: "wisp_reaper", Interval: 30 * time.Minute, LastFired: ago(10 * time.Minute)}},
		landings: []RigLandings{{Rig: "gastown", Last: ago(time.Hour), Landed: 7, Pending: 1}},
		backupAt: ago(12 * time.Hour), backupOK: true,
		mains: []RigMain{{Rig: "gastown", LastRun: "abc", LastGreen: "abc"}},
		seats: []Seat{{Rig: "gastown", Name: "polecat/opal", Run: true, Sampled: ago(time.Minute), Changed: ago(5 * time.Minute)}},
		dispatch: DispatchRecord{Active: true, Ticks: []DispatchTick{
			{At: ago(2 * time.Minute), Candidates: 2, Seats: []DispatchSeat{{Live: 1, Cap: 2}}, Dispatched: 1},
		}},
	}
}

func inputs(f *fake) Inputs {
	return Inputs{
		Now: now, Thresholds: DefaultThresholds(),
		Dolt: f, ExecTax: f, Heartbeat: f, Ticks: f, Landings: f, Escalations: f, Slots: f,
		Backups: f, Mains: f, Config: f, NeedsHuman: f, Seats: f, Dispatch: f,
	}
}

// field returns the report's field with key, failing when absent.
func field(t *testing.T, r Report, key string) Field {
	t.Helper()
	for _, f := range r.Fields {
		if f.Key() == key {
			return f
		}
	}
	var keys []string
	for _, f := range r.Fields {
		keys = append(keys, f.Key())
	}
	t.Fatalf("no field %q in %v", key, keys)
	return Field{}
}

func TestHealthyTownIsGreen(t *testing.T) {
	t.Parallel()
	r := Compute(context.Background(), inputs(healthy()))
	if r.Verdict != Green {
		for _, f := range r.Fields {
			if f.Verdict != Green {
				t.Errorf("field %s: %s %s %q", f.Key(), f.Verdict, f.Tag, f.Detail)
			}
		}
		t.Fatalf("verdict = %s, want green", r.Verdict)
	}
	if r.Landed == nil || *r.Landed != 7 {
		t.Errorf("Landed = %v, want 7", r.Landed)
	}
	if !r.At.Equal(now) || r.HeartbeatCount != 41 {
		t.Errorf("At %v count %d, want %v and 41", r.At, r.HeartbeatCount, now)
	}
	if f := field(t, r, "dolt"); f.Tag != Live || f.Value != "p50 10ms" {
		t.Errorf("dolt = %+v, want LIVE p50 10ms", f)
	}
}

func TestUnwiredSourcesAreUnknownNeverGreen(t *testing.T) {
	t.Parallel()
	r := Compute(context.Background(), Inputs{Now: now, Thresholds: DefaultThresholds()})
	if r.Verdict != VerdictUnknown || r.Verdict.ExitCode() != 3 {
		t.Fatalf("verdict = %s (exit %d), want unknown (3)", r.Verdict, r.Verdict.ExitCode())
	}
	if r.Landed != nil {
		t.Errorf("Landed = %d, want nil: an uncounted day is not zero", *r.Landed)
	}
	for _, f := range r.Fields {
		if f.Tag != Unknown || f.Verdict != VerdictUnknown || f.Value != "?" {
			t.Errorf("field %s = %+v, want UNKNOWN with value ?", f.Key(), f)
		}
	}
	if len(r.Fields) != 13 {
		t.Errorf("got %d fields, want one per source (13)", len(r.Fields))
	}
}

func TestFailedQueriesAreUnknown(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd: connection refused")
	f := healthy()
	f.hbErr, f.ticksErr, f.landingsErr, f.escErr, f.holdersErr = boom, boom, boom, boom, boom
	f.backupErr, f.mainsErr, f.waitErr, f.seatsErr = boom, boom, boom, boom
	f.execTaxErr, f.dispatchErr = boom, boom
	r := Compute(context.Background(), inputs(f))
	for _, key := range []string{"daemon", "tick", "landing", "escalation", "slot", "backup", "main", "needs-human", "seat", "exec-tax", "dispatch"} {
		got := field(t, r, key)
		if got.Tag != Unknown || got.Detail != boom.Error() {
			t.Errorf("%s = %+v, want UNKNOWN carrying the error", key, got)
		}
	}
	if r.Verdict != VerdictUnknown {
		t.Errorf("verdict = %s, want unknown", r.Verdict)
	}
}

func TestVerdictIsWorstField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []Verdict
		want Verdict
	}{
		{[]Verdict{Green, Green}, Green},
		{[]Verdict{Green, Degraded}, Degraded},
		{[]Verdict{Red, Degraded, Green}, Red},
		{[]Verdict{Red, VerdictUnknown}, VerdictUnknown},
		{[]Verdict{Green, "bogus"}, "bogus"},
	} {
		v := Green
		for _, w := range tc.in {
			v = v.Worse(w)
		}
		if v != tc.want {
			t.Errorf("worst of %v = %s, want %s", tc.in, v, tc.want)
		}
	}
	for v, code := range map[Verdict]int{Green: 0, Degraded: 1, Red: 2, VerdictUnknown: 3} {
		if v.ExitCode() != code {
			t.Errorf("%s exit = %d, want %d", v, v.ExitCode(), code)
		}
	}
}

func TestDolt(t *testing.T) {
	t.Parallel()
	refused := errors.New("dial tcp 127.0.0.1:3307: connection refused")
	for name, tc := range map[string]struct {
		pings     []time.Duration
		errs      []error
		want      Verdict
		value     string
		detailHas string
	}{
		"p50 not mean":   {pings: []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 9 * time.Second}, want: Green, value: "p50 20ms"},
		"slow degraded":  {pings: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}, want: Degraded, value: "p50 2s"},
		"slow red":       {pings: []time.Duration{6 * time.Second, 6 * time.Second, time.Second}, want: Red, value: "p50 6s"},
		"one failed":     {errs: []error{refused}, want: Degraded, value: "p50 10ms", detailHas: "1 of 3 pings failed"},
		"all failed red": {errs: []error{refused, refused, refused}, want: Red, value: "unreachable", detailHas: "connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			f.pings, f.pingErrs = tc.pings, tc.errs
			got := field(t, Compute(context.Background(), inputs(f)), "dolt")
			if got.Tag != Live || got.Verdict != tc.want || got.Value != tc.value || !strings.Contains(got.Detail, tc.detailHas) {
				t.Errorf("dolt = %+v, want LIVE %s %q detail containing %q", got, tc.want, tc.value, tc.detailHas)
			}
			if f.pinged != 3 {
				t.Errorf("pinged %d times, want DoltSamples (3)", f.pinged)
			}
		})
	}
}

// TestExecTax checks the field the daemon's exec probe publishes: the
// verdict on the median, and the raw milliseconds a reader compares between
// reports (gt-2ycne.1).
func TestExecTax(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		median    time.Duration
		threshold Limits
		want      Verdict
		value     string
	}{
		"clear":                           {median: 9 * time.Millisecond, threshold: DefaultThresholds().ExecTax, want: Green, value: "9ms/exec"},
		"at the threshold":                {median: 50 * time.Millisecond, threshold: DefaultThresholds().ExecTax, want: Red, value: "50ms/exec"},
		"taxed":                           {median: 180 * time.Millisecond, threshold: DefaultThresholds().ExecTax, want: Red, value: "180ms/exec"},
		"degenerate is degraded":          {median: 80 * time.Millisecond, threshold: Limits{Degraded: 60 * time.Millisecond, Red: 100 * time.Millisecond}, want: Degraded, value: "80ms/exec"},
		"a threshold of zero never trips": {median: time.Minute, threshold: Limits{}, want: Green, value: "1m/exec"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			f.execTax = tc.median
			in := inputs(f)
			in.Thresholds.ExecTax = tc.threshold
			r := Compute(context.Background(), in)
			got := field(t, r, "exec-tax")
			if got.Tag != Live || got.Verdict != tc.want || got.Value != tc.value {
				t.Errorf("exec-tax = %+v, want LIVE %s %q", got, tc.want, tc.value)
			}
			if r.ExecTaxMS == nil || *r.ExecTaxMS != float64(tc.median)/float64(time.Millisecond) {
				t.Errorf("ExecTaxMS = %v, want %v", r.ExecTaxMS, tc.median)
			}
			if tc.want != Green && got.Detail == "" {
				t.Errorf("exec-tax = %+v, want a detail naming what pays the cost", got)
			}
		})
	}
	// An unanswered probe is a question nobody can answer, not a fast tree.
	r := Compute(context.Background(), Inputs{Now: now, Thresholds: DefaultThresholds(), ExecTax: &fake{execTaxErr: errors.New("no temp dir")}})
	if r.ExecTaxMS != nil {
		t.Errorf("ExecTaxMS = %v with the probe failed, want nil", *r.ExecTaxMS)
	}
}

func TestDaemonHeartbeatFreshAndAdvancing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hb   HeartbeatRecord
		prev *Report
		tag  Tag
		want Verdict
	}{
		"no baseline is recorded": {hb: HeartbeatRecord{At: ago(time.Minute), Count: 5}, tag: Recorded, want: Green},
		"advancing is live":       {hb: HeartbeatRecord{At: ago(time.Minute), Count: 6}, prev: &Report{At: ago(3 * time.Minute), HeartbeatCount: 5}, tag: Live, want: Green},
		"stale is red":            {hb: HeartbeatRecord{At: ago(40 * time.Minute), Count: 6}, prev: &Report{At: ago(3 * time.Minute), HeartbeatCount: 5}, tag: Live, want: Red},
		"stuck count degrades":    {hb: HeartbeatRecord{At: ago(time.Minute), Count: 5}, prev: &Report{At: ago(12 * time.Minute), HeartbeatCount: 5}, tag: Live, want: Degraded},
		"stuck briefly is fine":   {hb: HeartbeatRecord{At: ago(time.Minute), Count: 5}, prev: &Report{At: ago(time.Minute), HeartbeatCount: 5}, tag: Live, want: Green},
		"never beat is unknown":   {tag: Unknown, want: VerdictUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			f.hb = tc.hb
			in := inputs(f)
			in.Prev = tc.prev
			got := field(t, Compute(context.Background(), in), "daemon")
			if got.Tag != tc.tag || got.Verdict != tc.want {
				t.Errorf("daemon = %+v, want %s %s", got, tc.tag, tc.want)
			}
		})
	}
}

func TestTicksAgainstInterval(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.ticks = []Tick{
		{Name: "ok", Interval: time.Hour, LastFired: ago(90 * time.Minute)},
		{Name: "late", Interval: 15 * time.Minute, LastFired: ago(31 * time.Minute)},
		{Name: "dead", Interval: 15 * time.Minute, LastFired: ago(time.Hour)},
		{Name: "never", Interval: time.Hour},
	}
	r := Compute(context.Background(), inputs(f))
	for key, want := range map[string]Verdict{"tick:ok": Green, "tick:late": Degraded, "tick:dead": Red, "tick:never": VerdictUnknown} {
		if got := field(t, r, key); got.Verdict != want {
			t.Errorf("%s = %+v, want %s", key, got, want)
		}
	}
	if got := field(t, r, "tick:late"); got.Tag != Recorded || got.Value != "31m/15m" {
		t.Errorf("tick:late = %+v, want RECORDED 31m/15m", got)
	}
}

func TestLandings(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.landings = []RigLandings{
		{Rig: "idle", Last: ago(100 * time.Hour), Landed: 0, Pending: 0},
		{Rig: "slow", Last: ago(3 * time.Hour), Landed: 2, Pending: 4},
		{Rig: "stuck", Last: ago(9 * time.Hour), Landed: 1, Pending: 1},
		{Rig: "new", Pending: 2},
	}
	r := Compute(context.Background(), inputs(f))
	if !f.since.Equal(ago(24 * time.Hour)) {
		t.Errorf("since = %v, want 24h before now", f.since)
	}
	for key, want := range map[string]Verdict{"landing/idle": Green, "landing/slow": Degraded, "landing/stuck": Red, "landing/new": Degraded} {
		if got := field(t, r, key); got.Verdict != want {
			t.Errorf("%s = %+v, want %s", key, got, want)
		}
	}
	if r.Landed == nil || *r.Landed != 3 {
		t.Errorf("Landed = %v, want 3", r.Landed)
	}

	f.landings = append(f.landings, RigLandings{Rig: "broken", Err: errors.New("unreadable")})
	r = Compute(context.Background(), inputs(f))
	if r.Landed != nil {
		t.Errorf("Landed = %d with one rig unknown, want nil", *r.Landed)
	}
	if got := field(t, r, "landing/broken"); got.Tag != Unknown {
		t.Errorf("landing/broken = %+v, want UNKNOWN", got)
	}
}

func TestAgeFields(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set  func(*fake)
		key  string
		tag  Tag
		want Verdict
	}{
		"no escalation":         {set: func(f *fake) {}, key: "escalation", tag: Live, want: Green},
		"young escalation":      {set: func(f *fake) { f.escOpened, f.escOK = ago(2*time.Hour), true }, key: "escalation", tag: Live, want: Degraded},
		"old escalation":        {set: func(f *fake) { f.escOpened, f.escOK = ago(5*time.Hour), true }, key: "escalation", tag: Live, want: Red},
		"free slots":            {set: func(f *fake) {}, key: "slot", tag: Recorded, want: Green},
		"long slot holder":      {set: func(f *fake) { f.holders = []SlotHolder{{"a", ago(time.Minute)}, {"b", ago(40 * time.Minute)}} }, key: "slot", tag: Recorded, want: Degraded},
		"holder without start":  {set: func(f *fake) { f.holders = []SlotHolder{{Name: "a"}} }, key: "slot", tag: Unknown, want: VerdictUnknown},
		"stale backup":          {set: func(f *fake) { f.backupAt = ago(40 * time.Hour) }, key: "backup", tag: Recorded, want: Degraded},
		"no backup":             {set: func(f *fake) { f.backupOK = false }, key: "backup", tag: Recorded, want: Red},
		"invalid config":        {set: func(f *fake) { f.configErr = errors.New("rigs.json: unknown key") }, key: "config", tag: Live, want: Red},
		"one needs human":       {set: func(f *fake) { f.waiting, f.waitOldest = 1, ago(time.Hour) }, key: "needs-human", tag: Live, want: Degraded},
		"needs human for a day": {set: func(f *fake) { f.waiting, f.waitOldest = 2, ago(25*time.Hour) }, key: "needs-human", tag: Live, want: Red},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			tc.set(f)
			got := field(t, Compute(context.Background(), inputs(f)), tc.key)
			if got.Tag != tc.tag || got.Verdict != tc.want {
				t.Errorf("%s = %+v, want %s %s", tc.key, got, tc.tag, tc.want)
			}
			if got.Verdict != Green && got.Detail == "" {
				t.Errorf("%s is %s with no detail", tc.key, got.Verdict)
			}
		})
	}
}

func TestMains(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.mains = []RigMain{
		{Rig: "a", LastRun: "1111111111", LastGreen: "1111111111"},
		{Rig: "b", LastRun: "2222222222", LastGreen: "1111111111"},
		{Rig: "c"},
		{Rig: "d", Err: errors.New("corrupt state")},
	}
	r := Compute(context.Background(), inputs(f))
	for key, want := range map[string]Verdict{"main/a": Green, "main/b": Red, "main/c": VerdictUnknown, "main/d": VerdictUnknown} {
		if got := field(t, r, key); got.Verdict != want {
			t.Errorf("%s = %+v, want %s", key, got, want)
		}
	}
	if got := field(t, r, "main/b"); got.Detail != "main red at 22222222" {
		t.Errorf("main/b detail = %q", got.Detail)
	}
}

func TestSeats(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.seats = []Seat{
		{Rig: "gt", Name: "polecat/quiet", Run: true, Sampled: ago(time.Minute), Changed: ago(10 * time.Minute)},
		{Rig: "gt", Name: "polecat/stalled", Run: true, Sampled: ago(time.Minute), Changed: ago(45 * time.Minute)},
		{Rig: "gt", Name: "polecat/dead", Run: true, Sampled: ago(time.Minute), DeadSamples: 2},
		{Rig: "gt", Name: "polecat/stale", Run: true, Sampled: ago(time.Hour), Changed: ago(time.Hour)},
		{Rig: "gt", Name: "polecat/unsampled", Run: true},
		{Rig: "gt", Name: "polecat/frozen", Frozen: true},
		{Rig: "gt", Name: "polecat/parked"},
	}
	r := Compute(context.Background(), inputs(f))
	for key, want := range map[string]Verdict{
		"seat/gt:polecat/quiet":     Green,
		"seat/gt:polecat/stalled":   Degraded,
		"seat/gt:polecat/dead":      Degraded,
		"seat/gt:polecat/stale":     VerdictUnknown,
		"seat/gt:polecat/unsampled": VerdictUnknown,
		"seat/gt:polecat/frozen":    Red,
	} {
		if got := field(t, r, key); got.Verdict != want {
			t.Errorf("%s = %+v, want %s", key, got, want)
		}
	}
	for _, fl := range r.Fields {
		if fl.Subject == "polecat/parked" {
			t.Errorf("parked seat listed: %+v", fl)
		}
	}
}

// seeded is a dispatcher tick that saw ready work, a free seat, and slung it.
func seeded(at time.Time, dispatched int) DispatchTick {
	return DispatchTick{At: at, Candidates: 3, Seats: []DispatchSeat{{Live: 1, Cap: 2}}, Dispatched: dispatched}
}

// The dispatch field is the operator's answer to "is anything filling the free
// seats": a dispatcher the town runs with no hold is red when it is off and
// red when every tick in the window had work and a seat and dispatched none,
// and quiet when the hold is deliberate or the town has nothing to fill
// (gt-xiw7o).
func TestDispatch(t *testing.T) {
	t.Parallel()
	idle := DispatchTick{At: ago(2 * time.Minute), Candidates: 0, Seats: []DispatchSeat{{Live: 0, Cap: 2}}}
	full := DispatchTick{At: ago(2 * time.Minute), Candidates: 3, Seats: []DispatchSeat{{Live: 2, Cap: 2}}}
	for _, tc := range []struct {
		name     string
		rec      DispatchRecord
		verdict  Verdict
		value    string
		detail   string
		linePart string
	}{
		{"off", DispatchRecord{}, Red, "off", "spec_dispatch is off", "dispatch=off[R]"},
		{
			"stalled",
			DispatchRecord{Active: true, Ticks: []DispatchTick{seeded(ago(6*time.Minute), 0), seeded(ago(2*time.Minute), 0)}},
			Red, "stalled", "none dispatched", "dispatch=stalled[R]",
		},
		{
			"held",
			DispatchRecord{Hold: "operator dispatch hold present (/town/seat-refill.hold); remove it to resume"},
			Green, "held", "operator dispatch hold present", "",
		},
		{"idle for want of candidates", DispatchRecord{Active: true, Ticks: []DispatchTick{idle}}, Green, "idle", "no candidates", ""},
		{"full roster", DispatchRecord{Active: true, Ticks: []DispatchTick{full}}, Green, "full", "every seat at its cap", ""},
		{"a dispatch in the window is not a stall", DispatchRecord{Active: true, Ticks: []DispatchTick{seeded(ago(6*time.Minute), 0), seeded(ago(2*time.Minute), 1)}}, Green, "ok", "", ""},
		{"a tick with nothing to do is not a stall", DispatchRecord{Active: true, Ticks: []DispatchTick{seeded(ago(6*time.Minute), 0), idle}}, Green, "idle", "no candidates", ""},
		{"a full roster is not a stall", DispatchRecord{Active: true, Ticks: []DispatchTick{seeded(ago(6*time.Minute), 0), full}}, Green, "full", "every seat at its cap", ""},
		{"ticks older than the window are not judged", DispatchRecord{Active: true, Ticks: []DispatchTick{seeded(ago(11*time.Minute), 0)}}, Green, "no ticks", "no tick in the last 10m", ""},
		{"a hold outranks a stalled dispatcher", DispatchRecord{Active: true, Hold: "town ESTOP active", Ticks: []DispatchTick{seeded(ago(2*time.Minute), 0)}}, Green, "held", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthy()
			f.dispatch = tc.rec
			r := Compute(context.Background(), inputs(f))
			got := field(t, r, "dispatch")
			if got.Verdict != tc.verdict || got.Value != tc.value || got.Tag != Recorded {
				t.Errorf("dispatch = %+v, want %s %q RECORDED", got, tc.verdict, tc.value)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("dispatch detail = %q, want it to carry %q", got.Detail, tc.detail)
			}
			if tc.linePart == "" {
				if line := Line(r, now, DefaultStaleAfter); strings.Contains(line, "dispatch=") {
					t.Errorf("the line reports an unremarkable dispatcher: %q", line)
				}
				return
			}
			if line := Line(r, now, DefaultStaleAfter); !strings.Contains(line, tc.linePart) {
				t.Errorf("line = %q, want it to carry %q", line, tc.linePart)
			}
		})
	}
}

func TestShort(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		-time.Second:                   "0s",
		850 * time.Millisecond:         "850ms",
		42 * time.Second:               "42s",
		7*time.Minute + 59*time.Second: "7m",
		3 * time.Hour:                  "3h",
		47 * time.Hour:                 "47h",
		72 * time.Hour:                 "3d",
	} {
		if got := Short(d); got != want {
			t.Errorf("Short(%v) = %q, want %q", d, got, want)
		}
	}
}
