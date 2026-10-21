package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
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
		Calls    *int     `json:"calls"`
		USD      *float64 `json:"usd"`
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
	j := &JevSpend{Calls: *rep.Calls, USD: *rep.USD}
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
// order the reader asked for them. Window is the scorecard command's own
// --since, as written ("7d"); empty when the command line does not say.
type JevScorecard struct {
	Batteries []JevBattery `json:"batteries"`
	Window    string       `json:"window,omitempty"`
	// Totals is the JEV panel's title line, summed over Batteries by Tally.
	Totals JevTotals `json:"totals"`
}

// JevTotals sums the batteries for the JEV panel's title line: how many
// batteries, their lines and labeled lines, and how many questions earned a
// verdict of earning or no-signal.
type JevTotals struct {
	Batteries int `json:"batteries"`
	Lines     int `json:"lines"`
	Labeled   int `json:"labeled"`
	Earning   int `json:"earning"`
	NoSignal  int `json:"no_signal"`
}

// Tally sets Totals from Batteries. The reader calls it once a read has every
// battery's line, so the page never counts them itself.
func (sc *JevScorecard) Tally() {
	t := JevTotals{Batteries: len(sc.Batteries)}
	for _, b := range sc.Batteries {
		t.Lines += b.Lines
		t.Labeled += b.Labeled
		for _, q := range b.Questions {
			switch q.Verdict {
			case "earning":
				t.Earning++
			case "no-signal":
				t.NoSignal++
			}
		}
	}
	sc.Totals = t
}

// JevBattery is one battery's line: how many lines its log holds in the
// window, how many of them a label rule could label, and each question's
// verdict.
type JevBattery struct {
	Name      string        `json:"name"`
	Lines     int           `json:"lines"`
	Labeled   int           `json:"labeled"`
	Questions []JevQuestion `json:"questions,omitempty"`
}

// JevQuestion is a question's verdict: earning, not-yet or no-signal.
type JevQuestion struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
}

// ParseJevScorecard folds the JSON `gt jev scorecard --json` prints for one
// battery into its line. The lines are the parsed items in the window and the
// labeled count is the items a rule labeled. Anything that is not that report's
// object is an error, so a command that printed something else reads as a
// failed read, not as a battery with no log.
func ParseJevScorecard(out []byte) (*JevBattery, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '{' {
		return nil, errors.New("jev scorecard: not a JSON object")
	}
	var rep struct {
		Battery   string `json:"battery"`
		Items     *int   `json:"items"`
		Questions []struct {
			ID      string `json:"id"`
			Verdict string `json:"verdict"`
		} `json:"questions"`
		Lines []struct {
			Labeled bool `json:"labeled"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, err
	}
	if rep.Battery == "" || rep.Items == nil {
		return nil, errors.New("jev scorecard: the report has no battery or items")
	}
	b := &JevBattery{Name: rep.Battery, Lines: *rep.Items}
	for _, l := range rep.Lines {
		if l.Labeled {
			b.Labeled++
		}
	}
	for _, q := range rep.Questions {
		b.Questions = append(b.Questions, JevQuestion{ID: q.ID, Verdict: q.Verdict})
	}
	return b, nil
}

// JevScorecardWindow is the window argv asks the scorecard command for, as
// written: the value of its --since flag in either the "--since N" or
// "--since=N" spelling, empty when the flag is absent.
func JevScorecardWindow(argv []string) string {
	for i, a := range argv {
		switch {
		case a == "--since" && i+1 < len(argv):
			return argv[i+1]
		case strings.HasPrefix(a, "--since="):
			return strings.TrimPrefix(a, "--since=")
		}
	}
	return ""
}
