package daemon

import (
	"time"
)

// logTimeZone names the zone the daemon's log stamps and its maintenance
// windows are read in (gt-gg9hv.34). The compose file sets TZ on the
// container; the start-up line says which zone was actually loaded, and a name
// that does not load is a CRITICAL line rather than a silent fall back to UTC.
func (d *Daemon) logTimeZone() {
	tz := d.env("TZ")
	if tz == "" {
		// No TZ: Go has read the host's zone into time.Local, and loading
		// it by name here would read the zone database a second time.
		d.logZone(time.Local)
		return
	}
	loc, err := d.seams.zoneLoader()(tz)
	if err != nil {
		// The town keeps running: a daemon that stopped over a missing zone
		// database would take the whole town down with it.
		d.logger.Printf("CRITICAL: TZ=%s cannot be loaded (no zoneinfo in this image): log stamps and maintenance windows are UTC", tz)
		return
	}
	d.logZone(loc)
}

// logZone writes the one start-up line naming loc and its current UTC offset,
// so a reader of the log can tell from the log alone which zone its stamps are
// in.
func (d *Daemon) logZone(loc *time.Location) {
	d.logger.Printf("log timezone: %s (UTC%s)", loc.String(), d.clk().Now().In(loc).Format("-07:00"))
}
