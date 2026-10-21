package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/jev/testsel"
)

// TestselBattery is the test-selection adapter's name: the --battery value.
const TestselBattery = "testsel"

// testselAffects is the question each asked package is answered on: could the
// change affect behavior this package's tests verify. The battery names its
// questions by package directory, so the scorecard reads them as one question.
const testselAffects QuestionID = "affects_tests"

// Refs the testsel adapter keeps in Item.Refs. refGate is unset when the gate
// reported no result for the package, so Label leaves it unlabeled.
const (
	refGate    = "gate"
	gatePassed = "passed"
	gateFailed = "failed"
)

// ruleTestselGate names the rule that labels a package from its gate result.
const ruleTestselGate = "testsel:gate-result"

func init() {
	Register(TestselBattery, func(townRoot string) Adapter {
		return newTestselAdapter(testsel.LogPath(townRoot))
	})
}

// testselLine is the part of a testsel.Line the scorecard reads.
type testselLine struct {
	TS              time.Time          `json:"ts"`
	Bead            string             `json:"bead"`
	Model           string             `json:"model"`
	Noul            map[string]float64 `json:"noul"`
	Gate            map[string]bool    `json:"gate"`
	QuestionVersion int                `json:"question_version"`
}

// testselCut is the summary of one threshold: the asked packages the battery
// would skip at it, how many of those the gate reported no result for, and how
// many the gate failed (a MISS).
type testselCut struct {
	threshold float64
	skip      int
	ungraded  int
	miss      int
}

// testselAdapter labels each asked package from the gate's result for it, and
// keeps the per-threshold summary the report prints beside the blocks.
type testselAdapter struct {
	log string

	mu    sync.Mutex
	asked int
	cuts  []testselCut
}

func newTestselAdapter(log string) *testselAdapter {
	a := &testselAdapter{log: log, cuts: make([]testselCut, len(testsel.Thresholds))}
	for i, t := range testsel.Thresholds {
		a.cuts[i].threshold = t
	}
	return a
}

func (a *testselAdapter) Battery() string { return TestselBattery }
func (a *testselAdapter) LogPath() string { return a.log }

// Parse is not how the scorecard reads a testsel line: one line answers every
// asked package, each its own item, so the scorecard calls ParseLines.
func (a *testselAdapter) Parse(_ []byte) (Item, error) {
	return Item{}, errors.New("a testsel line answers one item per asked package: read it with ParseLines")
}

// ParseLines reads one testsel line into one item per package Jev answered, in
// directory order. A line from another question set, or one that names no
// bead, is skipped whole.
func (a *testselAdapter) ParseLines(raw []byte) ([]Item, error) {
	var l testselLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Bead == "" || l.TS.IsZero() {
		return nil, errors.New("testsel line needs a bead and a ts")
	}
	if l.QuestionVersion != testsel.Version {
		return nil, fmt.Errorf("testsel line asked question set %d, the scorecard reads %d", l.QuestionVersion, testsel.Version)
	}
	dirs := make([]string, 0, len(l.Noul))
	for dir := range l.Noul {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	items := make([]Item, 0, len(dirs))
	for _, dir := range dirs {
		noul := l.Noul[dir]
		item := Item{
			ID:      fmt.Sprintf("%s@%s %s", l.Bead, l.TS.UTC().Format(time.RFC3339Nano), dir),
			Time:    l.TS,
			Model:   l.Model,
			Answers: map[QuestionID]Answer{testselAffects: {Noul: &noul}},
			Refs:    map[string]string{},
		}
		if passed, reported := l.Gate[dir]; reported {
			item.Refs[refGate] = gateFailed
			if passed {
				item.Refs[refGate] = gatePassed
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// Label labels a package from the gate's result for it: a failed package is
// affected (true), a passed one is not (false). A package the gate reported no
// result for is logged but not labeled. Every item counts toward the summary,
// labeled or not.
func (a *testselAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	gate := item.Refs[refGate]
	a.tally(item, gate)
	if gate == "" {
		return Labeling{}, nil
	}
	return Labeling{
		Labeled: true,
		Labels:  map[QuestionID]bool{testselAffects: gate == gateFailed},
		Rule:    ruleTestselGate,
	}, nil
}

// tally adds one asked package to the per-threshold summary.
func (a *testselAdapter) tally(item Item, gate string) {
	ans, ok := item.Answers[testselAffects]
	if !ok || ans.Noul == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	for i := range a.cuts {
		c := &a.cuts[i]
		if *ans.Noul >= c.threshold {
			continue
		}
		c.skip++
		switch gate {
		case "":
			c.ungraded++
		case gateFailed:
			c.miss++
		}
	}
}

// Context reports one summary line per threshold, and clears the tally so the
// next build starts fresh. Gate minutes saved are n/a: the gate's package
// results carry no durations, so the log cannot price a skip in minutes.
func (a *testselAdapter) Context() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.asked == 0 {
		return nil
	}
	lines := make([]string, 0, len(a.cuts))
	for i, c := range a.cuts {
		lines = append(lines, fmt.Sprintf("would skip at %s: %d package(s), %d ungraded, MISS %d, gate minutes saved n/a",
			strconv.FormatFloat(c.threshold, 'f', -1, 64), c.skip, c.ungraded, c.miss))
		a.cuts[i] = testselCut{threshold: c.threshold}
	}
	a.asked = 0
	return lines
}
