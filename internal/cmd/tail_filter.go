package cmd

import (
	"regexp"
	"strings"
)

// Hiding is by pattern on what the town does every few seconds, so anything
// the patterns do not name (landings, rejections, escalations, upgrade
// restarts, seat-refill dispatches, bead create/close/status changes) shows.

// tailRoutineDaemon matches the daemon.log lines that say "ran, nothing
// happened": the heartbeat, plugin handler skips, and the dogs' not-due and
// scan summaries. A Handler line is hidden only for the outcomes that did no
// work, so an "ok", a FAILED, or a failure to record the run still shows.
var tailRoutineDaemon = regexp.MustCompile(`^(` +
	`Heartbeat (starting|complete)` +
	`|Handler: (skipping plugin |running script plugin |script plugin \S+ skipped )` +
	`|jsonl_git_backup: ` +
	`|checkpoint_dog: ` +
	`|dog_cycle: checkpoint_dog ` +
	`|patrol_scan: ` +
	`)`)

// tailFailure rescues a routine-looking daemon line that reports a failure.
// "error(s)" is not one: patrol_scan prints it in every summary.
var tailFailure = regexp.MustCompile(`(?i)\b(failed|panic)\b|error:|timed out`)

// tailVisible reports whether the default view shows l. --all and --verbose
// skip it.
func tailVisible(l tailLine) bool {
	switch l.Kind {
	case tailKindEvents:
		// "<op> <issue> ...": a wisp's create/update/close is the patrol
		// loops' own churn, a few a minute.
		f := strings.Fields(l.Text)
		return len(f) < 2 || !isWispID(f[1])
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
