package daemon

import "time"

// newReconcilerTicker returns the reconciler patrol's tick channel and the func
// that stops it, ticking at the interval the patrol config in force names. The
// ticker comes from reconcilerTickerFn, so a test records the cadence without a
// real timer.
func (d *Daemon) newReconcilerTicker() (<-chan time.Time, func()) {
	interval := reconcilerInterval(d.patrol())
	newTicker := d.reconcilerTickerFn
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	return newTicker(interval)
}
