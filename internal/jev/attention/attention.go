// Package attention is the advisory Jev battery for the attention queue. A
// pass takes each raise since the last pass, asks Jev the question set about
// it (questions.go), and appends one line to a log. It never acks, hides, wakes
// or changes an item, and it writes nothing under .runtime/attention: the log
// is the only thing it writes.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the Jev
// client, the event and queue reads and the spend, so a test can run a pass
// with fakes and no network. Open wires the production versions.
package attention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// Caller names the pass's Jev calls in the usage ledger, so its spend can
	// be told apart from the other jobs that call Jev.
	Caller = "jev-attention"

	// maxOthers caps the other open items quoted beside an item, so one
	// question can compare it without growing with the queue.
	maxOthers = 30

	// maxSummaryRunes and maxFieldRunes cap the values quoted in a question.
	maxSummaryRunes = 500
	maxFieldRunes   = 200

	// maxConsecutiveFailures is how many calls in a row may fail before a pass
	// stops. A run of failures is an outage, not a property of the items, so
	// the pass leaves them for the next pass rather than logging them null.
	maxConsecutiveFailures = 3

	// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap
	// is town-wide: it counts every Jev caller in the usage ledger.
	DefaultMaxUSDPerDay = 1.0
)

// ErrBusy means another pass holds the run lock: a manual pass and the
// daemon's pass share one cursor, so they take turns.
var ErrBusy = errors.New("another jev attention pass is running")

// Asker asks one Ask call. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Settings is the operator's configuration for the pass, read from
// patrols.jev_attention.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests. The
	// scrub_check key sets it (gt-jb20i).
	ScrubCheck bool
}

// Pass is one run of the attention battery, with every dependency injected.
type Pass struct {
	Asker Asker
	// State reads the attention queue for the items still open at pass time.
	// Open reads the town's state.json.
	State func() (attention.State, error)
	// Events reads the transitions a pass asks about. Open reads the town's
	// events.jsonl, the current file only.
	Events func() ([]attention.Event, error)

	// LogPath is the log every answer appends to.
	LogPath string
	// CursorPath remembers the last raise a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Warn receives the pass's warnings. Nil discards them.
	Warn io.Writer
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of raises whose call returned answers.
	Asked int
	// Failed is the number of raises logged with null answers because their
	// call failed while a later raise's call succeeded.
	Failed int
	// Held is the number of failed calls the pass left unlogged, with the
	// cursor before them, so the next pass asks those raises again.
	Held int
	// Halted is true when the pass stopped after maxConsecutiveFailures
	// failed calls in a row.
	Halted bool
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The raises it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// Report is the one-line account of a pass that the command prints and the
// daemon logs. Failed calls are named apart from answered ones, so a pass
// where Jev was down does not read like one where it answered.
func (s Summary) Report() string {
	if s.Started {
		return "first pass; cursor set to now, no items asked"
	}
	out := fmt.Sprintf("%d items answered", s.Asked)
	if s.Failed > 0 {
		out += fmt.Sprintf(", %d logged with null answers after a failed call", s.Failed)
	}
	if s.Held > 0 {
		out += fmt.Sprintf(", %d failed calls left for the next pass", s.Held)
	}
	if s.Halted {
		out += fmt.Sprintf("; stopped after %d failed calls in a row", maxConsecutiveFailures)
	}
	if s.CapReached {
		out += "; the daily cap was reached, the rest wait for the next pass"
	}
	return out
}

// Run does one pass. Raises are taken in the order the event log records them,
// and each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about a raise twice.
//
// A failed call is held, not logged, until a later call in the pass succeeds:
// then the failure was about its raise, and it is logged with null answers and
// passed, so a single raise Jev cannot answer does not block the queue. If the
// calls keep failing the cause is an outage, not the items: the pass stops at
// maxConsecutiveFailures and the held raises stay behind the cursor for the
// next pass. A raise whose call was cut short by ctx is never logged or passed
// either.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	now := p.now()
	cur, ok, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		return sum, err
	}
	if !ok {
		sum.Started = true
		return sum, battery.WriteCursor(p.CursorPath, battery.Cursor{Time: now})
	}

	st, err := p.State()
	if err != nil {
		return sum, fmt.Errorf("reading the attention queue: %w", err)
	}
	events, err := p.Events()
	if err != nil {
		return sum, fmt.Errorf("reading the attention events: %w", err)
	}
	p.warnBehindLog(cur, events)
	// held is the run of failed calls since the last success, in order.
	var held []heldRaise
	record := func(h heldRaise) error {
		if err := battery.Append(p.LogPath, h.line); err != nil {
			return err
		}
		return battery.WriteCursor(p.CursorPath, battery.Cursor{Time: h.ev.TS, ID: h.ev.Key})
	}
	for _, ev := range pending(events, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, st, ev)
		if errors.Is(err, errCallFailed) {
			held = append(held, heldRaise{ev: ev, line: line})
			if len(held) == maxConsecutiveFailures {
				sum.Halted = true
				break
			}
			continue
		}
		if err != nil {
			return sum, err
		}
		for _, h := range held {
			if err := record(h); err != nil {
				return sum, err
			}
			sum.Failed++
		}
		held = nil
		if err := record(heldRaise{ev: ev, line: line}); err != nil {
			return sum, err
		}
		sum.Asked++
	}
	sum.Held = len(held)
	return sum, nil
}

