// Package attention is the advisory Jev battery for the attention queue. A
// pass takes each item raised since the last pass, asks Jev the question set
// about it (questions.go), and appends one line to a log. It never acks,
// hides, wakes or changes an item, and it writes nothing under
// .runtime/attention: the log is the only thing it writes.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the Jev
// client, the queue read and the spend, so a test can run a pass with fakes
// and no network. Open wires the production versions.
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
}

// Pass is one run of the attention battery, with every dependency injected.
type Pass struct {
	Asker Asker
	// State reads the attention queue. Open reads the town's state.json.
	State func() (attention.State, error)

	// LogPath is the log every answer appends to.
	LogPath string
	// CursorPath remembers the last item a pass asked about.
	CursorPath string

	// SpentToday reports the USD the pass has spent since local midnight.
	SpentToday func() (float64, error)
	// CapUSD is the daily cap: a pass asks nothing more once SpentToday
	// reaches it.
	CapUSD float64

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of items whose call returned answers.
	Asked int
	// Failed is the number of items logged with null answers because their
	// call failed while a later item's call succeeded.
	Failed int
	// Held is the number of failed calls the pass left unlogged, with the
	// cursor before them, so the next pass asks those items again.
	Held int
	// Halted is true when the pass stopped after maxConsecutiveFailures
	// failed calls in a row.
	Halted bool
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The items it did not reach wait for the next pass.
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

// Run does one pass. Items are taken in the order they were first seen, and
// each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about an item twice.
//
// A failed call is held, not logged, until a later call in the pass succeeds:
// then the failure was about its item, and it is logged with null answers and
// passed, so a single item Jev cannot answer does not block the queue. If the
// calls keep failing the cause is an outage, not the items: the pass stops at
// maxConsecutiveFailures and the held items stay behind the cursor for the
// next pass. An item whose call was cut short by ctx is never logged or
// passed either.
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
	// held is the run of failed calls since the last success, in order.
	var held []heldItem
	record := func(h heldItem) error {
		if err := battery.Append(p.LogPath, h.line); err != nil {
			return err
		}
		return battery.WriteCursor(p.CursorPath, battery.Cursor{Time: h.item.FirstSeen, ID: h.item.Key})
	}
	for _, it := range pending(st.Items, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		spent, err := p.SpentToday()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if spent >= p.CapUSD {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, st, it)
		if errors.Is(err, errCallFailed) {
			held = append(held, heldItem{item: it, line: line})
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
		if err := record(heldItem{item: it, line: line}); err != nil {
			return sum, err
		}
		sum.Asked++
	}
	sum.Held = len(held)
	return sum, nil
}

// heldItem is an item whose call failed, with the null-answers line it would
// be logged under.
type heldItem struct {
	item attention.Item
	line Line
}

// errCallFailed marks a Jev call that failed for a reason other than ctx's end.
var errCallFailed = errors.New("jev call failed")

// ask builds the line an item earns. A failed call returns the line with null
// answers and an error wrapping errCallFailed; Run decides whether to log it.
// A failure that a canceled ctx caused says nothing about the item, so ask
// returns ctx's error and no line instead.
func (p *Pass) ask(ctx context.Context, st attention.State, it attention.Item) (Line, error) {
	line := Line{
		TS:              p.now(),
		Key:             it.Key,
		Kind:            it.Kind,
		Severity:        it.Severity,
		Rig:             it.Rig,
		Bead:            it.Bead,
		SHA:             it.SHA,
		FirstSeen:       it.FirstSeen,
		QuestionVersion: QuestionVersion,
	}
	got, err := p.Asker.Ask(ctx, BuildState(it, others(st.Items, it.Key)), Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return line, fmt.Errorf("%w: %v", errCallFailed, err)
	}
	line.Model = got.Model
	urgency := got.Score[QUrgency]
	line.Answers = &Answers{
		NeedsHuman:         got.Noul[QNeedsHuman],
		ClearsOnItsOwn:     got.Noul[QClearsOnItsOwn],
		RoutineActionFixes: got.Noul[QRoutineActionFixes],
		RepeatsOpenItem:    got.Noul[QRepeatsOpenItem],
		Urgency:            Urgency{Score: urgency.Score, Confidence: urgency.Confidence},
	}
	return line, nil
}

// pending keeps the items after the cursor, oldest first. Ties on the first
// seen time break by key, so the cursor orders items completely.
func pending(items []attention.Item, cur battery.Cursor) []attention.Item {
	var out []attention.Item
	for _, it := range items {
		if it.FirstSeen.IsZero() || !after(cur, it.FirstSeen, it.Key) {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// after reports whether an item first seen at seen with key sorts after the
// cursor.
func after(cur battery.Cursor, seen time.Time, key string) bool {
	if seen.Equal(cur.Time) {
		return key > cur.ID
	}
	return seen.After(cur.Time)
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

// BuildState returns the state for one item: the item's values and the other
// open items' keys and summaries, each value a JSON string on its own line, so
// a value with quotes, newlines or instruction-like text stays inside its
// quotes.
func BuildState(it attention.Item, rest []attention.Item) string {
	var b strings.Builder
	b.WriteString("attention item\n")
	field := func(name, value string, max int) {
		fmt.Fprintf(&b, "%s: %s\n", name, quote(truncate(value, max)))
	}
	field("key", it.Key, maxFieldRunes)
	field("kind", string(it.Kind), maxFieldRunes)
	field("severity", string(it.Severity), maxFieldRunes)
	field("rig", it.Rig, maxFieldRunes)
	field("bead", it.Bead, maxFieldRunes)
	field("sha", it.SHA, maxFieldRunes)
	field("summary", it.Summary, maxSummaryRunes)
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
// town's state file, and the spend read from the same ledger. warnings
// receives the ledger's own warnings and may be nil.
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

	client, err := jev.NewClient(append([]jev.Option{
		jev.WithLedger(jev.UsageLedgerPath(townRoot)),
		jev.WithCaller(Caller),
		jev.WithWarnings(warnings),
	}, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}

	return &Pass{
		Asker:      client,
		State:      func() (attention.State, error) { return attention.ReadState(townRoot) },
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		SpentToday: func() (float64, error) { return town.Spent(time.Now) },
		CapUSD:     capUSD,
	}, nil
}
