package land

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The post-landing follow-up feed: after a landing om approved at or above the
// rig's own bar, the worker files shaped beads for the minor findings om raised
// and did not block on, de-duplicated against the beads earlier landings filed
// (gt-s01pn). A finding that really must be fixed refuses the landing instead;
// this feed is for the ones worth fixing and not worth holding the queue for
// (gt-4ty22).

const (
	// FollowUpLabel is the label every filed bead wears and the one a later run
	// lists to find the beads it must de-duplicate against. A landing whose own
	// bead wears it files nothing (gt-4ty22).
	FollowUpLabel = "om-followup"

	// DefaultFollowupBackpressure is the open follow-up count at which the feed
	// stops filing for a rig that configures none. It is backpressure rather
	// than a cap, so the count falling back under it resumes the feed on the
	// next landing (gt-4ty22).
	DefaultFollowupBackpressure = 40

	// followUpGroupFindings is the finding count above which a landing's
	// findings file one bead per package rather than one bead in all.
	followUpGroupFindings = 6

	// followUpPriority is the priority a filed bead takes: every finding a
	// passing review leaves is a minor, which is work nobody is waiting on, so
	// it sorts below the queue's own beads.
	followUpPriority = 3

	// followUpTimeout bounds the whole feed — the de-duplication read, the
	// comments and the file. The landing is already recorded when it runs, so a
	// Dolt that is slow or wedged costs a log line and nothing else.
	followUpTimeout = 20 * time.Second

	// FollowUpListLimit bounds the de-duplication read, so the feed's cost does
	// not grow with a town's whole history of follow-ups. A bead past the bound
	// is not de-duplicated against, which costs at most a repeat bead.
	FollowUpListLimit = 1000
)

// FollowupSpec is one follow-up bead to file.
type FollowupSpec struct {
	Title    string
	Body     string
	Labels   []string
	Priority int
}

// FollowupBeads is the beads surface the feed reads and writes.
//
// It stands beside Beads rather than joining it because both methods carry this
// package's own types: Beads is the surface beads.Client itself satisfies
// (`var _ Beads = beads.Client(nil)`), and beadsfake — which land's tests pass
// as Beads — cannot import this package (a test-time import cycle), so a method
// returning ExistingFollowup can never be part of it.
type FollowupBeads interface {
	// CreateFollowup files spec and returns the new bead's id.
	CreateFollowup(spec FollowupSpec) (id string, err error)
	// ListFollowups returns the beads labeled label, open and closed, at most
	// a bounded number of them: the set a landing de-duplicates against.
	ListFollowups(label string) ([]ExistingFollowup, error)
}

// fileFollowups files the follow-up beads this landing earns, or comments on
// the bead an earlier landing already filed for one of its findings (gt-s01pn).
//
// It runs once the landing is recorded and every failure is logged and dropped:
// the record is written, and nothing here may change the result or hold the
// queue past followUpTimeout.
func (l *Lander) fileFollowups(dir string, w Work, res Result) {
	if l.Followups == nil {
		return // the feed is off for this rig
	}
	// Only om's own approval. skipped, error: and overseer: are not verdicts om
	// reached, and a landing it rejected was rejected rather than recorded.
	if res.Verdict.Verdict != VerdictApprove {
		return
	}
	// The floor is the rig's own threshold, read the way the review read it:
	// the landing om approved at its bar is the one worth a follow-up. There is
	// no ceiling — a landing that risks a great deal and still passes leaves as
	// many minors as one that barely passes, and the review stays strictest
	// when every finding of a passing landing is written down (gt-4ty22).
	threshold, err := OMThreshold(dir)
	if err != nil {
		l.logf("%s: could not read the .om.json threshold the follow-up feed floors on (%v); reading it as 0", w.BeadID, err)
	}
	if res.Verdict.Score < threshold {
		return
	}
	findings := followupCandidates(followupFindings(res.Verdict.Findings))
	if len(findings) == 0 {
		return // nothing but info: worth knowing, not worth a bead
	}
	// The feed runs on its own goroutine so its timeout can abandon it, and it
	// hands its log lines back rather than writing them, so a feed that
	// outlives the timeout cannot interleave with the next landing's log
	// (gt-s01pn).
	done := make(chan []string, 1)
	go func() { done <- l.followupFeed(w, res, findings) }()
	select {
	case lines := <-done:
		for _, line := range lines {
			l.logf("%s: %s", w.BeadID, line)
		}
	case <-time.After(followUpTimeout):
		l.logf("%s: the follow-up feed did not finish in %s; the landing is unaffected", w.BeadID, followUpTimeout)
	}
}