// heldRaise is a raise whose call failed, with the null-answers line it would
// be logged under.
type heldRaise struct {
	ev   attention.Event
	line Line
}

// errCallFailed marks a Jev call that failed for a reason other than ctx's end.
var errCallFailed = errors.New("jev call failed")

// ask builds the line a raise earns. A failed call returns the line with null
// answers and an error wrapping errCallFailed; Run decides whether to log it.
// A failure that a canceled ctx caused says nothing about the raise, so ask
// returns ctx's error and no line instead.
func (p *Pass) ask(ctx context.Context, st attention.State, ev attention.Event) (Line, error) {
	line := Line{
		TS:              p.now(),
		Key:             ev.Key,
		Kind:            ev.Class,
		Severity:        ev.Severity,
		FirstSeen:       ev.TS,
		QuestionVersion: QuestionVersion,
	}
	var live *attention.Item
	var rest []attention.Item
	if it, ok := attention.Find(st, ev.Key); ok {
		live = &it
		rest = others(st.Items, ev.Key)
		line.Rig, line.Bead, line.SHA = it.Rig, it.Bead, it.SHA
	}
	line.Live = live != nil
	got, err := p.Asker.Ask(ctx, BuildState(ev, live, rest), Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return line, fmt.Errorf("%w: %v", errCallFailed, err)
	}
	line.Model = got.Model
	urgency := got.Score[QUrgency]
	line.Answers = &Answers{
		NeedsHuman:      got.Noul[QNeedsHuman],
		ClearsOnItsOwn:  got.Noul[QClearsOnItsOwn],
		RepeatsOpenItem: got.Noul[QRepeatsOpenItem],
		Urgency:         Urgency{Score: urgency.Score, Confidence: urgency.Confidence},
	}
	return line, nil
}

