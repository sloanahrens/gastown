package land

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The post-landing follow-up feed: after a landing om approved in the band, the
// worker files one shaped bead for the findings om raised and did not block on,
// de-duplicated against the beads earlier landings filed (gt-s01pn). A finding
// that really must be fixed blocks the landing through the score floor instead;
// this feed is for the ones worth fixing and not worth holding the queue for.

const (
	// FollowUpLabel is the label every filed bead wears and the one a later run
	// lists to find the beads it must de-duplicate against.
	FollowUpLabel = "om-followup"

	// followUpMaxScore tops the band. om scored the landing at or above it, so
	// what the review raised is not worth a bead either. It is named here
	// rather than configured because the top of the band is a property of om's
	// scale, not of a rig: om calls 0.9 the point where its findings stop
	// being worth filing.
	followUpMaxScore = 0.9

	// followUpBackpressure is the open follow-up count at which the feed stops
	// filing. It is backpressure rather than a cap, so the count falling back
	// under it resumes the feed on the next landing.
	followUpBackpressure = 30

	// followUpPriority is the priority a filed bead takes: it is work nobody is
	// waiting on, so it sorts below the queue's own beads.
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

// fileFollowups files the follow-up bead this landing earns, or comments on the
// bead an earlier landing already filed for one of its findings (gt-s01pn).
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
	// The band's floor is the rig's own threshold, read the way the review read
	// it: the landing om approved at the bar is exactly the one worth a
	// follow-up, and one it scored at followUpMaxScore or above has nothing
	// left to feed.
	threshold, err := OMThreshold(dir)
	if err != nil {
		l.logf("%s: could not read the .om.json threshold the follow-up feed bands on (%v); reading it as 0", w.BeadID, err)
	}
	if res.Verdict.Score < threshold || res.Verdict.Score >= followUpMaxScore {
		return
	}
	findings := followupCandidates(nonBlockingFindings(res.Verdict.Findings))
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
// comments a repeat earns, and the one bead the new findings earn. It returns
// the lines it wants logged and never returns an error — every failure is a
// line the caller logs and the landing does not see.
func (l *Lander) followupFeed(w Work, res Result, findings []FollowupCandidate) []string {
	existing, err := l.Followups.ListFollowups(FollowUpLabel)
	if err != nil {
		// Filing with no view of what exists would file a paraphrase of an
		// existing bead for every finding of every landing, so a failed read
		// files nothing.
		return []string{fmt.Sprintf("could not list the %s beads (%v); filing nothing for this landing", FollowUpLabel, err)}
	}
	if id := followupForCommit(existing, res.LandedCommit); id != "" {
		return []string{fmt.Sprintf("%s already carries this landing's follow-up; filing nothing", id)}
	}
	if n := openFollowups(existing); n >= followUpBackpressure {
		return []string{fmt.Sprintf("%d %s beads are open, at the %d-bead backpressure bound; filing nothing for this landing",
			n, FollowUpLabel, followUpBackpressure)}
	}
	var lines []string
	var fresh []FollowupCandidate
	for _, c := range findings {
		switch d := Decide(c, existing, 0); d.Action {
		case DedupeComment:
			// The finding is already filed: the repeat goes on that bead's
			// history, and nothing new is filed for it.
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
	id, err := l.Followups.CreateFollowup(followupSpec(w, res, fresh))
	if err != nil {
		return append(lines, fmt.Sprintf("could not file the follow-up bead for %s (%v)", findingsPhrase(len(fresh)), err))
	}
	return append(lines, fmt.Sprintf("filed %s for %s of the landing at %.2f", id, findingsPhrase(len(fresh)), res.Verdict.Score))
}

// nonBlockingFindings keeps the findings a follow-up bead is for: major and
// minor. Info is what om already decided is not worth fixing, and a finding om
// blocked on comes with a request_changes whose landing never got here.
func nonBlockingFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "major", "minor":
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

// followupForCommit is the open follow-up that already names commit, or "".
// A retry of a landing attempt that filed before failing finds its own bead
// this way and files no second one.
func followupForCommit(existing []ExistingFollowup, commit string) string {
	if commit == "" {
		return ""
	}
	for _, e := range existing {
		if !followupClosed(e) && strings.Contains(e.Description, commit) {
			return e.ID
		}
	}
	return ""
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

// followupSpec is the one bead a landing files: the title the retired
// refinery's beads carried, the shaped body, and the labels the next landing
// de-duplicates on — one fingerprint per finding it files for.
func followupSpec(w Work, res Result, findings []FollowupCandidate) FollowupSpec {
	labels := []string{FollowUpLabel}
	for _, c := range findings {
		labels = append(labels, Fingerprint(c.Path, c.Title))
	}
	return FollowupSpec{
		Title:    followupTitle(w, res, findings),
		Body:     followupBody(w, res, findings),
		Labels:   labels,
		Priority: followUpPriority,
	}
}

// followupTitle names the first finding, the count when one bead groups
// several, and what raised them: "Found: <finding> and 2 more (om major on
// gt-abc approved at 0.62)".
func followupTitle(w Work, res Result, findings []FollowupCandidate) string {
	first := findings[0]
	more := ""
	if n := len(findings) - 1; n > 0 {
		more = fmt.Sprintf(" and %d more", n)
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
func followupBody(w Work, res Result, findings []FollowupCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Goal\nFixed: the %s om raised on %s and did not block on, from the landing approved at %.2f (landed commit %s).\n",
		findingsPhrase(len(findings)), w.BeadID, res.Verdict.Score, res.LandedCommit)
	b.WriteString("\n")
	for _, c := range findings {
		fmt.Fprintf(&b, "- %s\n", c.Detail)
	}
	b.WriteString("\n## Constraints\nOnly the files the findings name. Keep the rig's gate green. Do not widen the change.\n")
	b.WriteString("\n## Out of scope\nAnything the findings do not name.\n")
	b.WriteString("\n## Gate\ngt done presubmit; the landing runs make gate\n")
	b.WriteString("\n## Size\none worker, one landing\n")
	b.WriteString("\n## Acceptance\n")
	for _, item := range acceptanceItems(findings) {
		fmt.Fprintf(&b, "- [ ] %s\n", item)
	}
	return b.String()
}

// acceptanceItems is the filed bead's acceptance list: one item per finding,
// with the ones past specdispatch.MaxAcceptance folded into the last item,
// which is the most one worker's landing can carry.
func acceptanceItems(findings []FollowupCandidate) []string {
	limit := specdispatch.MaxAcceptance
	if len(findings) <= limit {
		return findingTitles(findings)
	}
	out := findingTitles(findings[:limit-1])
	return append(out, fmt.Sprintf("the remaining %d findings under Goal are fixed", len(findings)-limit+1))
}

// findingTitles is one acceptance item per finding: the title, and the location
// when om named one.
func findingTitles(findings []FollowupCandidate) []string {
	out := make([]string, 0, len(findings))
	for _, c := range findings {
		item := NoteField(c.Title)
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