// followupFeed is the feed's I/O for one landing: the de-duplication read, the
// comments a repeat earns, and the beads the new findings earn. It returns the
// lines it wants logged and never returns an error — every failure is a line
// the caller logs and the landing does not see.
func (l *Lander) followupFeed(w Work, res Result, findings []FollowupCandidate) []string {
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		// Only a read work bead can rule out that this landing is itself a
		// follow-up, and a follow-up of a follow-up is how the feed would never
		// drain: every fix generates its own new minors.
		return []string{fmt.Sprintf("could not read the work bead to see whether it is a %s bead itself (%v); filing nothing for this landing", FollowUpLabel, err)}
	}
	if beads.HasLabel(issue, FollowUpLabel) {
		return []string{fmt.Sprintf("the bead is a %s follow-up itself, and a follow-up of a follow-up never drains the feed; filing nothing", FollowUpLabel)}
	}
	existing, err := l.Followups.ListFollowups(FollowUpLabel)
	if err != nil {
		// Filing with no view of what exists would file a paraphrase of an
		// existing bead for every finding of every landing, so a failed read
		// files nothing.
		return []string{fmt.Sprintf("could not list the %s beads (%v); filing nothing for this landing", FollowUpLabel, err)}
	}
	// The beads an earlier attempt at this landing filed. They are this
	// landing's own work, so a retry says nothing to them — the "seen again"
	// comment is for a finding another landing raised (gt-4ty22).
	own := followupIDsForCommit(existing, res.LandedCommit)
	open := openFollowups(existing)
	// The backpressure bound is not checked here. A finding that repeats an
	// existing bead earns its "seen again" comment however many beads are open,
	// so only the fresh findings meet the bound, in fileFollowupBeads, which
	// holds back what it cannot file (gt-qaelo).
	var lines []string
	var fresh []FollowupCandidate
	for _, c := range findings {
		switch d := Decide(c, existing, 0); d.Action {
		case DedupeComment:
			// The finding is already filed: the repeat goes on that bead's
			// history, and nothing new is filed for it.
			if own[d.Target] {
				continue
			}
			if err := l.Beads.AddComment(d.Target, followupComment(w, res, c)); err != nil {
				lines = append(lines, fmt.Sprintf("could not comment %s for the finding it repeats (%v)", d.Target, err))
			}
		default:
			fresh = append(fresh, c)
		}
	}
	if len(fresh) == 0 {
		return lines
	}
	return append(lines, l.fileFollowupBeads(w, res, fresh, open)...)
}

// fileFollowupBeads files the beads the fresh findings of one landing earn —
// one for the landing, or one per package — and leaves one comment on the work
// bead naming them. open is the om-followup count the de-duplication read
// found, so the backpressure bound is applied to every bead the landing files
// rather than once to the landing: a landing that splits into three beads
// stops at the bound instead of carrying the queue past it (gt-4ty22). It
// returns the lines it wants logged.
func (l *Lander) fileFollowupBeads(w Work, res Result, fresh []FollowupCandidate, open int) []string {
	groups := followupGroups(fresh)
	bound := l.followupBackpressure()
	var lines []string
	var ids []string
	for i, g := range groups {
		if open+len(ids) >= bound {
			held := 0
			for _, rest := range groups[i:] {
				held += len(rest.Findings)
			}
			lines = append(lines, l.holdBack(open+len(ids), bound, held))
			break
		}
		id, err := l.Followups.CreateFollowup(followupSpec(w, res, g))
		if err != nil {
			lines = append(lines, fmt.Sprintf("could not file the follow-up bead for %s (%v)", findingsPhrase(len(g.Findings)), err))
			continue
		}
		ids = append(ids, id)
		lines = append(lines, fmt.Sprintf("filed %s for %s of the landing at %.2f", id, findingsPhrase(len(g.Findings)), res.Verdict.Score))
	}
	if len(ids) == 0 {
		return lines
	}
	// The work bead is where a reader of the landing finds what its minors
	// became, so the filed beads are named there and nowhere else (gt-4ty22).
	if err := l.Beads.AddComment(w.BeadID, followupFiledComment(ids, res)); err != nil {
		lines = append(lines, fmt.Sprintf("could not comment the filed follow-up onto %s (%v)", w.BeadID, err))
	}
	return lines
}