// pending keeps the new raises after the cursor, oldest first. A cleared
// transition asks nothing. Ties on the time break by key, so the cursor orders
// raises completely.
func pending(events []attention.Event, cur battery.Cursor) []attention.Event {
	var out []attention.Event
	for _, ev := range events {
		if ev.State != attention.EventNew || ev.TS.IsZero() || !after(cur, ev.TS, ev.Key) {
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.Before(out[j].TS)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// after reports whether a raise at seen with key sorts after the cursor.
func after(cur battery.Cursor, seen time.Time, key string) bool {
	if seen.Equal(cur.Time) {
		return key > cur.ID
	}
	return seen.After(cur.Time)
}

// warnBehindLog says when the cursor is older than the first line of the
// event log. The lines between them were rotated away unasked, so the pass
// continues from the start of the current file.
func (p *Pass) warnBehindLog(cur battery.Cursor, events []attention.Event) {
	if p.Warn == nil || len(events) == 0 || !events[0].TS.After(cur.Time) {
		return
	}
	fmt.Fprintf(p.Warn, "jev attention: the cursor at %s is older than the first line of events.jsonl (%s); continuing from the start of the file\n",
		cur.Time.Format(time.RFC3339), events[0].TS.Format(time.RFC3339))
}

// others returns up to maxOthers items other than the one with key, in the
// order the queue lists them.
func others(items []attention.Item, key string) []attention.Item {
	var out []attention.Item
	for _, o := range items {
		if o.Key == key {
			continue
		}
		if len(out) == maxOthers {
			break
		}
		out = append(out, o)
	}
	return out
}

// BuildState returns the state for one raise: the event's values, then whether
// the item is still open and, when it is, its rig, bead and SHA and the other
// open items. Each value is a JSON string on its own line, so a value with
// quotes, newlines or instruction-like text stays inside its quotes.
func BuildState(ev attention.Event, live *attention.Item, rest []attention.Item) string {
	var b strings.Builder
	b.WriteString("attention item\n")
	field := func(name, value string, max int) {
		fmt.Fprintf(&b, "%s: %s\n", name, quote(truncate(value, max)))
	}
	handled := string(ev.Handled)
	if handled == "" {
		handled = string(attention.HandledInfo)
	}
	field("key", ev.Key, maxFieldRunes)
	field("kind", string(ev.Class), maxFieldRunes)
	field("severity", string(ev.Severity), maxFieldRunes)
	field("summary", ev.Text, maxSummaryRunes)
	field("rule", ev.Rule, maxFieldRunes)
	field("handled", handled, maxFieldRunes)
	fmt.Fprintf(&b, "wake: %t\n", ev.Wake)
	fmt.Fprintf(&b, "live: %t\n", live != nil)
	var rig, bead, sha string
	if live != nil {
		rig, bead, sha = live.Rig, live.Bead, live.SHA
	}
	field("rig", rig, maxFieldRunes)
	field("bead", bead, maxFieldRunes)
	field("sha", sha, maxFieldRunes)
	fmt.Fprintf(&b, "other open items: %d\n", len(rest))
	for _, o := range rest {
		fmt.Fprintf(&b, "other %s: %s\n", quote(o.Key), quote(truncate(o.Summary, maxSummaryRunes)))
	}
	return b.String()
}

// quote returns s as a JSON string literal on one line. It leaves <, > and &
// as they are, which the default encoder would turn into \u escapes.
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the attention queue read from the
// town's state file, the raises read from its event log, and the spend read
// from the same ledger. warnings receives the pass's warnings and the ledger's
// own, and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	return open(townRoot, s, warnings)
}

// open is Open with extra client options, which a test uses to supply a key
// without reading the operator's key file.
func open(townRoot string, s Settings, warnings io.Writer, extra ...jev.Option) (*Pass, error) {
	price := s.USDPerBillionTokens
	if !(price > 0 && !math.IsInf(price, 0)) {
		return nil, fmt.Errorf("jev_attention.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	if !(capUSD > 0 && !math.IsInf(capUSD, 0)) {
		return nil, fmt.Errorf("jev_attention.max_usd_per_day must be above zero, not %v", capUSD)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}
	client, err := jev.NewClient(append(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     town.AllowsNow,
	}), extra...)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	return &Pass{
		Asker:      client,
		State:      func() (attention.State, error) { return attention.ReadState(townRoot) },
		Events:     func() ([]attention.Event, error) { return attention.ReadEvents(townRoot) },
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     town.AllowsNow,
		Warn:       warnings,
	}, nil
}
