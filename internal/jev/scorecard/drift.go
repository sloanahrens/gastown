package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/drift"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// DriftBattery is the drift adapter's name, its --battery value.
const DriftBattery = "drift"

// driftWindow is how long after an answer a later docs edit still counts as
// making the section stale. A section no edit reaches within it is labeled not
// stale, so the label does not wait on a doc that may never change.
const driftWindow = 30 * 24 * time.Hour

// docTarget is the branch the drift pass reads docs from, so its labels read
// that branch's history.
const docTarget = "refs/heads/main"

// The rules that label a drift item, by the name --json reports. A doc_now_stale
// rule names where the edit came from; drift:kind-in-edit joins one when the
// drift_kind answer is checked against the edit.
const (
	ruleDriftDocEdit    = "drift:docs-edit"
	ruleDriftBeadLanded = "drift:docs-bead-landed"
	ruleDriftNoEdit     = "drift:no-edit-30d"
	ruleDriftKindInEdit = "drift:kind-in-edit"
)

// The Item.Refs keys the drift adapter labels from. The rig is refRig, shared
// with the landing adapter.
const (
	refBead    = "bead"
	refDoc     = "doc"
	refSection = "heading"
)

// The question ids the drift battery logs, named by the drift package.
const (
	qDocNowStale QuestionID = drift.QDocNowStale
	qDriftKind   QuestionID = drift.QDriftKind
)

// Diff and heading patterns. driftHunkRe matches a hunk header, capturing the
// old start and count; the count is absent when it is 1.
var (
	driftHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)
	// driftHeadingRe matches an ATX heading, the same shape the drift battery
	// splits its sections on.
	driftHeadingRe = regexp.MustCompile(`^#{1,6} `)
)

func init() {
	Register(DriftBattery, func(townRoot string) Adapter {
		return newDriftAdapter(
			drift.LogPath(townRoot),
			&townNotes{town: townRoot, stores: map[string]*beads.Beads{}},
			docGit{git: &townClone{town: townRoot}},
			time.Now,
		)
	})
}

// driftBeads lists a rig's beads. townNotes is one.
type driftBeads interface {
	List(rig string, opts beads.ListOptions) ([]*beads.Issue, error)
}

// cloneGit runs git in the bare clone the submission battery owns.
type cloneGit interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// townClone opens the bare clone on first use. Opening can fail, and a failure
// belongs to the label that needed the clone, not to the adapter's factory,
// which cannot return one.
type townClone struct {
	town  string
	clone *submission.Clone
}

// Run runs git in the town's bare clone.
func (c *townClone) Run(ctx context.Context, args ...string) (string, error) {
	if err := c.open(); err != nil {
		return "", err
	}
	return c.clone.Run(ctx, args...)
}

func (c *townClone) open() error {
	if c.clone != nil {
		return nil
	}
	clone, err := submission.OpenClone(c.town)
	if err != nil {
		return err
	}
	c.clone = clone
	return nil
}

// docEdit is one commit on the docs branch that changed a doc file.
type docEdit struct {
	Time time.Time
	// Before is the doc's text at the edit's parent, empty when the edit adds
	// the doc.
	Before string
	// Patch is the edit's zero-context diff of the doc.
	Patch string
}

// docHistory lists the edits to one doc, oldest first.
type docHistory interface {
	Edits(ctx context.Context, doc string) ([]docEdit, error)
}

// docGit reads a doc's edits from the bare clone's docs branch.
type docGit struct {
	git cloneGit
}

// Edits returns each commit on the docs branch that changed doc, oldest first,
// with the doc's text before it and its diff.
func (g docGit) Edits(ctx context.Context, doc string) ([]docEdit, error) {
	if git, ok := g.git.(inputGit); ok {
		return g.editsBatched(ctx, git, doc)
	}
	return g.editsOneByOne(ctx, doc)
}