// holdBack is what the feed says and does when the backpressure bound keeps
// findings from beads: the line it logs, and the item it hands the attention
// sink, every time the bound holds something back (gt-4ty22).
func (l *Lander) holdBack(open, bound, held int) string {
	if l.FollowupAlert != nil {
		l.FollowupAlert(fmt.Sprintf("%d %s beads are open, at the %d-bead backpressure bound: %s of this landing got no bead, and the feed files nothing more until the open count falls back",
			open, FollowUpLabel, bound, findingsPhrase(held)))
	}
	return fmt.Sprintf("%d %s beads are open, at the %d-bead backpressure bound; held back %s for this landing",
		open, FollowUpLabel, bound, findingsPhrase(held))
}

// FollowupBackpressureOrDefault is the open om-followup count at which a feed
// stops filing: the rig's configured count, or DefaultFollowupBackpressure when
// it configures none.
func FollowupBackpressureOrDefault(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultFollowupBackpressure
}

// followupBackpressure is the open follow-up count at which this lander's feed
// stops filing.
func (l *Lander) followupBackpressure() int {
	return FollowupBackpressureOrDefault(l.FollowupBackpressure)
}

// followupGroup is one bead's worth of a landing's findings. Package is the
// package the bead was filed for, and "" when the bead carries a landing whose
// findings all sit in one place.
type followupGroup struct {
	Package  string
	Findings []FollowupCandidate
}

// followupGroups splits a landing's findings into the beads they file: one bead
// for the landing, or one per package when the landing carries more than
// followUpGroupFindings findings or its findings reach more than one package. A
// package's findings are work one worker takes; a landing spread across three
// packages is not.
func followupGroups(findings []FollowupCandidate) []followupGroup {
	byPackage := map[string][]FollowupCandidate{}
	var order []string
	for _, c := range findings {
		pkg := followupPackage(c.Path)
		if _, seen := byPackage[pkg]; !seen {
			order = append(order, pkg)
		}
		byPackage[pkg] = append(byPackage[pkg], c)
	}
	if len(order) <= 1 && len(findings) <= followUpGroupFindings {
		return []followupGroup{{Findings: findings}}
	}
	sort.Strings(order)
	out := make([]followupGroup, 0, len(order))
	for _, pkg := range order {
		out = append(out, followupGroup{Package: pkg, Findings: byPackage[pkg]})
	}
	return out
}

// followupPackage is the package a finding belongs to: the directory its file
// sits in. A finding om named no path for has no package, and so does one in a
// file at the repository root — path.Dir spells that ".", which is not a
// package any worker takes (gt-4ty22).
func followupPackage(fpath string) string {
	fpath = strings.TrimSpace(fpath)
	if fpath == "" {
		return ""
	}
	if dir := path.Dir(path.Clean(fpath)); dir != "." {
		return dir
	}
	return ""
}

// followupFindings keeps the findings a follow-up bead is for: the minors. Any
// other severity is either info, which om already decided is not worth fixing,
// or a major or critical, which refuses the landing back to the author rather
// than reaching a recorded landing (gt-oi4je).
func followupFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if omSeverity(f.Severity) == omSeverityMinor {
			out = append(out, f)
		}
	}
	return out
}

