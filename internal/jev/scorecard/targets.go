package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/targets"
)

// TargetsBattery is the review-targeting battery's name: the --battery value.
const TargetsBattery = "targets"

// refRun is the deep-review run bead a targets line names, kept in Item.Refs.
const refRun = "run"

// Rules the targets adapter names in the --json report's lines.
const (
	ruleTargetsLedger = "targets:ledger"
	ruleTargetsClean  = "targets:clean"
)

// The ledger line prefixes scripts/deep-review.sh's formula writes into a run
// bead's notes.
const (
	findingPrefix = "finding: "
	queuedPrefix  = "queued: "
	p3Prefix      = "p3: "
)

func init() {
	Register(TargetsBattery, func(townRoot string) Adapter {
		return &targetsAdapter{
			log:  targets.LogPath(townRoot),
			runs: &townRuns{town: townRoot, stores: map[string]*beads.Beads{}},
		}
	})
}

// runBead is what the scorecard reads of a deep-review run bead.
type runBead struct {
	// Found is false for a bead the store no longer holds; such a run labels
	// nothing.
	Found bool
	// Status is the bead's status. Only a closed run has finished its ledger.
	Status string
	// Notes holds the run's ledger lines.
	Notes string
}

// runLedger reads a deep-review run bead by id.
type runLedger interface {
	Read(bead string) (runBead, error)
}

// townRuns reads a run bead through the beads store of the rig its prefix
// routes to, one store per rig.
type townRuns struct {
	town   string
	stores map[string]*beads.Beads
}

func (t *townRuns) Read(bead string) (runBead, error) {
	rig := beads.GetRigNameForPrefix(t.town, beads.ExtractPrefix(bead))
	if rig == "" {
		rig = "hq" // a town-level bead lives in the town's own store
	}
	store, ok := t.stores[rig]
	if !ok {
		store = beads.New(doltserver.FindRigBeadsDir(t.town, rig))
		t.stores[rig] = store
	}
	issue, err := store.Show(bead)
	if errors.Is(err, beads.ErrNotFound) {
		return runBead{}, nil
	}
	if err != nil {
		return runBead{}, err
	}
	return runBead{Found: true, Status: issue.Status, Notes: issue.Notes}, nil
}

// targetsAdapter labels each logged slice file from the run bead its line
// names: the run's finding, queued and p3 ledger lines name the files the
// review read and found something in.
type targetsAdapter struct {
	log  string
	runs runLedger

	mu      sync.Mutex
	ledgers map[string]ledger
	// order holds the runs in log order, and scored each run's files in the
	// slice's own risk order, for the per-run summary.
	order  []string
	scored map[string][]scoredFile
}

// scoredFile is one file a run scored, with what its ledger says about it.
type scoredFile struct {
	path     string
	found    bool
	priority *float64
}

func (a *targetsAdapter) Battery() string { return TargetsBattery }
func (a *targetsAdapter) LogPath() string { return a.log }

// targetsLine is the part of a targets.Line the scorecard reads.
type targetsLine struct {
	TS              time.Time                 `json:"ts"`
	Run             string                    `json:"run"`
	Path            string                    `json:"path"`
	Model           string                    `json:"model"`
	Answers         map[string]targets.Answer `json:"answers"`
	QuestionVersion int                       `json:"question_version"`
}

// Parse reads one targets log line. A line from another question set answers
// different wordings, so it is skipped and counted rather than compared.
func (a *targetsAdapter) Parse(raw []byte) (Item, error) {
	var line targetsLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return Item{}, err
	}
	if line.Run == "" || line.Path == "" || line.TS.IsZero() {
		return Item{}, errors.New("targets line needs a run, a path and a ts")
	}
	if line.QuestionVersion != targets.Version {
		return Item{}, fmt.Errorf("targets line asked question set %d, the scorecard reads %d",
			line.QuestionVersion, targets.Version)
	}
	answers := make(map[QuestionID]Answer, len(line.Answers))
	for id, ans := range line.Answers {
		answers[QuestionID(id)] = Answer{Noul: ans.Noul, Score: ans.Score}
	}
	return Item{
		ID:      line.Path,
		Time:    line.TS,
		Model:   line.Model,
		Answers: answers,
		Refs:    map[string]string{refRun: line.Run},
	}, nil
}

// Label labels a closed run's every logged file once its ledger is final: a
// file a finding, queued or p3 line names is a defect, every other file in the
// slice is clean. A run still open has not finished recording, so it labels
// nothing.
func (a *targetsAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	run := item.Refs[refRun]
	if run == "" {
		return Labeling{}, fmt.Errorf("item %s names no run", item.ID)
	}
	led, err := a.ledger(run)
	if err != nil {
		return Labeling{}, fmt.Errorf("reading run %s: %w", run, err)
	}
	found := led.files[item.ID]
	a.record(run, item, found)
	if !led.closed {
		return Labeling{}, nil
	}
	rule := ruleTargetsClean
	if found {
		rule = ruleTargetsLedger
	}
	return Labeling{
		Labeled: true,
		Labels: map[QuestionID]bool{
			targets.QLikelyDefect:   found,
			targets.QReviewPriority: found,
		},
		Rule: rule,
	}, nil
}

