package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/land"
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
	ruleRulesOmFinding   = "rules:om-rule-finding"
)

// omRuleIDPattern matches a standing rule id in an om finding's title or claim.
// om cites the rules the town enforces by number there, as in the finding
// "Status headers are not in R12 form". The id is case-sensitive and must stand
// alone, so "XR12" and "R12a" do not match.
const omRuleIDPattern = `\bR[0-9]+\b`

var omRuleIDRE = regexp.MustCompile(omRuleIDPattern)

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
			commentsBatch: func(rig string, ids []string) (map[string][]beads.Comment, error) {
				return batchComments(stores, &storesMu, rig, ids)
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
	// commentsBatch reads several of one rig's beads' comments in one query,
	// keyed by bead, for Prefetch. Every id the rig's issues table holds is a
	// key, empty when the bead has no comments. A nil reader leaves every bead
	// to comments.
	commentsBatch func(rig string, ids []string) (map[string][]beads.Comment, error)
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
	// reviews is each bead's first-attempt om verdict, read once on the first
	// label that reaches it.
	reviews     map[string]land.ReviewRecord
	reviewsRead bool
	reviewsErr  error
	// warn tells the operator about a failed prefetch read or a session left
	// unlabeled over the review log; nil means stderr through warnf. A test sets
	// it to see the warning without swapping os.Stderr.
	warn func(format string, args ...any)
}

// rulesPrefetchWorkers caps how many comment reads Prefetch runs at once. Each
// read is a bd subprocess, and the host is shared, so the pool stays small.
const rulesPrefetchWorkers = 4

// commentChunk is how many bead ids Prefetch puts in one comments query, the
// way showChunk bounds a batch of shows (batch.go). One `bd sql` call answers
// for a whole chunk, so a pass costs a handful of calls per rig rather than one
// per bead (gt-ueaagp).
const commentChunk = 100

