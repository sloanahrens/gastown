package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// JevSpend is the Jev row of the Cost panel: what the Jev ledger's calls cost
// over the window the spend command was asked for, and the caller that cost
// the most. It is folded from `gt jev spend --json`, whose by_caller map this
// keeps only the leader of.
type JevSpend struct {
	Calls int     `json:"calls"`
	USD   float64 `json:"usd"`
	// Window is the spend command's own --hours, as "24h"; empty when the
	// command line does not say.
	Window string `json:"window,omitempty"`
	// TopCaller is the caller with the most dollars (the most calls on a tie,
	// then the name), empty when the window holds no calls.
	TopCaller      string  `json:"top_caller,omitempty"`
	TopCallerCalls int     `json:"top_caller_calls,omitempty"`
	TopCallerUSD   float64 `json:"top_caller_usd,omitempty"`
	// Periods are the ledger's last hour, today, yesterday and last 168 hours in
	// dollars, which fill the Cost row's other columns. Nil when the spend
	// command does not print them, and the row leaves those columns blank.
	Periods *JevPeriods `json:"periods,omitempty"`
	// ReadFailed and Stale say the figures are the last good ones rather than
	// current: the hub sets them on the value it hands the Cost rows, so the row
	// can say why instead of drawing them as today's. Both are false on the
	// report the reader parses, so a figure from it reads current. They are
	// never sent or read as JSON: the marked copy only feeds the rows, and a
	// report or a state value cannot set them.
	ReadFailed bool `json:"-"`
	Stale      bool `json:"-"`
}

// JevPeriods is the "periods" object of `gt jev spend --json`.
type JevPeriods struct {
	Last1hUSD    float64 `json:"last_1h_usd"`
	TodayUSD     float64 `json:"today_usd"`
	YesterdayUSD float64 `json:"yesterday_usd"`
	Last7dUSD    float64 `json:"last_7d_usd"`
}