// editsOneByOne is Edits with three git calls per edit. It is the path for a
// git that cannot take input, and the reference editsBatched must agree with.
func (g docGit) editsOneByOne(ctx context.Context, doc string) ([]docEdit, error) {
	log, err := g.git.Run(ctx, "log", "--reverse", "--no-merges", "--format=%H%x00%P%x00%ct", docTarget, "--", doc)
	if err != nil {
		return nil, err
	}
	var out []docEdit
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return nil, fmt.Errorf("git log for %s: unexpected line %q", doc, line)
		}
		sha, parents := fields[0], strings.Fields(fields[1])
		secs, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git log for %s: bad commit time %q: %w", doc, fields[2], err)
		}
		before, err := g.textAt(ctx, parents, doc)
		if err != nil {
			return nil, err
		}
		patch, err := g.git.Run(ctx, "show", "--format=", "--unified=0", "--no-renames", sha, "--", doc)
		if err != nil {
			return nil, err
		}
		out = append(out, docEdit{Time: time.Unix(secs, 0), Before: before, Patch: patch})
	}
	return out, nil
}

// textAt returns doc at the commit's first parent, or empty when there is no
// parent or the parent does not hold the doc.
func (g docGit) textAt(ctx context.Context, parents []string, doc string) (string, error) {
	if len(parents) == 0 {
		return "", nil
	}
	tree, err := g.git.Run(ctx, "ls-tree", "--name-only", parents[0], "--", doc)
	if err != nil || strings.TrimSpace(tree) == "" {
		return "", err
	}
	return g.git.Run(ctx, "show", parents[0]+":"+doc)
}

// driftAdapter labels each doc section a drift line answered. A section is
// stale when a docs edit within driftWindow after the ask touches it, or when a
// landed bead closed in that window names its doc. It is not stale when no
// such edit or bead appears by the window's end. It reads and never writes.
type driftAdapter struct {
	log   string
	beads driftBeads
	docs  docHistory
	now   func() time.Time

	// since is the earliest ask parsed, so the closed-bead listing reaches every
	// ask in the log.
	since time.Time
	// edits holds each doc's edits by path, read once.
	edits map[string][]docEdit
	// closed holds each rig's closed beads, read once.
	closed map[string][]*beads.Issue
}

func newDriftAdapter(log string, b driftBeads, docs docHistory, now func() time.Time) *driftAdapter {
	return &driftAdapter{
		log:    log,
		beads:  b,
		docs:   docs,
		now:    now,
		edits:  map[string][]docEdit{},
		closed: map[string][]*beads.Issue{},
	}
}

func (a *driftAdapter) Battery() string { return DriftBattery }
func (a *driftAdapter) LogPath() string { return a.log }

// driftLine is the part of a drift.Line the scorecard reads.
type driftLine struct {
	TS              time.Time      `json:"ts"`
	Bead            string         `json:"bead"`
	Rig             string         `json:"rig"`
	Sections        []driftSection `json:"sections"`
	QuestionVersion int            `json:"question_version"`
	Model           string         `json:"model"`
}

// driftSection is one doc section a drift line answered.
type driftSection struct {
	Doc     string                `json:"doc"`
	Heading string                `json:"heading"`
	Answers map[QuestionID]Answer `json:"answers"`
}

// Parse is not how the scorecard reads a drift line: one line answers several
// sections, each its own item, so the scorecard calls ParseLines.
func (a *driftAdapter) Parse(_ []byte) (Item, error) {
	return Item{}, errors.New("a drift line answers one item per doc section: read it with ParseLines")
}

// ParseLines reads one drift line into one item per doc section. A line that
// is not from this question set, names no bead, or names a doc outside docs/
// is skipped whole.
func (a *driftAdapter) ParseLines(raw []byte) ([]Item, error) {
	var l driftLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Bead == "" || l.Rig == "" || l.TS.IsZero() {
		return nil, errors.New("drift line needs a bead, a rig and a ts")
	}
	if l.QuestionVersion != drift.Version {
		return nil, fmt.Errorf("drift line asked question set %d, the scorecard reads %d", l.QuestionVersion, drift.Version)
	}
	for _, s := range l.Sections {
		if !validDocPath(s.Doc) {
			return nil, fmt.Errorf("drift line names doc %q outside docs/", s.Doc)
		}
	}
	if a.since.IsZero() || l.TS.Before(a.since) {
		a.since = l.TS
	}
	items := make([]Item, 0, len(l.Sections))
	for i, s := range l.Sections {
		answers := make(map[QuestionID]Answer, len(s.Answers))
		for id, ans := range s.Answers {
			// A choice is scored by its probability for the option Jev picked,
			// so the picked option is the positive one.
			if ans.Choice != "" {
				ans.Positive = ans.Choice
			}
			answers[id] = ans
		}
		items = append(items, Item{
			ID:      fmt.Sprintf("%s/%s@%s#%d", l.Rig, l.Bead, l.TS.UTC().Format(time.RFC3339Nano), i),
			Time:    l.TS,
			Model:   l.Model,
			Answers: answers,
			Refs: map[string]string{
				refRig:     l.Rig,
				refBead:    l.Bead,
				refDoc:     s.Doc,
				refSection: s.Heading,
			},
		})
	}
	return items, nil
}