// beadPair is one bead whose comments Prefetch reads, in its rig.
type beadPair struct{ rig, bead string }

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
// by "+". The om source only adds a true: a session whose attempt-1 verdict,
// later than the submission, has a finding citing a standing rule id is labeled
// broke. It never changes a label to false, and a session no source matches is
// labeled false exactly as it was before the source. An unreadable review log
// leaves a session unlabeled, with a warning, only when no guard or stand-down
// record labels it broke; a session those already label stays broke, and its
// rule names only the sources that were read.
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

	// The om source is read after the others, so an unreadable review log cannot
	// unlabel a session that a guard or stand-down record already labels broke.
	reviews, reviewsErr := a.readReviews()
	if reviewsErr == nil {
		if r, ok := reviews[line.Bead]; ok && r.At.After(line.DoneAt) && citesStandingRule(r) {
			sources = append(sources, ruleRulesOmFinding)
		}
	}

	if len(sources) == 0 {
		if unreadable {
			return Labeling{}, nil
		}
		if reviewsErr != nil {
			// Only the review log can say whether this session is broke, and it
			// cannot be read. The session stays unlabeled and the reading goes on.
			warn := a.warn
			if warn == nil {
				warn = warnf
			}
			warn("leaving %s unlabeled: %v", line.Bead, reviewsErr)
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
// bead). Each rig's beads are read a chunk at a time, one query per chunk.
// Whatever the batch does not answer for is read one bead at a time, with a
// pool of rulesPrefetchWorkers reads: a bead the rig's issues table does not
// hold, or every bead of a chunk whose query failed.
//
// It reads the bead of every parsed line whose window has closed, which covers
// each bead Label reads for a line that reaches that read (Label can stop at an
// earlier check, so Prefetch may read a bead Label never asks for). A read that
// fails is left out of the cache, so Label reads that bead itself and answers
// as it always did, and each failure is named on stderr.
func (a *rulesAdapter) Prefetch(_ context.Context, items []Item) {
	var pairs []beadPair
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
		pairs = append(pairs, beadPair{line.Rig, line.Bead})
	}

	warn := a.warn
	if warn == nil {
		warn = warnf
	}
	// Both reads fill the cache and leave a failure for Label, so a batch that
	// gets a bead wrong costs time and never a label.
	rest := pairs
	if a.commentsBatch != nil {
		rest = a.prefetchBatch(pairs, warn)
	}
	a.prefetchEach(rest, warn)
}

// prefetchBatch caches what the batched read returned and gives back the pairs
// it did not answer for, for prefetchEach to read one at a time. It reads one
// query per rig per chunk of commentChunk ids. A chunk whose query fails is
// warned about, and every pair of it is read one at a time; so is a pair the
// result has no entry for, which is an id the rig's issues table does not hold.
// A bead with no comments is answered, with no comments, and is not read again.
func (a *rulesAdapter) prefetchBatch(pairs []beadPair, warn func(string, ...any)) []beadPair {
	// Group by rig in first-seen order, so the queries and their warnings come
	// out in the same order on every pass.
	var rigs []string
	byRig := map[string][]beadPair{}
	for _, p := range pairs {
		if _, ok := byRig[p.rig]; !ok {
			rigs = append(rigs, p.rig)
		}
		byRig[p.rig] = append(byRig[p.rig], p)
	}
	var rest []beadPair
	for _, rig := range rigs {
		group := byRig[rig]
		for start := 0; start < len(group); start += commentChunk {
			chunk := group[start:min(start+commentChunk, len(group))]
			ids := make([]string, len(chunk))
			for i, p := range chunk {
				ids[i] = p.bead
			}
			got, err := a.commentsBatch(rig, ids)
			if err != nil {
				warn("reading the comments of %s in a batch: %v", rig, err)
				rest = append(rest, chunk...)
				continue
			}
			for _, p := range chunk {
				comments, ok := got[p.bead]
				if !ok {
					rest = append(rest, p)
					continue
				}
				a.cacheComments(p.rig, p.bead, comments)
			}
		}
	}
	return rest
}

// prefetchEach reads one bead at a time with a pool of rulesPrefetchWorkers
// reads. Each worker writes only the results index it was handed, so the
// results need no lock, and the cache is filled in pair order, not in the order
// the reads finished. A failed read is warned about and left uncached.
func (a *rulesAdapter) prefetchEach(pairs []beadPair, warn func(string, ...any)) {
	type result struct {
		comments []beads.Comment
		err      error
	}
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

// readReviews returns each bead's first-attempt om verdict, read once; a failed
// read is kept with its error, so the log is not read again for each session.
// Where a bead has more than one first attempt, the first in the log is kept,
// as dispatch.go keeps it. A review log that cannot be read is an error, which
// Label turns into an unlabeled session where the log decides the label.
func (a *rulesAdapter) readReviews() (map[string]land.ReviewRecord, error) {
	if a.reviewsRead {
		return a.reviews, a.reviewsErr
	}
	a.reviewsRead = true
	recs, err := land.TownReviewsFile(a.town).Since(time.Time{})
	if err != nil {
		a.reviewsErr = fmt.Errorf("reading the review log: %w", err)
		return nil, a.reviewsErr
	}
	first := map[string]land.ReviewRecord{}
	for _, r := range recs {
		if r.Attempt != 1 {
			continue
		}
		if _, seen := first[r.BeadID]; !seen {
			first[r.BeadID] = r
		}
	}
	a.reviews = first
	return first, nil
}

// citesStandingRule reports whether a finding of the verdict cites a standing
// rule id in its title or its claim.
func citesStandingRule(r land.ReviewRecord) bool {
	for _, f := range r.FindingText {
		if omRuleIDRE.MatchString(f.Title) || omRuleIDRE.MatchString(f.Claim) {
			return true
		}
	}
	return false
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

// batchComments reads several of a rig's beads' comments in one query, through
// the store beadComments opens lazily. It holds mu for the whole call, since
// the store map is filled lazily and Prefetch makes these reads one at a time.
func batchComments(stores *townNotes, mu *sync.Mutex, rig string, ids []string) (map[string][]beads.Comment, error) {
	mu.Lock()
	defer mu.Unlock()
	return stores.Comments(rig, ids)
}