// ledger reads a run bead once per adapter, so a run's every line reads one
// bead.
func (a *targetsAdapter) ledger(run string) (ledger, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if led, ok := a.ledgers[run]; ok {
		return led, nil
	}
	b, err := a.runs.Read(run)
	if err != nil {
		return ledger{}, err
	}
	led := parseLedger(b)
	if a.ledgers == nil {
		a.ledgers = map[string]ledger{}
	}
	a.ledgers[run] = led
	return led, nil
}

// record keeps one scored file for its run's summary.
func (a *targetsAdapter) record(run string, item Item, found bool) {
	f := scoredFile{path: item.ID, found: found}
	if ans, ok := item.Answers[targets.QReviewPriority]; ok {
		f.priority = ans.Score
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scored == nil {
		a.scored = map[string][]scoredFile{}
	}
	if _, ok := a.scored[run]; !ok {
		a.order = append(a.order, run)
	}
	a.scored[run] = append(a.scored[run], f)
}

// Context reports each run once the labeling is done: the files it scored, the
// files with findings, and where its first finding ranks by Jev's
// review_priority beside its rank in the slice's own risk order, so the two
// rankings can be compared.
func (a *targetsAdapter) Context() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	lines := make([]string, 0, len(a.order))
	for _, run := range a.order {
		lines = append(lines, runSummary(run, a.ledgers[run], a.scored[run]))
	}
	a.order, a.scored = nil, nil
	return lines
}

// ledger is a run bead's verdict on the files in its slice.
type ledger struct {
	found  bool
	closed bool
	// files holds each file a valid ledger line names.
	files map[string]bool
}

// parseLedger reads a run bead's finding, queued and p3 lines. A line whose
// fields do not parse is skipped, as scripts/deep-review.sh skips it: refusing
// the whole ledger would lose the findings it does hold, and a parser that
// rejects the line cannot recover its finding either way.
func parseLedger(b runBead) ledger {
	l := ledger{
		found:  b.Found,
		closed: b.Found && strings.EqualFold(strings.TrimSpace(b.Status), "closed"),
		files:  map[string]bool{},
	}
	for _, raw := range strings.Split(b.Notes, "\n") {
		if file, ok := ledgerLineFile(strings.TrimSpace(raw)); ok {
			l.files[file] = true
		}
	}
	return l
}

// ledgerLineFile returns the file a finding, queued or p3 line names. The file
// is the fingerprint's first token for all three shapes, the token the script
// groups a finding by.
func ledgerLineFile(line string) (string, bool) {
	switch {
	case strings.HasPrefix(line, findingPrefix):
		fields := strings.Fields(line[len(findingPrefix):])
		if len(fields) < 2 { // the fingerprint and the bead, or digest-only
			return "", false
		}
		return fingerprintFile(fields[0])
	case strings.HasPrefix(line, queuedPrefix):
		fields := strings.Fields(line[len(queuedPrefix):])
		if len(fields) < 4 || !priorityField(fields[1]) {
			return "", false
		}
		if !cited(fields[2]) {
			return "", false
		}
		return fingerprintFile(fields[0])
	case strings.HasPrefix(line, p3Prefix):
		fields := strings.Fields(line[len(p3Prefix):])
		if len(fields) < 4 {
			return "", false
		}
		if _, err := time.Parse("2006-01-02", fields[0]); err != nil {
			return "", false
		}
		if !cited(fields[2]) {
			return "", false
		}
		return fingerprintFile(fields[1])
	}
	return "", false
}

// fingerprintFile returns the file in a <file>:<symbol>:<kind> fingerprint.
func fingerprintFile(fp string) (string, bool) {
	first := strings.Index(fp, ":")
	last := strings.LastIndex(fp, ":")
	if first <= 0 || last <= first+1 || last == len(fp)-1 {
		return "", false
	}
	return fp[:first], true
}

// cited reports whether cite carries both halves of a <file>:<line> citation.
func cited(cite string) bool {
	file, line, ok := strings.Cut(cite, ":")
	return ok && file != "" && line != ""
}

// priorityField reports whether sev is the severity a queued line carries.
func priorityField(sev string) bool {
	return sev == "P1" || sev == "P2" || sev == "P3"
}

// runSummary is one run's context line.
func runSummary(run string, led ledger, files []scoredFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s: %s scored", run, fileCount(len(files)))
	switch {
	case !led.found:
		b.WriteString("; the run bead is not in the store, so none is labeled")
		return b.String()
	case !led.closed:
		b.WriteString("; the run bead is still open, so none is labeled")
		return b.String()
	}
	first, findings := 0, 0
	for i, f := range files {
		if f.found {
			findings++
			if first == 0 {
				first = i + 1
			}
		}
	}
	switch findings {
	case 0:
		b.WriteString("; no finding in the ledger")
		return b.String()
	case 1:
		b.WriteString(", 1 with a finding")
	default:
		fmt.Fprintf(&b, ", %d with findings", findings)
	}
	fmt.Fprintf(&b, "; the first finding ranks %d by Jev's review_priority, %d by the slice's risk order",
		priorityRank(files), first)
	return b.String()
}

// priorityRank is the 1-based rank, in Jev's review_priority order, of the
// first file with a finding. A file with no priority answer ranks after every
// file that has one, ties keeping the slice's order.
func priorityRank(files []scoredFile) int {
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		a, b := files[idx[x]].priority, files[idx[y]].priority
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return *a > *b
	})
	for rank, i := range idx {
		if files[i].found {
			return rank + 1
		}
	}
	return 0
}

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}