// Label finds the labels for one doc section. doc_now_stale is true when a docs
// edit within the window touches the section, or a landed bead closed in it
// names the doc, and false when neither appears and the window has passed. A
// section still inside its window with no such edit is unlabeled. drift_kind is
// labeled only when an edit touched the section: true when the kind Jev picked
// is one the edit's diff names.
func (a *driftAdapter) Label(ctx context.Context, item Item) (Labeling, error) {
	doc, heading := item.Refs[refDoc], item.Refs[refSection]
	labels := map[QuestionID]bool{}
	var rules []string

	edits, err := a.editsOf(ctx, doc)
	if err != nil {
		return Labeling{}, err
	}
	if changed, ok := touchingEdit(edits, heading, item.Time); ok {
		labels[qDocNowStale] = true
		rules = append(rules, ruleDriftDocEdit)
		if picked := item.Answers[qDriftKind].Choice; picked != "" {
			labels[qDriftKind] = drift.EditKinds(changed)[picked]
			rules = append(rules, ruleDriftKindInEdit)
		}
	} else {
		landed, err := a.beadLanded(item)
		if err != nil {
			return Labeling{}, err
		}
		switch {
		case landed:
			labels[qDocNowStale] = true
			rules = append(rules, ruleDriftBeadLanded)
		case !a.now().Before(item.Time.Add(driftWindow)):
			labels[qDocNowStale] = false
			rules = append(rules, ruleDriftNoEdit)
		default:
			return Labeling{}, nil
		}
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// editsOf returns a doc's edits, reading them on first use.
func (a *driftAdapter) editsOf(ctx context.Context, doc string) ([]docEdit, error) {
	if e, ok := a.edits[doc]; ok {
		return e, nil
	}
	e, err := a.docs.Edits(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("reading the history of %s: %w", doc, err)
	}
	a.edits[doc] = e
	return e, nil
}

// touchingEdit returns the diff lines of the first edit inside the window after
// ask that touches the section, and whether one does.
func touchingEdit(edits []docEdit, heading string, ask time.Time) (string, bool) {
	for _, e := range edits {
		if !inDriftWindow(e.Time, ask) {
			continue
		}
		if changed, ok := sectionChange(e, heading); ok {
			return changed, true
		}
	}
	return "", false
}

// inDriftWindow reports whether t is after ask and no later than driftWindow
// past it. An edit at the ask's own instant is the state Jev judged, not a
// change after it.
func inDriftWindow(t, ask time.Time) bool {
	return t.After(ask) && !t.After(ask.Add(driftWindow))
}

// beadLanded reports whether a landed bead closed in the window after the ask
// names the item's doc in its title or description. A closed bead whose close
// time does not parse is an error, not a bead that did not land: the rule could
// not check it.
func (a *driftAdapter) beadLanded(item Item) (bool, error) {
	issues, err := a.closedIn(item.Refs[refRig])
	if err != nil {
		return false, err
	}
	doc := item.Refs[refDoc]
	for _, is := range issues {
		closedAt, err := time.Parse(time.RFC3339Nano, is.ClosedAt)
		if err != nil {
			return false, fmt.Errorf("closed bead %s has a close time %q that does not parse: %w", is.ID, is.ClosedAt, err)
		}
		if !inDriftWindow(closedAt, item.Time) {
			continue
		}
		if !stale.IsLandedReason(is.CloseReason) {
			continue
		}
		if namesDoc(is.Title+"\n"+is.Description, doc) {
			return true, nil
		}
	}
	return false, nil
}

// namesDoc reports whether text names doc as a whole path. A longer path that
// holds doc, such as old/docs/a.md or docs/a.md.bak, does not name it, and a
// full stop or comma after the path does.
func namesDoc(text, doc string) bool {
	re := regexp.MustCompile(`(^|[^\w./-])` + regexp.QuoteMeta(doc) + `($|[^\w./-]|\.($|\s))`)
	return re.MatchString(text)
}

// closedIn returns the rig's closed beads closed at or after the earliest ask,
// reading them on first use.
func (a *driftAdapter) closedIn(rig string) ([]*beads.Issue, error) {
	if issues, ok := a.closed[rig]; ok {
		return issues, nil
	}
	issues, err := a.beads.List(rig, beads.ListOptions{Status: statusClosed, ClosedAfter: a.since})
	if err != nil {
		return nil, fmt.Errorf("listing closed beads in %s: %w", rig, err)
	}
	a.closed[rig] = issues
	return issues, nil
}

// validDocPath reports whether doc is a Markdown file under docs/ that git can
// read without an option or a range in its name.
func validDocPath(doc string) bool {
	return strings.HasPrefix(doc, "docs/") && strings.HasSuffix(doc, ".md") && !strings.Contains(doc, "..")
}

// sectionChange returns the diff lines of the hunks of e that change the section
// named heading, and whether any does. A hunk changes the section when its old
// lines overlap the section before the edit, or when it adds the heading line,
// which is how an edit that adds the section shows.
func sectionChange(e docEdit, heading string) (string, bool) {
	spans := sectionSpans(e.Before, heading)
	lines := strings.Split(e.Patch, "\n")
	var changed strings.Builder
	touched := false
	for i := 0; i < len(lines); i++ {
		m := driftHunkRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		oldStart, _ := strconv.Atoi(m[1])
		oldCount := 1
		if m[2] != "" {
			oldCount, _ = strconv.Atoi(m[2])
		}
		var body []string
		for i+1 < len(lines) && !strings.HasPrefix(lines[i+1], "@@") {
			i++
			body = append(body, lines[i])
		}
		if !overlapsAny(spans, oldStart, oldCount) && !addsHeading(body, heading) {
			continue
		}
		touched = true
		for _, l := range body {
			if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
				changed.WriteString(l + "\n")
			}
		}
	}
	return changed.String(), touched
}

// sectionSpans returns the lines each section named heading covers in text, as
// [first, end) line numbers counted from 1. A section runs from its heading to
// the next heading, and a heading inside a code fence is body text. The empty
// heading is the text above the first heading.
func sectionSpans(text, heading string) [][2]int {
	lines := strings.Split(text, "\n")
	type headingLine struct {
		line int
		text string
	}
	var headings []headingLine
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if t, ok := headingText(l); ok {
			headings = append(headings, headingLine{line: i + 1, text: t})
		}
	}
	var spans [][2]int
	first := 1
	for i := 0; i <= len(headings); i++ {
		end := len(lines) + 1
		if i < len(headings) {
			end = headings[i].line
		}
		name := ""
		if i > 0 {
			name = headings[i-1].text
		}
		if name == heading {
			spans = append(spans, [2]int{first, end})
		}
		if i < len(headings) {
			first = headings[i].line
		}
	}
	return spans
}

// overlapsAny reports whether old lines [start, start+count) touch any span. A
// hunk with no old lines is an insertion after line start, which touches a span
// holding that line. A top-of-file insertion (start 0) sits above line 1, so it
// is read as touching the span that starts at line 1.
func overlapsAny(spans [][2]int, start, count int) bool {
	for _, s := range spans {
		if count == 0 {
			at := max(start, 1)
			if s[0] <= at && at < s[1] {
				return true
			}
			continue
		}
		if start < s[1] && start+count > s[0] {
			return true
		}
	}
	return false
}

// addsHeading reports whether a hunk adds a line that is the heading.
func addsHeading(body []string, heading string) bool {
	for _, l := range body {
		if !strings.HasPrefix(l, "+") {
			continue
		}
		if t, ok := headingText(l[1:]); ok && t == heading {
			return true
		}
	}
	return false
}

// headingText returns a heading line's text without its hashes, and whether the
// line is a heading.
func headingText(line string) (string, bool) {
	if !driftHeadingRe.MatchString(line) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimLeft(line, "#")), true
}
