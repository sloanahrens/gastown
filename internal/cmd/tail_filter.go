package cmd

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// Hiding is by pattern on what the town does every few seconds, so anything
// the patterns do not name (landings, rejections, escalations, upgrade
// restarts, spec-dispatcher ticks, bead create/close/status changes) shows.

// tailRoutineDaemon matches the daemon.log lines that say "ran, nothing
// happened": the heartbeat, plugin handler skips, the patrols a config leaves
// off, the dogs' not-due and scan summaries, the doctor's all-clear, the
// convoy bookkeeping, and the alert clears. A Handler line is hidden only for
// the outcomes that did no work, so an "ok", a FAILED, or a failure to record
// the run still shows.
var tailRoutineDaemon = regexp.MustCompile(`^(` +
	`Heartbeat (starting|complete)` +
	`|Handler: (skipping plugin |running script plugin |script plugin \S+ skipped )` +
	`|(Mayor|Handler) patrol disabled in config, skipping` +
	`|jsonl_git_backup: ` +
	`|checkpoint_dog: ` +
	`|doctor_dog: all clear` +
	`|dog_cycle: checkpoint_dog ` +
	`|patrol_scan: ` +
	`|Convoy: checking convoy` +
	`|Convoy: \S+ tracked by [0-9]+ convoy` +
	`|clearAlerts\(` +
	`)`)

// tailFailure rescues a routine-looking daemon line that reports a failure.
// "error(s)" is not one: patrol_scan prints it in every summary.
var tailFailure = regexp.MustCompile(`(?i)\b(failed|panic)\b|error:|timed out`)

// tailVisible reports whether the default view shows l. --all and --verbose
// skip it. A townhealth line that repeats the last one shown is hidden by the
// view's latch, not here: this only says whether the line is the kind the
// default view keeps.
func tailVisible(l tailLine) bool {
	switch l.Kind {
	case tailKindEvents:
		// "<op> <issue> ...": a wisp's create/update/close is the patrol
		// loops' own churn, a few a minute.
		f := strings.Fields(l.Text)
		if len(f) >= 2 && isWispID(f[1]) {
			return false
		}
		return !tailAgentBeadOpen(f)
	case tailKindDaemon:
		if id, ok := strings.CutPrefix(l.Text, "Convoy: close detected: "); ok {
			f := strings.Fields(id)
			return len(f) == 0 || !isWispID(f[0])
		}
		return !tailRoutineDaemon.MatchString(l.Text) || tailFailure.MatchString(l.Text)
	}
	return true
}

// isWispID reports whether id is a wisp's: gt-wisp-c3w9, hq-wisp-e0g8y7.
func isWispID(id string) bool {
	return strings.Contains(id, "-wisp-")
}

// tailAgentBeadOpen reports whether an events line is a worker's agent bead
// being opened — the write the daemon and every worker make whenever a
// session starts, and the loudest bead churn in the stream. The journal does
// not always record the status a write set, so an update carrying no status
// counts as the same open write; one that says it moved anywhere else still
// shows, as do the bead's create and close (gt-uqabu).
//
// f is the line's fields: "<op> <issue> [status=<s>] [actor=<a>] seq=<n>".
func tailAgentBeadOpen(f []string) bool {
	if len(f) < 2 || f[0] != "update" || !isPolecatAgentBead(f[1]) {
		return false
	}
	for _, field := range f[2:] {
		if status, ok := strings.CutPrefix(field, "status="); ok {
			return status == "open"
		}
	}
	return true
}

// isPolecatAgentBead reports whether id names a polecat's agent bead:
// gt-<rig>-polecat-<name>, or ff-polecat-<name> where the rig is the prefix.
func isPolecatAgentBead(id string) bool {
	_, role, _, ok := beads.ParseAgentBeadID(id)
	return ok && role == constants.RolePolecat
}

// tailTownHealth matches the townhealth line the daemon rewrites every few
// minutes.
var tailTownHealth = regexp.MustCompile(`^townhealth: `)

// tailTownHealthAgeRe matches the "tick 4ms ago" age inside it: the field
// that says when the line was written, not what it reports.
var tailTownHealthAgeRe = regexp.MustCompile(`\btick \S+ ago\b`)

// tailTownHealthMeasureRe matches the "exec-tax=133ms/exec" reading beside
// the age. The reading jitters by a few milliseconds on every tick, so a key
// that strips only the age leaves every line distinct and hides none; a
// reading that goes missing is not this pattern and still prints, as
// "exec-tax[?]" (gt-uqabu).
var tailTownHealthMeasureRe = regexp.MustCompile(`\bexec-tax=\S+`)

