package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/jev/failcause"
)

// FailcauseBattery is the failing-test cause battery's name, its --battery value.
const FailcauseBattery = "failcause"

// The rules that label a cause line, by the name --json reports.
const (
	ruleFailcauseRecurred = "failcause:recurred-within-14d"
	ruleFailcauseQuiet    = "failcause:quiet-14d"
)

// qWillRecur is the question id the cause battery logs its will_recur noul
// under, named by the failcause package.
const qWillRecur QuestionID = failcause.QWillRecur

func init() {
	Register(FailcauseBattery, func(townRoot string) Adapter {
		return newFailcauseAdapter(
			failcause.LogPath(townRoot),
			failcause.CIFailureLedgerPath(townRoot),
			time.Now,
		)
	})
}

// failcauseAdapter labels each cause line's will_recur from the CI-failure
// ledger the landing worker writes. It reads and never writes. The ledger is
// read once, on the first Label.
type failcauseAdapter struct {
	log        string
	ledgerPath string
	now        func() time.Time

	// lines holds each parsed line by item ID, so Label has the test and the
	// time the window starts from.
	lines map[string]failcause.Line

	loaded   bool
	ledger   failcause.CILedger
	ledgerOK bool
}

func newFailcauseAdapter(log, ledgerPath string, now func() time.Time) *failcauseAdapter {
	return &failcauseAdapter{log: log, ledgerPath: ledgerPath, now: now, lines: map[string]failcause.Line{}}
}

func (a *failcauseAdapter) Battery() string { return FailcauseBattery }
func (a *failcauseAdapter) LogPath() string { return a.log }

// failcauseID names one judged test: its landing, the test, and the moment it
// was judged, so a bead that fails the same test again is its own item.
func failcauseID(l failcause.Line) string {
	return l.Rig + "/" + l.Bead + "/" + l.Package + " " + l.Test + "@" + l.TS.UTC().Format(time.RFC3339Nano)
}

// Parse reads one cause line. A line that has no answers, or was asked under
// another question set, cannot be compared with this one, so it is skipped.
func (a *failcauseAdapter) Parse(raw []byte) (Item, error) {
	var l failcause.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || l.Package == "" || l.Test == "" || l.TS.IsZero() {
		return Item{}, errors.New("failcause line needs a rig, a bead, a package, a test and a ts")
	}
	if l.QuestionVersion != failcause.Version {
		return Item{}, fmt.Errorf("failcause line asked question set %d, the scorecard reads %d", l.QuestionVersion, failcause.Version)
	}
	if l.Answers == nil {
		return Item{}, errors.New("failcause line has no answers")
	}
	it := Item{
		ID:      failcauseID(l),
		Time:    l.TS,
		Model:   l.Model,
		Answers: make(map[QuestionID]Answer, len(l.Answers)),
	}
	for id, ans := range l.Answers {
		it.Answers[QuestionID(id)] = Answer{Noul: ans.Noul, Choice: ans.Choice, Probabilities: ans.Probabilities}
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label sets will_recur for one judged test from the CI-failure ledger. A
// repeat of the test on a different bead within fourteen days of the judging
// is true, whatever the ledger holds now, since a record that was written
// cannot be unwritten. Past the window with no repeat, will_recur is false,
// but only when the ledger's last write came strictly before the judging plus
// the shortest retention the ledger has kept: then no write, whatever its
// retention, can have pruned a record the window could hold. A write at that
// instant or later may have pruned a repeat, so that case is unlabeled, and so
// is any test whose window is still open.
func (a *failcauseAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the failcause adapter", item.ID)
	}
	if err := a.load(); err != nil {
		return Labeling{}, err
	}
	start := line.TS
	end := start.Add(failcause.WillRecurWindow)
	if repeatWithin(a.ledger.Records, line, start, end) {
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qWillRecur: true}, Rule: ruleFailcauseRecurred}, nil
	}
	if a.now().Before(end) {
		return Labeling{}, nil
	}
	// The last write must be strictly before the boundary. A write exactly at
	// it cannot have pruned a record after start, but the ledger holds only an
	// mtime: the daemon takes its clock, then queues on the ledger lock, then
	// rewrites, and a filesystem may floor the timestamp, so a recorded instant
	// at the boundary cannot be told from a write that came after it.
	if !a.ledgerOK || !a.ledger.Modified.Before(start.Add(failcause.CIFailureRetentionFloor)) {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: map[QuestionID]bool{qWillRecur: false}, Rule: ruleFailcauseQuiet}, nil
}

// load reads the CI-failure ledger once.
func (a *failcauseAdapter) load() error {
	if a.loaded {
		return nil
	}
	ledger, ok, err := failcause.ReadCILedger(a.ledgerPath)
	if err != nil {
		return fmt.Errorf("reading the CI-failure ledger: %w", err)
	}
	a.ledger, a.ledgerOK, a.loaded = ledger, ok, true
	return nil
}

// repeatWithin reports whether the ledger holds a failure of l's test on a
// bead other than l's, judged after start and no later than end.
func repeatWithin(recs []failcause.CIRecord, l failcause.Line, start, end time.Time) bool {
	for _, r := range recs {
		if r.Unparsed || r.Rig != l.Rig || r.Package != l.Package || r.Test != l.Test {
			continue
		}
		if r.Bead == "" || r.Bead == l.Bead {
			continue
		}
		if r.Time.After(start) && !r.Time.After(end) {
			return true
		}
	}
	return false
}
