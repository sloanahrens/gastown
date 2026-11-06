package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
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
	// Labeled is how many of the battery's labeled lines carried an answer to
	// this question; a question with none prints nothing there.
	Labeled int `json:"labeled,omitempty"`
	// BaseRate and Precision are fractions; Precision is at 0.5.
	BaseRate  *float64 `json:"base_rate,omitempty"`
	Precision *float64 `json:"precision,omitempty"`
}