// tailTownHealthLatch hides a townhealth line whose text, minus the age and
// the measurement it carries, repeats the last one shown. The town health
// line is rewritten on every tick whether or not anything moved, so those two
// fields alone change; a change in what it reports still prints.
//
// One latch belongs to one view, and a line it hides does not become the new
// last one, so a status that holds still keeps quiet until it moves.
type tailTownHealthLatch struct {
	mu   sync.Mutex
	last string
}

func (l *tailTownHealthLatch) visible(text string) bool {
	if !tailTownHealth.MatchString(text) {
		return true
	}
	key := tailTownHealthMeasureRe.ReplaceAllString(tailTownHealthAgeRe.ReplaceAllString(text, "tick -"), "exec-tax=X")
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == l.last {
		return false
	}
	l.last = key
	return true
}

// tailDuplicateWindow is how close two identical events lines have to be for
// the second to be the same event written twice.
const tailDuplicateWindow = 2 * time.Second

// tailJournalOps are the operations a bd events record carries
// (internal/beads/events.go). A line whose first field is not one of them is
// not a record — a source's own error line, say — and is never a duplicate.
var tailJournalOps = map[string]bool{
	"create": true, "update": true, "close": true, "delete": true,
	"dep_add": true, "dep_remove": true, "comment": true,
}

// tailEventKey reads the identity of an events line: the operation, the bead,
// and the status the write left it in. Two journal rows with the same key
// close together are one write the store recorded twice, or a retry that
// landed on the same row; seq, actor and time are what the retry changes, so
// they are not part of the key. "" for a line that is not an "<op> <id> ..."
// record — a source's own error line, say — which is never a duplicate.
func tailEventKey(text string) string {
	f := strings.Fields(text)
	if len(f) < 2 || !tailJournalOps[f[0]] || f[1] == "" {
		return ""
	}
	key := f[0] + "\x00" + f[1] + "\x00"
	for _, field := range f[2:] {
		if status, ok := strings.CutPrefix(field, "status="); ok {
			return key + status
		}
	}
	return key
}

// tailEventDuplicateLatch hides an events line that repeats the one before it
// for the same bead, operation and status within tailDuplicateWindow. A bd
// write arrives as several journal rows when it is retried (two commits, one
// logical event), and the same row is rewritten as a store races a retry, so
// the stream otherwise carries the same sentence two or three times.
//
// The window slides: a run of identical rows stays hidden while it keeps
// coming, and the next write after a pause prints again.
type tailEventDuplicateLatch struct {
	mu  sync.Mutex
	key string
	at  time.Time
}

func (l *tailEventDuplicateLatch) visible(ln tailLine) bool {
	if ln.Kind != tailKindEvents || ln.At.IsZero() {
		return true
	}
	key := tailEventKey(ln.Text)
	if key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	same := key == l.key && ln.At.Sub(l.at) < tailDuplicateWindow && !ln.At.Before(l.at)
	l.key, l.at = key, ln.At
	return !same
}

// tailSeatWaitIcon leads the one line a convoy's seat-retry loop prints.
const tailSeatWaitIcon = "⏳"

// The daemon lines a convoy's seat-retry loop writes: the attempt that fed the
// bead, the attempt that could not, and the reason that names a missing seat
// as opposed to any other complaint.
var (
	tailConvoyFeedRe   = regexp.MustCompile(`^Convoy \S+: feeding (\S+) to `)
	tailConvoyDeferRe  = regexp.MustCompile(`^Convoy \S+: deferring (\S+): (.*)$`)
	tailConvoyNoSeatRe = regexp.MustCompile(`(?i:\bpool: full\b|\bno seat\b)`)
)

// tailSeatWaitLatch collapses a convoy's seat-retry loop into one line. A
// convoy retries a bead every few seconds while the pool has no seat, so the
// stream carries "deferring gt-x: pool: full ..." once per attempt, all of it
// saying the same thing. The first prints as one waiting line; the rest are
// suppressed until the bead, the reason or the outcome changes. A later
// success — the convoy's "feeding gt-x" line — clears the latch, so the bead
// printing a fresh waiting line means it lost its seat again.
type tailSeatWaitLatch struct {
	mu     sync.Mutex
	bead   string
	reason string
}

// tailSeatWaitText is the one line a seat-waiting bead prints as.
func tailSeatWaitText(bead string) string {
	return tailSeatWaitIcon + " waiting for a seat: " + bead
}

// adjust returns the line to print and whether to print it.
func (l *tailSeatWaitLatch) adjust(ln tailLine) (tailLine, bool) {
	if ln.Kind != tailKindDaemon {
		return ln, true
	}
	if m := tailConvoyFeedRe.FindStringSubmatch(ln.Text); m != nil {
		l.mu.Lock()
		if l.bead == m[1] {
			l.bead, l.reason = "", ""
		}
		l.mu.Unlock()
		return ln, true
	}
	m := tailConvoyDeferRe.FindStringSubmatch(ln.Text)
	if m == nil || !tailConvoyNoSeatRe.MatchString(m[2]) {
		return ln, true
	}
	bead, reason := m[1], strings.TrimSpace(m[2])
	l.mu.Lock()
	repeat := l.bead == bead && l.reason == reason
	if !repeat {
		l.bead, l.reason = bead, reason
	}
	l.mu.Unlock()
	if repeat {
		return ln, false
	}
	ln.Text = tailSeatWaitText(bead)
	return ln, true
}