// followupCandidates renders each finding as the candidate the decision reads
// and the body lists, so both read the same text.
func followupCandidates(findings []Finding) []FollowupCandidate {
	out := make([]FollowupCandidate, 0, len(findings))
	for _, f := range findings {
		out = append(out, FollowupCandidate{
			Path: f.Path, Title: f.Title, Severity: f.Severity, Line: f.Line,
			Detail: findingLine(f),
		})
	}
	return out
}

// followupIDsForCommit is the follow-ups that name commit in their body: the
// beads an earlier attempt at this landing filed. A retry files no second bead
// for them and has nothing to say to them either, but only for them — an
// attempt that filed one package of three leaves the other two to the retry,
// so one filed bead never stands for the whole landing (gt-4ty22).
func followupIDsForCommit(existing []ExistingFollowup, commit string) map[string]bool {
	if commit == "" {
		return nil
	}
	var ids map[string]bool
	for _, e := range existing {
		if !strings.Contains(e.Description, commit) {
			continue
		}
		if ids == nil {
			ids = map[string]bool{}
		}
		ids[e.ID] = true
	}
	return ids
}

// openFollowups counts the follow-ups that are still open. Everything a bead
// can be that is not closed is work still waiting, which is what the
// backpressure bound measures.
func openFollowups(existing []ExistingFollowup) int {
	n := 0
	for _, e := range existing {
		if !followupClosed(e) {
			n++
		}
	}
	return n
}

// followupClosed reports whether a follow-up is closed, whatever bd spelled
// the status as.
func followupClosed(e ExistingFollowup) bool {
	return strings.EqualFold(strings.TrimSpace(e.Status), "closed")
}

// followupSpec is the one bead a group of a landing's findings files: the
// title the retired refinery's beads carried, the shaped body, and the labels
// the next landing de-duplicates on — one fingerprint per finding it files for.
func followupSpec(w Work, res Result, g followupGroup) FollowupSpec {
	labels := []string{FollowUpLabel}
	for _, c := range g.Findings {
		labels = append(labels, Fingerprint(c.Path, c.Title))
	}
	return FollowupSpec{
		Title:    followupTitle(w, res, g),
		Body:     followupBody(w, res, g),
		Labels:   labels,
		Priority: followUpPriority,
	}
}

// followupTitle names the first finding, the count when one bead groups
// several, the package a split bead was filed for, and what raised them:
// "Found: <finding> and 2 more in internal/land (om minor on gt-abc approved
// at 0.62)".
func followupTitle(w Work, res Result, g followupGroup) string {
	first := g.Findings[0]
	more := ""
	if n := len(g.Findings) - 1; n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	if g.Package != "" {
		more += " in " + g.Package
	}
	raised := fmt.Sprintf("on %s approved at %.2f", w.BeadID, res.Verdict.Score)
	if severity := strings.TrimSpace(first.Severity); severity != "" {
		raised = "om " + severity + " " + raised
	}
	return fmt.Sprintf("Found: %s%s (%s)", NoteField(first.Title), more, raised)
}

// followupBody is the filed bead's description in the shape the spec
// dispatcher lints: the findings with their location and om's id, and the five
// sections a bead must carry to be dispatchable at all.
func followupBody(w Work, res Result, g followupGroup) string {
	var b strings.Builder
	in := ""
	if g.Package != "" {
		in = " in " + g.Package
	}
	fmt.Fprintf(&b, "## Goal\nFixed: the %s om raised on %s%s and did not block on, from the landing approved at %.2f (landed commit %s).\n",
		findingsPhrase(len(g.Findings)), w.BeadID, in, res.Verdict.Score, res.LandedCommit)
	b.WriteString("\n")
	for _, c := range g.Findings {
		fmt.Fprintf(&b, "- %s\n", c.Detail)
	}
	b.WriteString("\n## Constraints\nOnly the files the findings name. Keep the rig's gate green. Do not widen the change.\n")
	b.WriteString(followupRevertNote)
	b.WriteString("\n## Out of scope\nAnything the findings do not name.\n")
	b.WriteString("\n## Gate\ngt done presubmit; the landing runs make gate\n")
	b.WriteString("\n## Size\none worker, one landing\n")
	b.WriteString("\n## Acceptance\n")
	for _, item := range acceptanceItems(g.Findings) {
		fmt.Fprintf(&b, "- [ ] %s\n", item)
	}
	return b.String()
}

