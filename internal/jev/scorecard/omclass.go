package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/jev/omclass"
)

// OMClassBattery is the lesson-class battery's name, its --battery value.
const OMClassBattery = "omclass"

// omclassKeyword is the Item.Refs key for the keyword class a line carries.
const omclassKeyword = "keyword"

func init() {
	Register(OMClassBattery, func(townRoot string) Adapter {
		return newOMClassAdapter(omclass.LogPath(townRoot))
	})
}

// omclassAdapter reads the lesson-class log. The keyword class is not a yes or
// a no, so the binary metrics do not score it. Instead the report says, per
// keyword class, how often Jev's pick agrees with it and what Jev picks when it
// does not (Contextual).
type omclassAdapter struct {
	log string
	// picks holds each parsed finding's keyword and Jev's pick, by item ID.
	// Label records them, and Context tallies them once every line is read.
	picks map[string]omclassPick
}

// omclassPick is one finding's keyword class beside the class Jev picked.
type omclassPick struct {
	keyword string
	choice  string
}

func newOMClassAdapter(log string) *omclassAdapter {
	return &omclassAdapter{log: log, picks: map[string]omclassPick{}}
}

func (a *omclassAdapter) Battery() string { return OMClassBattery }
func (a *omclassAdapter) LogPath() string { return a.log }

// omclassID names one finding: its bead, its attempt and its index in the
// review's finding text.
func omclassID(l omclass.Line) string {
	return l.Rig + "/" + l.Bead + "#" + strconv.Itoa(l.Attempt) + "/" + strconv.Itoa(l.Finding)
}

// Parse reads one finding's line. A line with no keyword, no class answer, or
// a question set this scorecard cannot read cannot be compared with the
// keyword class, so it is skipped.
func (a *omclassAdapter) Parse(raw []byte) (Item, error) {
	var l omclass.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || l.Attempt < 1 || l.TS.IsZero() || l.Keyword == "" {
		return Item{}, errors.New("omclass line needs a rig, a bead, an attempt, a ts and a keyword")
	}
	if !readableQuestionSet(l.QuestionVersion, omclass.Version) {
		return Item{}, fmt.Errorf("omclass line asked question set %d, the scorecard reads %d or older", l.QuestionVersion, omclass.Version)
	}
	if l.Failure != "" {
		return Item{}, errors.New("omclass line records a failed ask, with no class answer")
	}
	ans, ok := l.Answers[omclass.QRuleClass]
	if !ok || ans.Choice == "" {
		return Item{}, errors.New("omclass line has no class answer")
	}
	return Item{
		ID:      omclassID(l),
		Time:    l.TS,
		Model:   l.Model,
		Answers: map[QuestionID]Answer{QuestionID(omclass.QRuleClass): {Choice: ans.Choice, Probabilities: ans.Probabilities}},
		Refs:    map[string]string{omclassKeyword: l.Keyword},
	}, nil
}

// Label records the finding's keyword and Jev's pick for Context. It returns
// no label: a keyword class is not a yes or a no, so nothing here is scored by
// the binary metrics.
func (a *omclassAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	a.picks[item.ID] = omclassPick{
		keyword: item.Refs[omclassKeyword],
		choice:  item.Answers[QuestionID(omclass.QRuleClass)].Choice,
	}
	return Labeling{}, nil
}

// Context reports the agreement with the keyword class: one headline for every
// finding, then one line per keyword class that has findings, with what Jev
// picked instead where it differs.
func (a *omclassAdapter) Context() []string {
	byKeyword := map[string]map[string]int{}
	agreeByKeyword := map[string]int{}
	total, agree := 0, 0
	for _, p := range a.picks {
		total++
		if p.choice == p.keyword {
			agree++
			agreeByKeyword[p.keyword]++
		}
		if byKeyword[p.keyword] == nil {
			byKeyword[p.keyword] = map[string]int{}
		}
		byKeyword[p.keyword][p.choice]++
	}
	lines := []string{fmt.Sprintf("%d findings, Jev agrees with the keyword class on %d (%s)", total, agree, percent(agree, total))}
	keywords := make([]string, 0, len(byKeyword))
	for kw := range byKeyword {
		keywords = append(keywords, kw)
	}
	sort.Strings(keywords)
	for _, kw := range keywords {
		n := 0
		for _, c := range byKeyword[kw] {
			n += c
		}
		lines = append(lines, fmt.Sprintf("keyword %s: %d findings, Jev agrees %d (%s); Jev picks instead: %s",
			kw, n, agreeByKeyword[kw], percent(agreeByKeyword[kw], n), picksText(byKeyword[kw], kw)))
	}
	return lines
}

// picksText lists the picks other than the keyword class, most frequent first,
// or says there were none.
func picksText(picks map[string]int, keyword string) string {
	type pick struct {
		name string
		n    int
	}
	var others []pick
	for name, n := range picks {
		if name != keyword {
			others = append(others, pick{name, n})
		}
	}
	if len(others) == 0 {
		return "none"
	}
	sort.Slice(others, func(i, j int) bool {
		if others[i].n != others[j].n {
			return others[i].n > others[j].n
		}
		return others[i].name < others[j].name
	})
	parts := make([]string, 0, len(others))
	for _, o := range others {
		parts = append(parts, fmt.Sprintf("%s %d", o.name, o.n))
	}
	return strings.Join(parts, ", ")
}

// percent is part of whole as a whole percentage, or n/a with no whole.
func percent(part, whole int) string {
	if whole == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d%%", part*100/whole)
}