// tailDefaultFilter is the default view's filter. It hides the routine lines,
// collapses a convoy's seat-retry loop, drops an events line that repeats the
// one before it, and latches a townhealth line that repeats the last one
// shown. --all and --verbose keep every raw line.
type tailDefaultFilter struct {
	townHealth tailTownHealthLatch
	duplicates tailEventDuplicateLatch
	seats      tailSeatWaitLatch
}

func (f *tailDefaultFilter) visible(ln tailLine) (tailLine, bool) {
	if !tailVisible(ln) {
		return ln, false
	}
	adjusted, ok := f.seats.adjust(ln)
	if !ok {
		return ln, false
	}
	ln = adjusted
	if !f.duplicates.visible(ln) {
		return ln, false
	}
	return ln, f.townHealth.visible(ln.Text)
}

// The fields the default view strikes from an events line. The seq numbers the
// journal's cursor, and the actor is the writer: both are the form to grep by,
// which is what --verbose is for. An actor that is not the operator's own git
// identity is kept — that one names somebody else's write.
var tailSeqFieldRe = regexp.MustCompile(` seq=[0-9]+`)

// tailDropActorField drops " actor=<name>" when name is gitUser. The name is
// whatever the writer recorded, and bd records a person's name with its space
// ("Sloan Ahrens"), so the field runs to the next field or to the end — not to
// the next space, which would match only the given name.
func tailDropActorField(text, gitUser string) string {
	const field = " actor="
	i := strings.Index(text, field)
	if i < 0 {
		return text
	}
	rest := text[i+len(field):]
	end := len(rest)
	for _, next := range []string{" seq=", " ts="} {
		if j := strings.Index(rest, next); j >= 0 && j < end {
			end = j
		}
	}
	if rest[:end] != gitUser {
		return text
	}
	return text[:i] + rest[end:]
}

// tailTrimEventFields drops the cursor and the operator's own identity from an
// events line. A line that is not an events record, and a view that keeps the
// raw columns, are returned unchanged.
func tailTrimEventFields(ln tailLine, gitUser string) tailLine {
	if ln.Kind != tailKindEvents {
		return ln
	}
	ln.Text = tailSeqFieldRe.ReplaceAllString(ln.Text, "")
	if gitUser != "" {
		ln.Text = tailDropActorField(ln.Text, gitUser)
	}
	return ln
}

// The verdict prefixes the review loop writes. A comment or a note that opens
// with one of these is a verdict, not chatter, and the stream shows it.
var tailVerdictMarkers = []string{
	"OVERSEER REVIEW",
	"OVERSEER RULING",
	"STEWARD",
	"MERGE REJECTION",
}

// tailVerdictWidth bounds the verdict text the stream prints.
const tailVerdictWidth = 100

// tailVerdictText reads the verdict a comment or a note event carries, "" when
// the event says nothing the operator needs. For a comment it is the newest
// comment — the one the event is about; for a note it is the last MERGE
// REJECTION block in the notes. Either has to open with a marker.
func tailVerdictText(issue *beads.Issue, op string) string {
	if issue == nil {
		return ""
	}
	switch op {
	case "comment":
		if n := len(issue.Comments); n > 0 {
			return tailMarkerText(issue.Comments[n-1].Text)
		}
	case "update":
		return tailMarkerText(lastMergeRejectionLine(issue.Notes))
	}
	return ""
}

// lastMergeRejectionLine returns the opening line of the last MERGE REJECTION
// block in notes, "" when the notes hold none. The block's first line is the
// one that names the attempt, the kind and the reason.
func lastMergeRejectionLine(notes string) string {
	var last string
	for _, line := range strings.Split(notes, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "MERGE REJECTION") {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

// tailMarkerText returns text when it opens with a verdict marker, trimmed to
// tailVerdictWidth runes, "" otherwise.
func tailMarkerText(text string) string {
	text = strings.TrimSpace(text)
	found := false
	for _, marker := range tailVerdictMarkers {
		if strings.HasPrefix(text, marker) {
			found = true
			break
		}
	}
	if !found {
		return ""
	}
	return tailTruncateRunes(text, tailVerdictWidth)
}

// tailTruncateRunes cuts text to at most n runes, marking the cut.
func tailTruncateRunes(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n]) + "…"
}

