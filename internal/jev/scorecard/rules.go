package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/rules"
)

// RulesBattery is the rules battery's name, its --battery value.
const RulesBattery = "rules"

// ruleWindow is how long after a finished session a guard record counts
// against it. A line younger than the window stays unlabeled, since a record
// may still be coming, and once the window has passed with no record, and every
// source was readable, the line is labeled not broken.
const ruleWindow = 20 * time.Minute

// The rules that label a rules line, by the name --json reports.
const (
	ruleRulesRevertGuard = "rules:revert-guard"
	ruleRulesStandDown   = "rules:rework-stand-down"
	ruleRulesNoRecord    = "rules:no-guard-record"
)

// reworkStandDownPrefix opens the close-skipped comment gt done writes when a
// rework stands down because its head is the rejected one (gt-3e1z4). The
// comment text is the record: no structured file holds it.
const reworkStandDownPrefix = "DONE_CLOSE_SKIPPED: head unchanged since the rejection"

// The question ids the rules battery logs, named by the rules package.
const (
	qBrokeStandingRule QuestionID = rules.QBrokeStandingRule
	qRuleKind          QuestionID = rules.QRuleKind
)

func init() {
	Register(RulesBattery, func(townRoot string) Adapter {
		stores := &townNotes{town: townRoot, stores: map[string]*beads.Beads{}}
		var storesMu sync.Mutex
		return &rulesAdapter{
			town: townRoot,
			comments: func(rig, bead string) ([]beads.Comment, error) {
				return beadComments(stores, &storesMu, rig, bead)
			},
			lines: map[string]rules.Line{},
			now:   time.Now,
		}
	})
}

// rulesAdapter labels each logged session from the guard records the town
// keeps for its bead: the revert guard's refusal ledger, and the close-skipped
// comments of a rework stand-down. It reads and never writes.
type rulesAdapter struct {
	town string
	// comments reads a bead's comments in its rig.
	comments func(rig, bead string) ([]beads.Comment, error)
	// lines holds each parsed line by item ID, so Label has the line's bead,
	// rig and submission time.
	lines map[string]rules.Line
	now   func() time.Time

	// refusals is the revert guard's ledger, read once on the first label.
	refusals     []attention.Refusal
	refusalsRead bool
	// sessionsRead counts the sessions whose bead comments Label read.
	sessionsRead int
	// commentCache holds each bead's comments by commentKey, once read by
	// Prefetch or by Label. A failed read is not kept.
	commentCache map[string][]beads.Comment
	// warn tells the operator about a failed prefetch read; nil means stderr
	// through warnf. A test sets it to see the warning without swapping
	// os.Stderr.
	warn func(format string, args ...any)
}

// rulesPrefetchWorkers caps how many comment reads Prefetch runs at once. Each
// read is a bd subprocess, and the host is shared, so the pool stays small.
const rulesPrefetchWorkers = 4

// commentKey names a bead's comments in the cache. Parse rejects a slash in a
// bead id, so the last slash in a key divides its rig from its bead.
func commentKey(rig, bead string) string {
	return rig + "/" + bead
}

func (a *rulesAdapter) Battery() string { return RulesBattery }
func (a *rulesAdapter) LogPath() string { return rules.LogPath(a.town) }

// rulesID names one logged session: its bead and the moment it was submitted,
// so a bead resubmitted after a refusal is its own item.
func rulesID(l rules.Line) string {
	return l.Bead + "@" + l.DoneAt.UTC().Format(time.RFC3339Nano)
}

