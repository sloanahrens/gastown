package shelltier

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// Exit is the process exit code of a shell-tier sweep.
type Exit int

const (
	// ExitGreen means no script failed and the tier counted something.
	ExitGreen Exit = 0
	// ExitRed means a script failed, or the tier counted nothing.
	ExitRed Exit = 1
)

// Summary renders r as the tier's one result line, the line the daemon and the
// post-land revert parse. The text is tiersweep.Summary's.
func Summary(r Result, logDir string) string {
	line, _ := r.render(logDir)
	return line
}

// FormatDuration renders d in the summary line's form, at whole seconds.
func FormatDuration(d time.Duration) string {
	return tiersweep.FormatDuration(wholeSeconds(d))
}

// Exit is ExitRed when the run failed a script or counted nothing, and
// ExitGreen otherwise. It is the verdict the summary line prints.
func (r Result) Exit() Exit {
	if _, red := r.render(""); red {
		return ExitRed
	}
	return ExitGreen
}

// render is the summary line and whether the tier is RED.
func (r Result) render(logDir string) (string, bool) {
	return tiersweep.Summary("shell", logDir, r.Passed, r.Failed, r.Skipped,
		strings.Join(r.FailedNames, " "), wholeSeconds(r.Elapsed))
}

// wholeSeconds is d rounded to the nearest second, the unit the line prints.
// The duration is rounded to the nearest whole second because the daemon logs
// elapsed.Round(time.Second); tiersweep.FormatDuration is the one formatter and
// this is its thin adapter.
func wholeSeconds(d time.Duration) int {
	return int(d.Round(time.Second) / time.Second)
}
