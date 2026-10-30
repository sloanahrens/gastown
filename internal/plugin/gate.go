package plugin

import "time"

// RunLedgerWindow is the span of run history evaluating the gate reads from
// the plugin run ledger; zero means it reads none, and an unparsable cooldown
// duration is returned as an error.
//
// Only a cooldown gate consults the ledger, back through its duration. A
// duration that reaches no run (zero or negative) ends now, so it holds
// nothing closed and reads nothing. A tick takes the longest of these windows
// for its single ledger read and evaluates each plugin against the same
// method, so the list and the evaluation cannot disagree on which gates the
// ledger serves.
func (g *Gate) RunLedgerWindow() (time.Duration, error) {
	if g == nil || g.Type != GateCooldown || g.Duration == "" {
		return 0, nil
	}
	window, err := time.ParseDuration(g.Duration)
	if err != nil {
		return 0, err
	}
	if window <= 0 {
		return 0, nil
	}
	return window, nil
}
