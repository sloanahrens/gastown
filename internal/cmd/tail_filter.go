package cmd

import (
	"regexp"
	"strings"
	"sync"

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
