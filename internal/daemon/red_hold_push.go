package daemon

import (
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/notify"
)

// pushRedHold is the heartbeat step (gt-e678p8): one red-hold push pass.
func (d *Daemon) pushRedHold() {
	d.runRedHoldPush(notify.PushRedHold)
}

// runRedHoldPush runs one pass of push and logs its sends and error. A send is
// a marker stamp that moved; an unreadable marker leaves the sends unknown.
func (d *Daemon) runRedHoldPush(push func(townRoot string, now time.Time) error) {
	before, beforeErr := notify.ReadRedHoldMarker(d.config.TownRoot)
	if err := push(d.config.TownRoot, d.clk().Now()); err != nil {
		d.logger.Printf("red hold push: %v", err)
	}
	after, afterErr := notify.ReadRedHoldMarker(d.config.TownRoot)
	if err := errors.Join(beforeErr, afterErr); err != nil {
		d.logger.Printf("red hold push: marker unreadable, sends unknown: %v", err)
		return
	}
	if !after.TextedAt.Equal(before.TextedAt) {
		d.logger.Printf("red hold push: texted the red hold")
	}
	if !after.MailedAt.Equal(before.MailedAt) {
		d.logger.Printf("red hold push: emailed the red hold")
	}
}
