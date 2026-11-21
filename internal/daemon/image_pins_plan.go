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
	// imagePinsStaleBehind is how many commits behind its tip a pin must be
	// before the planner raises image-pins-stale for it. The operator approved
	// stale at 30 on 2026-11-14 (gt-wmoa0d); the drift guard's
	// daemonimage.MaxPinBehind is the bound that stops a roll.
	imagePinsStaleBehind = 30
	// imagePinsFileBehind is how many commits behind its tip a pin must be
	// before the planner files a refresh bead. It sits below the threshold
	// `gt daemon image pins` keeps, daemonimage.PinRefreshBehind, and below
	// imagePinsStaleBehind; both sit under the drift guard's
	// daemonimage.MaxPinBehind. The patrol ticks every 30 minutes by default,
	// so tick, dispatch and landing must fit in the margin under that bound:
	// 15 leaves 25 commits, about 75 minutes at a commit every three minutes
	// (gt-hc7fk3, gt-3gcvcc).
	imagePinsFileBehind = 15
	// imagePinsFileAge is how much older than its source's tip a pin's commit
	// may be before the planner files a refresh bead: two days under the drift
	// guard's daemonimage.MaxPinAge bound, so a low-traffic source the commit
	// count never catches still lands a refresh before the guard refuses the
	// build (gt-263ppi). Both read the age from committer times, as Age does.
	// The margin is two days, not one: the refresh must land, not just be
	// filed, inside it.
	imagePinsFileAge = daemonimage.MaxPinAge - 48*time.Hour
	// imagePinsStaleKey and imagePinsUnreadableKey are the per-pin items this
	// planner raises. Each names its pin in ImagePinItem.Pin.
	imagePinsStaleKey      = "image-pins-stale"
	imagePinsUnreadableKey = "image-pins-unreadable"
)

// ImagePinState is one image pin as the planner sees it: the pin file's name
// and pinned commit, the source tip the read returned, how many commits the pin
// trails it, and the error when that pin's read failed.
type ImagePinState struct {
	Name   string
	Pin    string
	Tip    string
	Behind int
	// Age is the tip's committer time minus the pin's, zero where it was not
	// read. A pin at its tip has not aged against it, so Age files nothing
	// there however the field reads.
	Age time.Duration
	// Err is the read failure for this pin. Its distance from the tip is
	// unknown, so the planner never files on it, whatever Behind holds.
	Err error
	// AgeErr is the age read's failure for this pin, recorded by
	// fillImagePinAges. The pin's commit count and tip were read, so the
	// planner's commit-count rules still apply to it, and it raises an
	// image-pins-unreadable item naming the age it could not read (gt-0hb3sx).
	AgeErr error
	// LacksFloor marks a bd pin whose own commit does not hold the samples
	// build, read when that commit is not its source's tip (gt-o2hvs3). It is
	// set before the lag read, so a state with an Err may carry it; the planner
	// files only on a pin whose Err is nil.
	LacksFloor bool
}

// ImagePinRefreshBead is an open refresh bead the planner found: the id the
// item names, and when it was opened.
type ImagePinRefreshBead struct {
	ID       string
	OpenedAt time.Time
}

// ImagePinItem is one attention item the planner raises. The patrol publishes
// it as an attention item with the kind and rule that belong to its key. Pin
// names the pin a per-pin item is about; it is empty for the refresh-stuck item,
// which is about the open bead.
type ImagePinItem struct {
	Key      string
	Pin      string
	Severity attention.Severity
	Summary  string
}

// ImagePinPlan is the planner's decision for one tick.
type ImagePinPlan struct {
	// File names the pins a refresh bead is filed for, in the order the
	// states arrived. Empty means file none: no pin trails by the file
	// threshold, or a refresh bead is already open.
	File []ImagePinState
	// Unreadable names the pins whose read failed this tick, in the order the
	// states arrived, so a failed read is told apart from a fresh pin. Each has
	// an image-pins-unreadable item in Items.
	Unreadable []string
	// Items are the conditions this tick raises.
	Items []ImagePinItem
}