// ParseJevSpend folds the JSON `gt jev spend --json` prints into the row.
// Anything that is not that report's object is an error, so a command that
// printed something else reads as a failed read, not as a window of no spend.
func ParseJevSpend(out []byte) (*JevSpend, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '{' {
		return nil, errors.New("jev spend: not a JSON object")
	}
	var rep struct {
		Calls    *int        `json:"calls"`
		USD      *float64    `json:"usd"`
		Periods  *JevPeriods `json:"periods"`
		ByCaller map[string]struct {
			Calls int     `json:"calls"`
			USD   float64 `json:"usd"`
		} `json:"by_caller"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, err
	}
	if rep.Calls == nil || rep.USD == nil {
		return nil, errors.New("jev spend: the report has no calls or usd")
	}
	j := &JevSpend{Calls: *rep.Calls, USD: *rep.USD, Periods: rep.Periods}
	names := make([]string, 0, len(rep.ByCaller))
	for name := range rep.ByCaller {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := rep.ByCaller[name]
		if j.TopCaller == "" || c.USD > j.TopCallerUSD || (c.USD == j.TopCallerUSD && c.Calls > j.TopCallerCalls) {
			j.TopCaller, j.TopCallerCalls, j.TopCallerUSD = name, c.Calls, c.USD
		}
	}
	return j, nil
}

// JevSpendWindow is the window argv asks the spend command for, as "24h": the
// value of its --hours flag, in either the "--hours N" or "--hours=N" spelling.
// It is empty when the flag is absent or not a positive whole number.
func JevSpendWindow(argv []string) string {
	for i, a := range argv {
		v := ""
		switch {
		case a == "--hours" && i+1 < len(argv):
			v = argv[i+1]
		case strings.HasPrefix(a, "--hours="):
			v = strings.TrimPrefix(a, "--hours=")
		default:
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return strconv.Itoa(n) + "h"
		}
		return ""
	}
	return ""
}

// JevScorecard is the Jev batteries' scorecard lines, one per battery in the
// order the cached scorecard holds them. Window is the window the refresh
// scored, as written ("7d"); empty when the cache does not name one.
type JevScorecard struct {
	Batteries []JevBattery `json:"batteries"`
	Window    string       `json:"window,omitempty"`
	// ScoredAt is the cache's top-level scored_at: when the refresh that wrote
	// the file ran. Nil until a cache has been read.
	ScoredAt *time.Time `json:"scored_at,omitempty"`
	// NoScorecard is set when the cache file does not exist yet: no refresh has
	// run, so the page says how to make one instead of drawing a scorecard.
	NoScorecard bool `json:"no_scorecard,omitempty"`
	// Totals is the JEV panel's title line, summed over Batteries by Tally.
	Totals JevTotals `json:"totals"`
	// Unavailable names the batteries whose newest read failed, in Batteries'
	// order, set by Tally. Each is either a placeholder with no line or a
	// kept last good line.
	Unavailable []string `json:"unavailable,omitempty"`
}

// JevTotals sums the batteries for the JEV panel's title line: how many
// batteries, their items and labeled items, and how many questions carry each
// verdict.
type JevTotals struct {
	Batteries int `json:"batteries"`
	Items     int `json:"items"`
	Labeled   int `json:"labeled"`
	Earning   int `json:"earning"`
	NoSignal  int `json:"no_signal"`
	NotYet    int `json:"not_yet"`
}

// MarshalJSON serves the totals' count under both names the state has carried:
// items, the panel's word, and lines, the key /api/state sent before the rename,
// so a reader of the older name gets the same number rather than an absent
// field (gt-5enkg).
func (t JevTotals) MarshalJSON() ([]byte, error) {
	type totals JevTotals
	return json.Marshal(struct {
		totals
		Lines int `json:"lines"`
	}{totals(t), t.Items})
}

// Tally sets Totals and Unavailable from Batteries. The reader calls it once it
// has converted every cached block, and the hub again after it has put last
// good lines back, so the page never counts them itself. A failed battery with
// no line is left out of Totals.Batteries; a kept last good line counts. A battery
// with a Unit is left out of the battery, item and labeled totals, since its
// packages would swamp a headline about items, while its questions still count
// in the verdicts.
func (sc *JevScorecard) Tally() {
	t := JevTotals{}
	sc.Unavailable = nil
	for _, b := range sc.Batteries {
		if b.Failed {
			sc.Unavailable = append(sc.Unavailable, b.Name)
			if !b.Kept {
				continue
			}
		}
		if b.Unit == "" {
			t.Batteries++
			t.Items += b.Items
			t.Labeled += b.Labeled
		}
		for _, q := range b.Questions {
			switch q.Verdict {
			case "earning":
				t.Earning++
			case "no-signal":
				t.NoSignal++
			case "not-yet":
				t.NotYet++
			}
		}
	}
	sc.Totals = t
}

// JevPackagesBattery is the battery whose items are packages rather than log
// lines: the test-selection battery asks one question per package the change
// would have run.
const JevPackagesBattery = "testsel"

// JevBattery is one battery's line: how many items its log holds in the
// window, how many of them a label rule could label, how many its report could
// not answer for, and each question's verdict.
type JevBattery struct {
	Name    string `json:"name"`
	Items   int    `json:"items"`
	Labeled int    `json:"labeled"`
	// Unanswered is how many of the battery's lines the report counted in
	// lines_skipped: they carried no answer the scorecard could score.
	Unanswered int `json:"unanswered,omitempty"`
	// Unit names what Items counts when it is not the other batteries' items:
	// JevPackagesBattery counts packages, so its row says so and Tally leaves it
	// out of the title line.
	Unit      string        `json:"unit,omitempty"`
	Questions []JevQuestion `json:"questions,omitempty"`
	// ScoredAt is when the report behind this line was scored, from the cache
	// block. The page draws its age ("scored 5h ago"); it is nil for a battery
	// a failed refresh left with no block, whose line carries no score.
	ScoredAt *time.Time `json:"scored_at,omitempty"`
	// Failed marks a battery whose newest refresh failed. Kept says the line is
	// then the last good one the cache kept; a Failed battery that is not Kept
	// has no line and only its name is real.
	Failed bool `json:"failed,omitempty"`
	Kept   bool `json:"kept,omitempty"`
}

// MarshalJSON serves a battery's count under both items and lines, for the
// reason JevTotals.MarshalJSON gives (gt-5enkg).
func (b JevBattery) MarshalJSON() ([]byte, error) {
	type battery JevBattery
	return json.Marshal(struct {
		battery
		Lines int `json:"lines"`
	}{battery(b), b.Items})
}

// JevQuestion is a question's verdict: earning, not-yet or no-signal, with the
// scorecard's own figures for the chip's tooltip. A figure the report leaves
// out, or prints as null, is absent rather than zero.
type JevQuestion struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	// Kind is the question's kind as the scorecard names it: noul, choice or
	// score. A score question has no precision, so it has no lift.
	Kind string `json:"kind,omitempty"`
	// Labeled is how many of the battery's labeled lines carried an answer to
	// this question; a question with none prints nothing there.
	Labeled int `json:"labeled,omitempty"`
	// Positives and Negatives split the labeled lines by their label. They are
	// always sent, so a zero count reads as zero, not as absent.
	Positives int `json:"positives"`
	Negatives int `json:"negatives"`
	// BaseRate and Precision are fractions; Precision is at 0.5.
	BaseRate  *float64 `json:"base_rate,omitempty"`
	Precision *float64 `json:"precision,omitempty"`
	// PrecisionAtBest is the precision at the cut with the best F1, the cut the
	// verdict is read at. Absent for a score question and when the cut predicts
	// no yes.
	PrecisionAtBest *float64 `json:"precision_at_best,omitempty"`
}

// jevOffBatteries are the batteries the panel lists as off by name: the town
// runs neither the duplicate-finding pass nor the test-selection battery, so
// neither takes a row. An off battery is listed once, in Off.
var jevOffBatteries = map[string]bool{"dedupe": true, "testsel": true}

// jevPatrolBatteries is the patrol's list. A battery with a report of no items
// that is not on it is off; one that is on it keeps its row while its log is
// empty, so the page shows it as quiet rather than as off.
var jevPatrolBatteries = map[string]bool{"finding": true, "notes": true, "targets": true}

// jevUsedBy names what reads a question's answer, keyed battery.question. A
// question not named here is read by none.
var jevUsedBy = map[string]string{
	"attention.needs_human": "gt attention and dashboard queue order",
	"landing.risk":          "risk-path order",
}

// jevReusedAfter is how much older than the cache's scored_at a battery's
// block must be before the summary names it as reused. A block the refresh
// scored carries the refresh's own instant; a reused one keeps the instant it
// first scored at, which is older. The hour is a margin, not a measured lag.
const jevReusedAfter = time.Hour

// jevLogUnchanged is why a reused block was not scored again: the refresh
// found its log unchanged since the block was scored.
const jevLogUnchanged = "log unchanged"

// JevSummary is the JEV page's summary, built here from the cached scorecard
// and the Jev spend row so the page draws it rather than sorting the batteries
// itself. Every group names its questions as battery.question.
type JevSummary struct {
	// Refreshed is when the refresh that wrote the cache ran.
	Refreshed *time.Time `json:"refreshed,omitempty"`
	// Reused names the batteries whose block a refresh kept, with when the
	// block was scored and why it was not scored again.
	Reused []JevReused `json:"reused,omitempty"`
	// Off names the batteries the panel lists as off, each once. They take no
	// row and appear in no other group.
	Off []string `json:"off,omitempty"`
	// Earning lists the questions whose verdict is earning, in cache order.
	Earning []JevEarning `json:"earning,omitempty"`
	// Watching lists the not-yet questions that have labels of both classes but
	// too few of each to earn.
	Watching []JevNotYet `json:"watching,omitempty"`
	// Stuck groups the not-yet questions that cannot earn for want of labels.
	Stuck JevStuck `json:"stuck"`
	// Retire groups the no-signal questions by battery.
	Retire []JevRetire `json:"retire,omitempty"`
	// Spend is the Jev spend against the configured cap. A figure the reads did
	// not give is absent.
	Spend JevSpendCap `json:"spend"`
}

// JevReused is one battery whose block a refresh kept.
type JevReused struct {
	Battery  string    `json:"battery"`
	ScoredAt time.Time `json:"scored_at"`
	Reason   string    `json:"reason"`
}

// JevEarning is one earning question. Lift is the precision at the cut minus
// the base rate; a score question has none, and NoLift says why.
type JevEarning struct {
	Battery        string   `json:"battery"`
	ID             string   `json:"id"`
	Labeled        int      `json:"labeled"`
	Positives      int      `json:"positives"`
	BaseRate       *float64 `json:"base_rate,omitempty"`
	PrecisionAtCut *float64 `json:"precision_at_cut,omitempty"`
	Lift           *float64 `json:"lift,omitempty"`
	NoLift         string   `json:"no_lift,omitempty"`
	UsedBy         string   `json:"used_by"`
}

// JevNotYet is one not-yet question with labels of both classes.
type JevNotYet struct {
	Name      string `json:"name"`
	Labeled   int    `json:"labeled"`
	Positives int    `json:"positives"`
	Negatives int    `json:"negatives"`
}

// JevStuck is the not-yet questions that cannot earn for want of labels, each
// cause counted and named.
type JevStuck struct {
	NoLabelRule   JevCause `json:"no_label_rule"`
	OnlyPositive  JevCause `json:"only_positive"`
	ZeroPositives JevCause `json:"zero_positives"`
}

// JevCause is one cause's count and the questions it holds.
type JevCause struct {
	Count int      `json:"count"`
	Names []string `json:"names,omitempty"`
}

func (c *JevCause) add(name string) {
	c.Names = append(c.Names, name)
	c.Count = len(c.Names)
}

// JevRetire is one battery's no-signal questions.
type JevRetire struct {
	Battery   string   `json:"battery"`
	Questions []string `json:"questions"`
}

// JevSpendCap is the Jev spend against the cap, as numbers the page can draw.
// Each is a pointer so an absent figure is not sent as zero.
type JevSpendCap struct {
	// Window is the spend command's window, as "24h".
	Window    string   `json:"window,omitempty"`
	Calls     *int     `json:"calls,omitempty"`
	WindowUSD *float64 `json:"window_usd,omitempty"`
	// TodayUSD is the ledger's dollars since local midnight.
	TodayUSD *float64 `json:"today_usd,omitempty"`
	// CapUSD is the configured daily cap.
	CapUSD *float64 `json:"cap_usd,omitempty"`
	// CapUnreadable says the cap is not known this poll, so CapUSD is absent
	// because the read failed or the configured cap is negative, not because no
	// cap is set.
	CapUnreadable bool `json:"cap_unreadable,omitempty"`
}

// BuildJevSummary folds the cached scorecard, the Jev spend row and the
// configured daily cap into the page's summary. It returns nil when there is
// no scorecard to summarize. capUSD is nil when no cap is configured, a cap of
// zero is read the same way, and a negative cap is unreadable.
func BuildJevSummary(sc *JevScorecard, spend *JevSpend, capUSD *float64) *JevSummary {
	if sc == nil {
		return nil
	}
	s := &JevSummary{Spend: jevSpendAgainstCap(spend, capUSD)}
	if sc.ScoredAt != nil {
		at := *sc.ScoredAt
		s.Refreshed = &at
	}
	off := map[string]bool{}
	for _, b := range sc.Batteries {
		if !jevIsOff(b) {
			continue
		}
		off[b.Name] = true
		if !slices.Contains(s.Off, b.Name) {
			s.Off = append(s.Off, b.Name)
		}
	}
	for _, b := range sc.Batteries {
		if off[b.Name] {
			continue
		}
		if sc.ScoredAt != nil && b.ScoredAt != nil && sc.ScoredAt.Sub(*b.ScoredAt) > jevReusedAfter {
			s.Reused = append(s.Reused, JevReused{Battery: b.Name, ScoredAt: *b.ScoredAt, Reason: jevLogUnchanged})
		}
		var retire []string
		for _, q := range b.Questions {
			name := b.Name + "." + q.ID
			switch q.Verdict {
			case "earning":
				s.Earning = append(s.Earning, jevEarningOf(b.Name, q))
			case "not-yet":
				switch {
				case q.Labeled == 0:
					s.Stuck.NoLabelRule.add(name)
				case q.Positives == 0:
					s.Stuck.ZeroPositives.add(name)
				case q.Negatives == 0:
					s.Stuck.OnlyPositive.add(name)
				default:
					s.Watching = append(s.Watching, JevNotYet{Name: name, Labeled: q.Labeled, Positives: q.Positives, Negatives: q.Negatives})
				}
			case "no-signal":
				retire = append(retire, q.ID)
			}
		}
		if len(retire) > 0 {
			s.Retire = append(s.Retire, JevRetire{Battery: b.Name, Questions: retire})
		}
	}
	return s
}

// jevIsOff is whether the panel lists a battery as off: it is one of the
// names the town runs without, or it reports a log with no items and is not on
// the patrol's list. A battery with no report is never off, since its missing
// line is a failed read and not an empty log.
func jevIsOff(b JevBattery) bool {
	if jevOffBatteries[b.Name] {
		return true
	}
	return b.ScoredAt != nil && b.Items == 0 && !jevPatrolBatteries[b.Name]
}

// jevEarningOf is one earning question's row. A question with no precision at
// its cut, or a score question, has no lift, and the row says why.
func jevEarningOf(battery string, q JevQuestion) JevEarning {
	e := JevEarning{Battery: battery, ID: q.ID, Labeled: q.Labeled, Positives: q.Positives,
		BaseRate: q.BaseRate, PrecisionAtCut: q.PrecisionAtBest, UsedBy: "none"}
	if u, ok := jevUsedBy[battery+"."+q.ID]; ok {
		e.UsedBy = u
	}
	switch {
	case q.Kind == "score":
		e.NoLift = "a score question has no precision, so it has no lift"
	case q.PrecisionAtBest != nil && q.BaseRate != nil:
		lift := *q.PrecisionAtBest - *q.BaseRate
		e.Lift = &lift
	default:
		e.NoLift = "the cut has no precision to measure lift on"
	}
	return e
}

// jevSpendAgainstCap is the spend row and the cap as the summary's figures. A
// missing spend row leaves every spend figure absent. A zero cap leaves the cap
// absent, and a negative cap leaves it absent and marks it unreadable.
func jevSpendAgainstCap(spend *JevSpend, capUSD *float64) JevSpendCap {
	var c JevSpendCap
	switch {
	case capUSD != nil && *capUSD < 0:
		c.CapUnreadable = true
	case capUSD != nil && *capUSD > 0:
		limit := *capUSD
		c.CapUSD = &limit
	}
	if spend == nil {
		return c
	}
	calls, usd := spend.Calls, spend.USD
	c.Window = spend.Window
	c.Calls = &calls
	c.WindowUSD = &usd
	if spend.Periods != nil {
		today := spend.Periods.TodayUSD
		c.TodayUSD = &today
	}
	return c
}