// Parse reads one rules line. A session that was not asked has no answers, and
// one from an older question set cannot be compared with this one, so both are
// skipped and counted.
func (a *rulesAdapter) Parse(raw []byte) (Item, error) {
	var l rules.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || strings.ContainsAny(l.Bead, `/\`) || l.TS.IsZero() || l.DoneAt.IsZero() {
		return Item{}, errors.New("rules line needs a rig, a bead, a ts and a done_at")
	}
	if l.QuestionVersion != rules.Version {
		return Item{}, fmt.Errorf("rules line asked question set %d, the scorecard reads %d", l.QuestionVersion, rules.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("rules line has no answers: %s", l.Reason)
	}
	it := Item{
		ID:      rulesID(l),
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

// Label finds the labels for one logged session. A session younger than the
// window is unlabeled. Otherwise, a guard record for its bead inside the window
// after the submission, and written for the session's own polecat, labels it
// broke_standing_rule true. A window that has passed with no record labels it
// false, but only when every source was read: a bead the store does not hold,
// a stand-down comment whose time cannot be read, or a record with no polecat
// to match it by, leaves the session unlabeled, since the record may be there.
// The rule that set the label is named with each source that matched, joined
// by "+".
//
// Only broke_standing_rule is labeled. rule_kind has no guard record that
// names a kind, so it is never labeled here.
func (a *rulesAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the rules adapter", item.ID)
	}
	if !a.windowClosed(line) {
		return Labeling{}, nil
	}
	refusals, err := a.readRefusals()
	if err != nil {
		return Labeling{}, err
	}
	comments, err := a.commentsOf(line.Rig, line.Bead)
	// A bead the store does not hold has no comments to read, so its stand-down
	// records are unreadable, not absent.
	unreadable := errors.Is(err, beads.ErrNotFound)
	if err != nil && !unreadable {
		return Labeling{}, fmt.Errorf("reading the comments of %s: %w", line.Bead, err)
	}
	if !unreadable {
		a.sessionsRead++
	}

	end := line.DoneAt.Add(ruleWindow)
	var sources []string
	// A refusal names the polecat whose submission it refused, and a stand-down
	// comment is written as the polecat that ran gt done. Another polecat's record
	// belongs to that polecat's session. A record that names no polecat, or a line
	// with none, cannot be matched, so it is not read as this session's.
	for _, ref := range refusals {
		if ref.Kind != attention.KindRevertGuard || ref.Bead != line.Bead || !inRuleWindow(ref.TS, line.DoneAt, end) {
			continue
		}
		if isOwnSubmission(ref, line) {
			continue
		}
		if ref.Worker == "" || line.Polecat == "" {
			unreadable = true
			continue
		}
		if ref.Worker == line.Polecat {
			sources = append(sources, ruleRulesRevertGuard)
			break
		}
	}
	for _, c := range comments {
		if !strings.HasPrefix(c.Text, reworkStandDownPrefix) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil {
			unreadable = true
			continue
		}
		if !inRuleWindow(at, line.DoneAt, end) {
			continue
		}
		if c.Author == "" || line.Polecat == "" {
			unreadable = true
			continue
		}
		// A polecat's comment carries its BD_ACTOR, gastown/polecats/<name>, as
		// bd comments shows it (gt-8hla5).
		if c.Author == line.Rig+"/polecats/"+line.Polecat {
			if at.Equal(line.DoneAt) {
				// A comment at the submission instant may be the line's own source,
				// as a refusal at that instant may be (see isOwnSubmission).
				continue
			}
			sources = append(sources, ruleRulesStandDown)
			break
		}
	}

	if len(sources) == 0 {
		if unreadable {
			return Labeling{}, nil
		}
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qBrokeStandingRule: false}, Rule: ruleRulesNoRecord}, nil
	}
	return Labeling{Labeled: true, Labels: map[QuestionID]bool{qBrokeStandingRule: true}, Rule: strings.Join(sources, "+")}, nil
}

// windowClosed reports whether a line's window has passed, so Label reads its
// bead. A line still inside the window stays unlabeled.
func (a *rulesAdapter) windowClosed(line rules.Line) bool {
	return a.now().Sub(line.DoneAt) >= ruleWindow
}

// Prefetch reads the comments of the beads Label will read, once per (rig,
// bead), with a pool of rulesPrefetchWorkers reads. It reads the bead of every
// parsed line whose window has closed, which covers each bead Label reads for a
// line that reaches that read (Label can stop at an earlier check, so Prefetch
// may read a bead Label never asks for). A read that fails is left out of the
// cache, so Label reads that bead itself and answers as it always did, and each
// failure is named on stderr.
func (a *rulesAdapter) Prefetch(_ context.Context, items []Item) {
	type pair struct{ rig, bead string }
	var pairs []pair
	seen := map[string]bool{}
	for _, it := range items {
		line, ok := a.lines[it.ID]
		if !ok || !a.windowClosed(line) {
			continue
		}
		key := commentKey(line.Rig, line.Bead)
		if seen[key] {
			continue
		}
		seen[key] = true
		pairs = append(pairs, pair{line.Rig, line.Bead})
	}

	type result struct {
		comments []beads.Comment
		err      error
	}
	// Each worker writes only the results index it was handed, so the results
	// need no lock, and the cache is filled below in pair order, not in the order
	// the reads finished.
	results := make([]result, len(pairs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(rulesPrefetchWorkers, len(pairs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i].comments, results[i].err = a.comments(pairs[i].rig, pairs[i].bead)
			}
		}()
	}
	for i := range pairs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	warn := a.warn
	if warn == nil {
		warn = warnf
	}
	for i, p := range pairs {
		if results[i].err != nil {
			warn("prefetching the comments of %s/%s: %v", p.rig, p.bead, results[i].err)
			continue
		}
		a.cacheComments(p.rig, p.bead, results[i].comments)
	}
}

// commentsOf returns a bead's comments from the cache, or reads them and keeps
// them. A failed read is not kept, so the next Label tries the bead again.
func (a *rulesAdapter) commentsOf(rig, bead string) ([]beads.Comment, error) {
	if c, ok := a.commentCache[commentKey(rig, bead)]; ok {
		return c, nil
	}
	comments, err := a.comments(rig, bead)
	if err != nil {
		return nil, err
	}
	a.cacheComments(rig, bead, comments)
	return comments, nil
}

// cacheComments keeps one bead's comments for the labels that follow.
func (a *rulesAdapter) cacheComments(rig, bead string, comments []beads.Comment) {
	if a.commentCache == nil {
		a.commentCache = map[string][]beads.Comment{}
	}
	a.commentCache[commentKey(rig, bead)] = comments
}

// Context says which guard sources the labels were read from, and which rule
// has no record. The report prints these lines beside the metrics.
func (a *rulesAdapter) Context() []string {
	return []string{
		fmt.Sprintf("rules: guard sources read: revert-guard refusals (refusals.jsonl and refusals.jsonl.1), %d record(s); rework stand-down comments, for %d session(s)",
			len(a.refusals), a.sessionsRead),
		"rules: BLOCKED mail is not read: no code path writes a record for it",
		fmt.Sprintf("rules: %s not-yet: no guard record names the rule a session broke", qRuleKind),
	}
}

// readRefusals returns the revert guard's ledger, read once, oldest first. The
// ledger rotates to refusals.jsonl.1 at its size limit, and attention reads only
// the current file, so the rotated one is read here too. Without it a refusal
// rotated away would leave its line with no record.
func (a *rulesAdapter) readRefusals() ([]attention.Refusal, error) {
	if a.refusalsRead {
		return a.refusals, nil
	}
	rotated, err := attention.ReadRefusalFile(attention.RotatedRefusalsPath(a.town))
	if err != nil {
		return nil, fmt.Errorf("reading the rotated refusal ledger: %w", err)
	}
	current, err := attention.ReadRefusalFile(attention.RefusalsPath(a.town))
	if err != nil {
		return nil, fmt.Errorf("reading the refusal ledger: %w", err)
	}
	a.refusals = append(rotated, current...)
	a.refusalsRead = true
	return a.refusals, nil
}

// inRuleWindow reports whether at falls from the submission to the window's end.
func inRuleWindow(at, from, end time.Time) bool {
	return !at.Before(from) && !at.After(end)
}

// isOwnSubmission reports whether ref is the refusal the line was made from. A
// line made from a refusal takes that refusal's worker and time as its
// submission, so the refusal sits on the window's start and is not evidence
// about the session it made.
func isOwnSubmission(ref attention.Refusal, line rules.Line) bool {
	return ref.Worker == line.Polecat && ref.TS.Equal(line.DoneAt)
}

// beadComments reads a bead's comments from its rig's store. A bead the store
// does not hold returns beads.ErrNotFound, so Label leaves its session
// unlabeled rather than reading it as having no record. The store map is
// filled lazily, so mu covers the lookup, and the comment read runs outside it:
// Prefetch's workers read in parallel.
func beadComments(stores *townNotes, mu *sync.Mutex, rig, bead string) ([]beads.Comment, error) {
	mu.Lock()
	b, err := stores.store(rig)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.Comments(bead)
}
