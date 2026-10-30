package tiersweep

import "fmt"

// FormatDuration renders a whole second count in the form the daemon prints an
// elapsed time. The daemon logs elapsed.Round(time.Second) through %s
// (tierSweepSweptLine and the stage-finished line in
// internal/daemon/tier_sweep.go), which is time.Duration.String() for a whole
// number of seconds: 45s, 2m14s, 1h0m0s. A sweep's line and the daemon's line
// for the same tier therefore read alike in `gt tail`.
func FormatDuration(seconds int) string {
	switch {
	case seconds >= 3600:
		return fmt.Sprintf("%dh%dm%ds", seconds/3600, (seconds%3600)/60, seconds%60)
	case seconds >= 60:
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

// Summary renders one tier's summary line and whether the tier is RED. The
// line's shape is the daemon's to parse (tierSweepSummaryRE in
// internal/daemon/tier_sweep.go), so logDir is a parameter: the line names it.
//
// logDir amends the signature gt-tpg0p.97 states, which has no field the
// "(logs <dir>)" text could come from; the caller's Options.LogDir is the
// directory a sweep writes its package logs into (gt-jtyrs).
//
// A tier with a failure is RED and names its failures when it has names. A
// tier that counted nothing is RED too, since an empty tier's list broke and
// GREEN would report nothing as passing (gt-tqxdd). Skips alone are GREEN:
// TIER_SWEEP_SKIP is deliberate.
func Summary(tier, logDir string, passed, failed, skipped int, failedNames string, seconds int) (line string, red bool) {
	verdict, note := "GREEN", ""
	switch {
	case failed > 0:
		verdict, red = "RED", true
		if failedNames != "" {
			note = " failed: " + failedNames
		}
	case passed+failed+skipped == 0:
		verdict, red = "RED", true
	}
	return fmt.Sprintf("%s: %s %s passed=%d failed=%d skipped=%d%s (logs %s) in %s",
		Name, tier, verdict, passed, failed, skipped, note, logDir, FormatDuration(seconds)), red
}