// PlanImagePins decides whether the daemon files a refresh bead and which pins
// it names, and raises the items the states show now. A pin files at
// imagePinsFileBehind commits behind its source tip, below the pins command's
// threshold so the refresh lands before the guard; a pin that is not at its tip
// also files once its commit is imagePinsFileAge older than the tip, whatever
// its count, and a pin whose own commit lacks the samples build files at once.
// A pin whose read failed never files and is named in Unreadable,
// and raises image-pins-unreadable: its distance is unknown, so it is never
// treated as fresh. A pin whose age read failed keeps its count rules and
// raises image-pins-unreadable naming its unreadable age. A pin
// imagePinsStaleBehind commits behind or more raises image-pins-stale. These
// pin items are raised whether or not a refresh bead is open. An open refresh
// bead blocks a second file and raises image-pins-refresh-stuck once it is
// older than imagePinsRefreshStuck. A bead
// with no OpenedAt raises that item too: an age that cannot be shown to be
// under the bound is not fresh.
func PlanImagePins(pins []ImagePinState, open *ImagePinRefreshBead, now time.Time) ImagePinPlan {
	var plan ImagePinPlan
	for _, pin := range pins {
		if pin.Err != nil {
			// Behind is not trusted with a failed read: the pin's distance is
			// unknown, so the Behind check below must not see this pin at all.
			plan.Unreadable = append(plan.Unreadable, pin.Name)
			plan.Items = append(plan.Items, ImagePinItem{
				Key:      imagePinsUnreadableKey,
				Pin:      pin.Name,
				Severity: attention.SeverityMedium,
				Summary:  fmt.Sprintf("image pin %s cannot be read: it is not counted as fresh, and no refresh bead is filed for it", pin.Name),
			})
			continue
		}
		if pin.Behind >= imagePinsStaleBehind {
			plan.Items = append(plan.Items, ImagePinItem{
				Key:      imagePinsStaleKey,
				Pin:      pin.Name,
				Severity: attention.SeverityMedium,
				Summary:  fmt.Sprintf("image pin %s is %d commits behind its source's origin/main, at or over the %d-commit stale bound", pin.Name, pin.Behind, imagePinsStaleBehind),
			})
		}
		if pin.AgeErr != nil {
			// The pin's count was read, so the rules above and below still
			// judge it; only its age is unknown, and the item says so.
			plan.Items = append(plan.Items, ImagePinItem{
				Key:      imagePinsUnreadableKey,
				Pin:      pin.Name,
				Severity: attention.SeverityMedium,
				Summary:  fmt.Sprintf("image pin %s: its age could not be read, so only its commit count rules apply", pin.Name),
			})
		}
		// A source that moves rarely ages past the guard's bound with few
		// commits behind it, so an old commit files too; a pin at its tip has
		// not aged against it. A bd pin whose own commit lacks the samples build
		// files whatever its count and age (gt-o2hvs3). Err was handled above, so
		// a failed read never reaches this rule.
		if pin.Behind >= imagePinsFileBehind || (pin.Tip != pin.Pin && pin.Age >= imagePinsFileAge) || pin.LacksFloor {
			plan.File = append(plan.File, pin)
		}
	}
	if open == nil {
		return plan
	}
	// One refresh bead at a time: the pins it names are the ones a second bead
	// would race it for, so the file decision waits for it to land.
	plan.File = nil
	if item, raised := refreshStuckItem(open, now); raised {
		plan.Items = append(plan.Items, item)
	}
	return plan
}

// refreshStuckItem is the image-pins-refresh-stuck item for an open refresh
// bead, and whether this tick raises it: an open bead that is past
// imagePinsRefreshStuck, or whose age is not recorded.
func refreshStuckItem(open *ImagePinRefreshBead, now time.Time) (ImagePinItem, bool) {
	if open.OpenedAt.IsZero() {
		// An age nobody recorded cannot be shown to be under the bound, and a
		// duration measured from the zero time would print as a saturated
		// number, so the summary says the age is unknown.
		return ImagePinItem{
			Key:      imagePinsRefreshStuckKey,
			Severity: attention.SeverityMedium,
			Summary:  fmt.Sprintf("refresh bead %s is open and its age is unknown: the image pins wait for it to land", open.ID),
		}, true
	}
	age := now.Sub(open.OpenedAt)
	if age <= imagePinsRefreshStuck {
		return ImagePinItem{}, false
	}
	// Medium: only the drift guard's 40-commit bound stops a roll, so this
	// asks the overseer to look before a pin gets there.
	return ImagePinItem{
		Key:      imagePinsRefreshStuckKey,
		Severity: attention.SeverityMedium,
		Summary:  fmt.Sprintf("refresh bead %s open %s: the image pins wait for it to land", open.ID, townhealth.Short(age)),
	}, true
}