// The verdict's own colors: a pass is green, a refusal red, and an advisory
// "(shadow)" verdict yellow whatever it decided.
var (
	tailVerdictPassRe = regexp.MustCompile(`\bPASS\b`)
	tailVerdictFailRe = regexp.MustCompile(`\bFAIL\b|(?i:\brefused\b|\brejection\b)`)
)

// tailVerdictClass is the class a verdict line is drawn in.
func tailVerdictClass(text string) tailClass {
	switch {
	case strings.Contains(text, "(shadow)"):
		return tailClassWarning
	case tailVerdictPassRe.MatchString(text):
		return tailClassSuccess
	case tailVerdictFailRe.MatchString(text):
		return tailClassFailure
	}
	return tailClassPlain
}

// tailClass is what a line's text says it is: the color and the emoji the
// default view gives it. Only the text is read, so a line a source labeled
// itself is classified the way the daemon's own lines are.
type tailClass int

const (
	tailClassPlain tailClass = iota // nothing to report: dim, no emoji
	tailClassFailure
	tailClassWarning
	tailClassSuccess
	tailClassLanding
	tailClassDispatch
	tailClassRestart
	// tailClassCount bounds the table the view draws a class through.
	tailClassCount
)

// The class patterns, each a phrase the town's own lines use. RED, GREEN,
// SLOW and FAILED are the markers the health, landing and gate lines
// capitalize, so those match case-sensitively: "red" and "green" the ordinary
// words would tag half the stream.
var (
	// tailClassFailureRe: something broke. "cannot" is how a source reports
	// the read it could not make, and "error:" is a colon-delimited error
	// report, so a summary counting "0 error(s)" stays plain.
	tailClassFailureRe = regexp.MustCompile(`\bRED\b|(?i:\b(failed|failure|panic)\b|\breject\w*\b|\bcannot\b|error:)`)
	// tailClassWarningRe: something ran long or wants a human.
	tailClassWarningRe = regexp.MustCompile(`\bSLOW\b|(?i:\btimed out\b|\bescalat)`)
	// tailClassSuccessRe: a landing, a post-land check, a green health line.
	tailClassSuccessRe = regexp.MustCompile(`(?i:\bpost-land green\b|\blanded\b)|\bGREEN\b`)
	// tailClassLandingRe: the refinery's own two lines for a merge it is
	// still gating (internal/land).
	tailClassLandingRe = regexp.MustCompile(`(?i:\bmerged\b.*\bgating\b)|\bstages: `)
	// tailClassDispatchRe: work handed to a worker.
	tailClassDispatchRe = regexp.MustCompile(`(?i:\b(slung|sling|spawn)\w*\b)`)
	// tailDispatchMentionRe: the text that quotes a dispatch instead of
	// reporting one. A convoy line about stranded work prints the command
	// that would resume it — "(resume with: gt sling gt-x gastown --branch
	// ...)" — one per stranded bead, so the word alone would tag the line
	// with an action that has not happened (gt-uqabu).
	tailDispatchMentionRe = regexp.MustCompile(`(?i:resume with:)`)
	// tailClassRestartRe: a process replaced by the same one.
	tailClassRestartRe = regexp.MustCompile(`(?i:\b(restart|upgrade)\w*\b)`)
	// tailRestartNotAnEventRe: the shapes that spend a restart word without
	// naming one — a Dolt retry, and the watchdog noting a heartbeat from
	// before a restart.
	tailRestartNotAnEventRe = regexp.MustCompile(`(?i:\btry restarting\b|pre-restart)`)
	// tailZeroCountRe: a running summary's count of none — the patrol scan's
	// "0 restarted, 0 refused, 0 error(s)", the landing pass's "2 landed, 0
	// repaired, 0 rejected, 0 failed". What such a line reports is the
	// nonzero part of it, so its zero pairs are struck from the copy the
	// classifier reads; the line itself prints whole.
	tailZeroCountRe = regexp.MustCompile(`\b0 (landed|repaired|rejected|failed|restarted|refused|skipped|unknown|error)\b`)
)

// tailClassOf classifies one line's text. The classes are tried most severe
// first, so a line that reports both a failure and a dispatch is a failure.
func tailClassOf(text string) tailClass {
	text = tailZeroCountRe.ReplaceAllString(text, "none")
	switch {
	case tailClassFailureRe.MatchString(text):
		return tailClassFailure
	case tailClassWarningRe.MatchString(text):
		return tailClassWarning
	case tailClassSuccessRe.MatchString(text):
		return tailClassSuccess
	case tailClassLandingRe.MatchString(text):
		return tailClassLanding
	case tailClassDispatchRe.MatchString(text) && !tailDispatchMentionRe.MatchString(text):
		return tailClassDispatch
	case tailClassRestartRe.MatchString(text) && !tailRestartNotAnEventRe.MatchString(text):
		return tailClassRestart
	}
	return tailClassPlain
}
