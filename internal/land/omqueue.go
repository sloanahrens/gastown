package land

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The om finding queue: after a landing om approved, the major and critical
// findings it raised are written onto the WORK bead as the `queued:` ledger
// lines the next deep-review run reads, so what om said about a landing feeds
// work instead of stopping at the git note (gt-qtj72).
//
// It is not the follow-up feed beside it (followup.go). That one files a bead
// per finding, and it stays off. Here the landing only records what om raised;
// the deep-review verifier decides which findings are worth a bead, by trying
// to refute each one first.
//
// Its input is now a review that cannot pass: a landing carrying a major or
// critical finding is refused back to the author whatever it scored
// (gt-oi4je), so an approved landing reaches this queue with nothing to
// record.

const (
	// OMFindingsLabel is the label a work bead whose findings were queued
	// wears, so a later run can find them without reading every note.
	OMFindingsLabel = "om-findings"

	// omQueueTimeout bounds the whole queue — the read, the notes, the label
	// and the comment. The landing is already recorded when it runs, so a Dolt
	// that is slow or wedged costs a log line and nothing else.
	omQueueTimeout = 20 * time.Second

	// The two ledger markers a queued finding is recognized by. queued: is the
	// line this queue writes; finding: is the line a later deep-review run
	// writes once the finding has a bead of its own. The script reads both the
	// same way (scripts/deep-review.sh seen), and reading both here is what
	// keeps a retried landing from queuing a finding the review already filed.
	omQueuedMarker  = "queued: "
	omFindingMarker = "finding: "
)

// queueOMFindings writes the major and critical findings of an approved landing
// onto the work bead as queued: lines, labels the bead om-findings and leaves
// one comment naming the count.
//
// It runs once the landing is recorded and every failure is logged and dropped:
// the record is written, and nothing here may change the result or hold the
// queue past omQueueTimeout.
func (l *Lander) queueOMFindings(w Work, res Result) {
	if !l.OMQueue {
		return // the queue is off for this rig
	}
	// Only om's own approval. skipped, error: and overseer: are not verdicts om
	// reached, and a landing it rejected was rejected rather than recorded. The
	// score does not gate this: a finding is worth reading and refuting at any
	// score om approved at.
	if res.Verdict.Verdict != VerdictApprove {
		return
	}
	findings := omQueueFindings(res.Verdict.Findings)
	if len(findings) == 0 {
		return // info and minor are not what the review spends a verifier on
	}
	// The queue runs on its own goroutine so its timeout can abandon it, and it
	// hands its log lines back rather than writing them, so a queue that
	// outlives the timeout cannot interleave with the next landing's log
	// (the shape fileFollowups uses).
	done := make(chan []string, 1)
	go func() { done <- l.omQueueFeed(w, res, findings) }()
	select {
	case lines := <-done:
		for _, line := range lines {
			l.logf("%s: %s", w.BeadID, line)
		}
	case <-time.After(omQueueTimeout):
		l.logf("%s: the om finding queue did not finish in %s; the landing is unaffected", w.BeadID, omQueueTimeout)
	}
}

// omQueueFeed is the queue's I/O for one landing: the read, the notes, the
// label and the comment. It returns the lines it wants logged and never returns
// an error — every failure is a line the caller logs and the landing does not
// see.
func (l *Lander) omQueueFeed(w Work, res Result, findings []Finding) []string {
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		// A finding's line is only safe to append to a bead whose notes were
		// read: queuing blind would append the same lines again on every retry.
		return []string{fmt.Sprintf("could not read the work bead for the findings already queued on it (%v); queuing nothing for this landing", err)}
	}
	var fresh []string
	for _, f := range findings {
		fp := Fingerprint(f.Path, f.Title)
		if notesCarryFingerprint(issue.Notes, fp) {
			continue // an earlier pass over this landing already queued it
		}
		line, ok := queuedLine(f, fp)
		if !ok {
			continue
		}
		fresh = append(fresh, line)
	}
	if len(fresh) == 0 {
		// Nothing fresh means a retry of a landing already recorded, so the
		// label and the comment are already there too: a second pass writes
		// nothing at all.
		return nil
	}
	if err := l.Beads.AppendNotes(w.BeadID, strings.Join(fresh, "\n")); err != nil {
		return []string{fmt.Sprintf("could not queue %s (%v)", findingsPhrase(len(fresh)), err)}
	}
	lines := []string{fmt.Sprintf("queued %s for the next deep-review run", findingsPhrase(len(fresh)))}
	if !beads.HasLabel(issue, OMFindingsLabel) {
		if err := l.Beads.Update(w.BeadID, beads.UpdateOptions{AddLabels: []string{OMFindingsLabel}}); err != nil {
			lines = append(lines, fmt.Sprintf("could not label the work bead %s (%v)", OMFindingsLabel, err))
		}
	}
	if err := l.Beads.AddComment(w.BeadID, omQueueComment(len(findings), res.Verdict.Score)); err != nil {
		lines = append(lines, fmt.Sprintf("could not comment the queued findings (%v)", err))
	}
	return lines
}

// omQueueFindings keeps the findings the queue records: critical and major,
// whatever their severity's case or padding. Info is what om already decided is
// not worth fixing, and minor is what it decided is not worth a review pass.
func omQueueFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if _, ok := omPriority(f.Severity); ok {
			out = append(out, f)
		}
	}
	return out
}

// The ledger priorities omPriority maps om's blocking severities onto.
const (
	omPriorityCritical = "P1"
	omPriorityMajor    = "P2"
)

// omPriority maps an om severity onto the ledger's priority: critical is P1 and
// major is P2. A severity the review has no priority for is not one the queue
// records.
func omPriority(severity string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return omPriorityCritical, true
	case "major":
		return omPriorityMajor, true
	}
	return "", false
}

// queuedLine renders one finding as the ledger line the deep-review run reads:
//
//	queued: <fingerprint> <P1|P2> <file:line> <claim>
//
// It reports false for a finding it cannot render. A finding om gave no title
// has no claim to record, and the reader that parses this ledger reads a line
// with fewer than four fields as a corrupt ledger rather than as one line it
// cannot use (scripts/deep-review.sh queued), so an empty claim would cost the
// whole run its ledger instead of that one finding.
func queuedLine(f Finding, fingerprint string) (string, bool) {
	priority, ok := omPriority(f.Severity)
	if !ok {
		return "", false
	}
	claim := NoteField(f.Title)
	if claim == "" {
		return "", false
	}
	// The ledger names a location for every finding, and a line om left at zero
	// is the file's first line rather than no line: the field is one token the
	// reader counts.
	line := f.Line
	if line < 1 {
		line = 1
	}
	return fmt.Sprintf("%s%s %s %s:%d %s", omQueuedMarker, fingerprint, priority, NoteField(f.Path), line, claim), true
}

// notesCarryFingerprint reports whether a work bead's notes already name fp:
// either as the queued: line this queue wrote, or as the finding: line a later
// deep-review run wrote once the finding got a bead. The ledger needs no second
// queue once either one names it.
func notesCarryFingerprint(notes, fingerprint string) bool {
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{omQueuedMarker, omFindingMarker} {
			rest, ok := strings.CutPrefix(line, marker)
			if !ok {
				continue
			}
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] == fingerprint {
				return true
			}
		}
	}
	return false
}

// omQueueComment is the one comment the queue leaves on the work bead: what om
// raised on the landing, and that the next deep-review run reads it.
func omQueueComment(n int, score float64) string {
	return fmt.Sprintf("om raised %s (major/critical) on this approved landing (%.2f); queued for the next deep-review run.",
		findingsPhrase(n), score)
}
