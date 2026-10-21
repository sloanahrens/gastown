// Package dedupe is the advisory duplicate check for new findings. A pass
// takes the findings filed since the last pass, asks Jev which existing bead
// each one duplicates, and appends one line per finding to a log. It never
// edits, labels, comments on, closes or links a bead: the log is the only
// thing it writes, and nothing a polecat reads changes.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the Jev
// client, the hm search, the bead reads and the spend, so a test can run a
// pass with fakes and no network. Open wires the production versions.
package dedupe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// RigName is the rig whose findings a pass reads and whose beads it
	// offers as candidates.
	RigName = "gastown"

	// Caller names the pass's Jev calls in the usage ledger, so its spend can
	// be told apart from the other jobs that call Jev.
	Caller = "jev-dedupe"

	// maxCandidates caps the candidates offered per finding.
	maxCandidates = 5

	// minConfidence is the confidence below which an answer logs as none.
	minConfidence = 0.5

	// maxTitleRunes and maxDescRunes cap the bead values quoted in a question.
	maxTitleRunes = 300
	maxDescRunes  = 600

	// maxRetryWait caps how long a 429's Retry-After can hold a pass.
	maxRetryWait = 30 * time.Second

	// noneOption is the Choice option for no duplicate, and the candidate a
	// logged line names when the pass found none.
	noneOption = "none"
)

// Asker asks one battery of questions about one state. *jev.Client satisfies
// it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Hit is one hm search result: the bead it names and the field that matched.
type Hit struct {
	Kind string
	ID   string
	// Field is the matched chunk's field: "title", "description",
	// "comment:4", "notes" and so on. Empty when hm gave none.
	Field string
}

// Searcher returns the hm hits for a text, best first.
type Searcher interface {
	Search(ctx context.Context, text string) ([]Hit, error)
}

// Beads reads the rig's beads. A pass only reads them.
type Beads interface {
	// Since lists every bead created at or after since.
	Since(since time.Time) ([]*beads.Issue, error)
	// Show returns one bead.
	Show(id string) (*beads.Issue, error)
}

