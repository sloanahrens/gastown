package daemon

// The pin refresh planner (gt-wmoa0d) decides, from the four pins' reads and
// the one refresh bead that may already be open, whether the daemon files a
// shaped bead for a polecat to land. Filing nothing is the normal state: the
// patrol ticks far more often than a pin trails by the file threshold.

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/townhealth"
)

const (
	// imagePinsRefreshStuck is how long an open refresh bead may sit before
	// the planner raises image-pins-refresh-stuck. Three hours is long enough
	// that one gate and one landing of the bead are not the item (gt-wmoa0d).
	imagePinsRefreshStuck = 3 * time.Hour
	// imagePinsRefreshStuckKey is the one item this half of the planner
	// raises. There is one open refresh bead at a time, so one key holds it.
	imagePinsRefreshStuckKey = "image-pins-refresh-stuck"
)

// ImagePinState is one image pin as the planner sees it: the pin file's name
// and pinned commit, the source tip the read returned, how many commits the pin
// trails it, and the error when that pin's read failed.
type ImagePinState struct {
	Name   string
	Pin    string
	Tip    string
	Behind int
	// Err is the read failure for this pin. Its distance from the tip is
	// unknown, so the planner never files on it, whatever Behind holds.
	Err error
}

// ImagePinRefreshBead is an open refresh bead the planner found: the id the
// item names, and when it was opened.
type ImagePinRefreshBead struct {
	ID       string
	OpenedAt time.Time
}

// ImagePinItem is one attention item the planner raises. The patrol publishes
// it as an attention item with the kind and rule that belong to its key.
type ImagePinItem struct {
	Key      string
	Severity attention.Severity
	Summary  string
}

// ImagePinPlan is the planner's decision for one tick.
type ImagePinPlan struct {
	// File names the pins a refresh bead is filed for, in the order the
	// states arrived. Empty means file none: no pin trails by the file
	// threshold, or a refresh bead is already open.
	File []ImagePinState
	// Items are the conditions this tick raises.
	Items []ImagePinItem
}

// PlanImagePins decides whether the daemon files a refresh bead and which pins
// it names, and raises the items the states show now. A pin files at
// daemonimage.PinRefreshBehind commits behind its source tip, the same trigger
// `gt daemon image pins` uses; a pin whose read failed never files and raises
// no item; an open refresh bead blocks a second file and raises
// image-pins-refresh-stuck once it is older than imagePinsRefreshStuck. A bead
// with no OpenedAt raises that item too: an age that cannot be shown to be
// under the bound is not fresh.
func PlanImagePins(pins []ImagePinState, open *ImagePinRefreshBead, now time.Time) ImagePinPlan {
	var plan ImagePinPlan
	for _, pin := range pins {
		if pin.Err != nil {
			// Behind is not trusted with a failed read: the pin's distance is
			// unknown, and the image-pins-unreadable item for it comes with the
			// stale item (gt-3ycfvf).
			continue
		}
		if pin.Behind < daemonimage.PinRefreshBehind {
			continue
		}
		plan.File = append(plan.File, pin)
	}
	if open == nil {
		return plan
	}
	// One refresh bead at a time: the pins it names are the ones a second bead
	// would race it for, so the file decision waits for it to land.
	plan.File = nil
	if age := now.Sub(open.OpenedAt); age > imagePinsRefreshStuck {
		// Medium: only the drift guard's 40-commit bound stops a roll, so this
		// asks the overseer to look before a pin gets there.
		plan.Items = append(plan.Items, ImagePinItem{
			Key:      imagePinsRefreshStuckKey,
			Severity: attention.SeverityMedium,
			Summary:  fmt.Sprintf("refresh bead %s open %s: the image pins wait for it to land", open.ID, townhealth.Short(age)),
		})
	}
	return plan
}