// followupRevertNote is the paragraph every generated bead carries in its
// Constraints. A finding that asks to remove a line an earlier landing added
// makes gt done's revert check refuse the fix, and the polecat was left to find
// that out; five hit it. The text never names the check's override: the
// refusal omits it on purpose because agents read it and bypass the check, and
// no label or keyword is read from a finding (gt-j0puq).
const followupRevertNote = "If the fix removes or rewrites a line an earlier landing added, gt done's revert " +
	"check will refuse it. Do not work around the refusal: say so in a comment on this bead, naming each file " +
	"the fix deletes (or a directory of at least two path components, with a trailing slash), and ask the " +
	"operator for a deletes-by-spec waiver.\n"

// findingResolved opens every acceptance item. A finding's title may state a
// defect or ask for a change, so a box that restates it asks the worker to
// leave the defect in place: om read a correct fix as contradicting such a box
// and rejected it twice (gt-u4xrr). The box names the finding as resolved,
// which a diff that fixes it satisfies whatever the title's grammar (gt-twr9x).
const findingResolved = "Finding resolved: "

// acceptanceItems is the filed bead's acceptance list: one item per finding,
// with the ones past specdispatch.MaxAcceptance folded into the last item,
// which is the most one worker's landing can carry.
func acceptanceItems(findings []FollowupCandidate) []string {
	limit := specdispatch.MaxAcceptance
	if len(findings) <= limit {
		return findingItems(findings)
	}
	out := findingItems(findings[:limit-1])
	return append(out, fmt.Sprintf("%sthe remaining %d findings under Goal", findingResolved, len(findings)-limit+1))
}

// findingItems is one acceptance item per finding: "Finding resolved: <title>",
// and the location when om named one.
func findingItems(findings []FollowupCandidate) []string {
	out := make([]string, 0, len(findings))
	for _, c := range findings {
		item := findingResolved + NoteField(c.Title)
		if loc := location(c.Path, c.Line); loc != "" {
			item += " (" + loc + ")"
		}
		out = append(out, item)
	}
	return out
}

// findingLine renders one finding as the body lists it: the title, the location
// and om's own id, which is what a reader follows back to the verdict. A field
// om left empty is left out rather than shown blank.
func findingLine(f Finding) string {
	var parts []string
	if loc := location(f.Path, f.Line); loc != "" {
		parts = append(parts, "at "+loc)
	}
	if id := strings.TrimSpace(f.ID); id != "" {
		parts = append(parts, "om finding "+id)
	}
	if severity := strings.TrimSpace(f.Severity); severity != "" {
		parts = append(parts, severity)
	}
	line := NoteField(f.Title)
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	return line
}

// location is a finding's path and line as "path:line"; a line om left at zero
// is the path alone, and a finding with no path has no location.
func location(path string, line int) string {
	path = strings.TrimSpace(path)
	switch {
	case path == "":
		return ""
	case line > 0:
		return fmt.Sprintf("%s:%d", path, line)
	}
	return path
}

// followupFiledComment is the line the feed leaves on the work bead naming the
// beads this landing earned, so a reader of the landed work finds its minors
// without reading the follow-up beads themselves.
func followupFiledComment(ids []string, res Result) string {
	what := "follow-up"
	if len(ids) > 1 {
		what = "follow-ups"
	}
	return fmt.Sprintf("%s filed for this landing: %s (om approved at %.2f)", what, strings.Join(ids, ", "), res.Verdict.Score)
}

// followupComment is the line a repeat leaves on the bead that already filed
// the finding: it came back, on this landing, at this score.
func followupComment(w Work, res Result, c FollowupCandidate) string {
	severity := strings.TrimSpace(c.Severity)
	if severity == "" {
		severity = "finding"
	}
	return fmt.Sprintf("seen again on %s (om %s, %.2f, %s)", w.BeadID, severity, res.Verdict.Score, shortSHA(res.LandedCommit))
}

// findingsPhrase is "1 finding" or "3 findings", the count the body and the
// feed's log line both spell out.
func findingsPhrase(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}