// Pass is one run of the duplicate check, with every dependency injected.
type Pass struct {
	Asker    Asker
	Searcher Searcher
	Beads    Beads

	// LogPath is the suggestion log every answer appends to.
	LogPath string
	// CursorPath remembers the last finding a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Wait pauses before a retry after a 429. Nil means a timer that stops
	// early when ctx is done.
	Wait func(ctx context.Context, d time.Duration) error
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of findings that got a log line this pass.
	Asked int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The findings it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// Run does one pass. Findings are taken in the order they were created, and
// each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about a finding twice. A finding whose question
// was cut short by ctx is neither logged nor passed: the next pass asks it
// again. A finding whose answer was paid for is passed even when its log line
// cannot be written, so the next pass does not bill it again. Run returns
// ErrBusy when another pass holds the run lock.
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

	all, err := p.Beads.Since(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing beads since %s: %w", cur.Time.Format(time.RFC3339), err)
	}
	findings := pending(all, cur)
	for _, f := range findings {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, f)
		if err != nil {
			return sum, err
		}
		if err := battery.Append(p.LogPath, line); err != nil {
			err = fmt.Errorf("writing the dedupe log: %w", err)
			// The answer is paid for. Advance the cursor before reporting,
			// so the next pass does not ask, and bill, the same finding
			// again; the lost log line is the lesser failure.
			if cerr := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: beads.ParseIssueTime(f.CreatedAt), ID: f.ID}); cerr != nil {
				return sum, errors.Join(err, cerr)
			}
			return sum, err
		}
		sum.Asked++
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: beads.ParseIssueTime(f.CreatedAt), ID: f.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// pending keeps the findings after the cursor, oldest first. A finding is a
// bead titled "Found:" or labeled review-deep or om-followup. Ties on the
// creation time break by ID, so the cursor orders findings completely.
func pending(all []*beads.Issue, cur battery.Cursor) []*beads.Issue {
	var out []*beads.Issue
	for _, b := range all {
		created := beads.ParseIssueTime(b.CreatedAt)
		if created.IsZero() || !isFinding(b) || !after(cur, created, b.ID) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		ci := beads.ParseIssueTime(out[i].CreatedAt)
		cj := beads.ParseIssueTime(out[j].CreatedAt)
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// isFinding reports whether a bead is a finding the pass asks about.
func isFinding(b *beads.Issue) bool {
	if strings.HasPrefix(b.Title, "Found:") {
		return true
	}
	for _, l := range b.Labels {
		if l == "review-deep" || l == "om-followup" {
			return true
		}
	}
	return false
}

// after reports whether a bead created at created with id sorts after the
// cursor.
func after(cur battery.Cursor, created time.Time, id string) bool {
	if created.Equal(cur.Time) {
		return id > cur.ID
	}
	return created.After(cur.Time)
}

// ask builds the one line a finding earns. Every finding is asked the Nouls
// and the severity Score, and the duplicate Choice and any_duplicate too when
// the search offered a candidate. A failed search, a failed call, an unusable
// answer, a low-confidence answer and a candidate set that could not be read
// all log none, so a finding is asked about once whatever Jev does; the
// line's outcome says which of them it was. The one exception is a canceled
// ctx: a failure that ctx's end caused says nothing about the finding, so ask
// returns ctx's error and no line, and the next pass asks again.
func (p *Pass) ask(ctx context.Context, f *beads.Issue) (Line, error) {
	line := Line{
		TS:                p.now(),
		Finding:           f.ID,
		Candidate:         noneOption,
		CandidatesOffered: []string{},
		QuestionVersion:   Version,
	}
	hits, searchErr := p.Searcher.Search(ctx, f.Title)
	if searchErr != nil && ctx.Err() != nil {
		return Line{}, ctx.Err()
	}
	var cands []*beads.Issue
	var unreadable bool
	if searchErr == nil {
		cands, unreadable = p.candidates(f, hits)
	}
	for _, c := range cands {
		line.CandidatesOffered = append(line.CandidatesOffered, c.ID)
	}

	state, qs := buildQuestions(f, cands)
	got, err := p.askJev(ctx, state, qs)
	if err != nil && ctx.Err() != nil {
		return Line{}, ctx.Err()
	}
	if err == nil {
		line.Model = got.Model
		line.Answers = answersFrom(got)
	}
	// The search's failure outranks the call's: it is why no candidate was
	// offered, and a line with null answers still shows the call failed.
	switch {
	case searchErr != nil:
		line.Outcome = OutcomeSearchFailed
		return line, nil
	case err != nil:
		line.Outcome = OutcomeAskFailed
		return line, nil
	case len(cands) == 0 && unreadable:
		line.Outcome = OutcomeReadFailed
		return line, nil
	case len(cands) == 0:
		line.Outcome = OutcomeNoCandidates
		return line, nil
	}

	ans := got.Choice[QDuplicate]
	line.Confidence = ans.Confidence
	switch {
	case ans.Choice != noneOption && !offered(line.CandidatesOffered, ans.Choice):
		line.Outcome = OutcomeUnknownOption
	case ans.Confidence < minConfidence:
		line.Outcome = OutcomeLowConfidence
	default:
		line.Outcome = OutcomeAnswered
		line.Candidate = ans.Choice
	}
	return line, nil
}

// candidates keeps up to maxCandidates beads from the hits, in rank order.
// It drops the finding itself, any bead created after the finding, and any
// hit that matched on something other than the title or description, such as
// a comment or a close reason. A bead it cannot read is dropped too, and
// unreadable reports that one was, so the pass can tell a failed read from a
// search that found nothing.
func (p *Pass) candidates(f *beads.Issue, hits []Hit) (out []*beads.Issue, unreadable bool) {
	created := beads.ParseIssueTime(f.CreatedAt)
	seen := map[string]bool{f.ID: true}
	for _, h := range hits {
		if len(out) == maxCandidates {
			break
		}
		if h.Kind != "bead" || (h.Field != "title" && h.Field != "description") || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		b, err := p.Beads.Show(h.ID)
		if err != nil || b == nil {
			unreadable = true
			continue
		}
		bc := beads.ParseIssueTime(b.CreatedAt)
		if bc.IsZero() {
			unreadable = true
			continue
		}
		if bc.After(created) {
			continue
		}
		out = append(out, b)
	}
	return out, unreadable
}

// askJev asks the battery about one finding. A 429 gets one retry, after the
// delay the server asked for, capped at maxRetryWait.
func (p *Pass) askJev(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	got, err := p.Asker.Ask(ctx, state, qs)
	if !errors.Is(err, jev.ErrRateLimited) {
		return answered(got, err)
	}
	var wait time.Duration
	var rl *jev.RateLimitError
	if errors.As(err, &rl) {
		wait = min(rl.RetryAfter, maxRetryWait)
	}
	if err := p.wait(ctx, wait); err != nil {
		return nil, err
	}
	return answered(p.Asker.Ask(ctx, state, qs))
}

// answered turns an Ask that returned neither answers nor an error into
// ErrBadResponse, so ask never dereferences nil answers.
func answered(got *jev.Answers, err error) (*jev.Answers, error) {
	if err == nil && got == nil {
		return nil, jev.ErrBadResponse
	}
	return got, err
}

// buildQuestions returns the state and the question set for one finding. The
// state quotes the finding and each candidate, one per line, so the model
// reads each value as data. The duplicate Choice and any_duplicate are asked
// only when the search offered a candidate; the other Nouls and the Score are
// asked every time.
func buildQuestions(f *beads.Issue, cands []*beads.Issue) (string, map[string]jev.Question) {
	var b strings.Builder
	fmt.Fprintf(&b, "finding: %s\n", quote(excerpt(f)))
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		fmt.Fprintf(&b, "candidate %s: %s\n", quote(c.ID), quote(excerpt(c)))
		ids = append(ids, c.ID)
	}
	qs := Questions()
	if len(ids) > 0 {
		qs[QDuplicate] = duplicateChoice(ids)
	} else {
		delete(qs, QAnyDuplicate)
	}
	return b.String(), qs
}

// excerpt is a bead's title and the start of its description.
func excerpt(b *beads.Issue) string {
	s := truncate(b.Title, maxTitleRunes)
	if b.Description != "" {
		s += "\n" + truncate(b.Description, maxDescRunes)
	}
	return s
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

// offered reports whether choice is one of the candidate IDs offered.
func offered(ids []string, choice string) bool {
	for _, id := range ids {
		if id == choice {
			return true
		}
	}
	return false
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *Pass) wait(ctx context.Context, d time.Duration) error {
	if p.Wait != nil {
		return p.Wait(ctx, d)
	}
	return waitFor(ctx, d)
}
