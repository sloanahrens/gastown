package daemon

// The image pins patrol tick (gt-ouyejg): one tick reads the pins file and each
// pin's source tip, asks the planner, and files the one refresh bead the
// planner says is due. The tick writes neither pins file, so the refresh itself
// stays a polecat's landing. Everything the tick reads or files is a seam, so a
// unit test drives every branch with no file, network or git.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// imagePinsTickSeams are one tick's readings, decisions and filer, injected so
// a test supplies each. The pins reader, tip reader, open-bead lookup and filer
// are required: a tick given none of them cannot tell whether a pin trails, and
// refusing up front keeps it from reporting the quiet "" that means nothing was
// due.
type imagePinsTickSeams struct {
	// now is the clock the planner measures an open refresh bead's age against.
	now func() time.Time
	// readPins returns the image pins file's bytes.
	readPins func() ([]byte, error)
	// readTips reads each pin's source tip and how far the pin trails it.
	readTips func(ctx context.Context, data []byte) ([]ImagePinState, error)
	// openBead returns the one refresh bead already open, or nil.
	openBead func() (*ImagePinRefreshBead, error)
	// plan decides whether a refresh bead is due. Nil means PlanImagePins; a
	// test replaces it to see what the tick asked.
	plan func(pins []ImagePinState, open *ImagePinRefreshBead, now time.Time) ImagePinPlan
	// file files the refresh bead for the pins the planner named and returns
	// its id.
	file func(pins []ImagePinState) (string, error)
}

// check returns the error naming every required seam the tick was not given, so
// an unwired tick fails before its first read instead of at the read that
// needed the seam. The planner is not named: nil is the production planner.
func (s imagePinsTickSeams) check() error {
	var missing []string
	if s.now == nil {
		missing = append(missing, "clock")
	}
	if s.readPins == nil {
		missing = append(missing, "pins reader")
	}
	if s.readTips == nil {
		missing = append(missing, "tip reader")
	}
	if s.openBead == nil {
		missing = append(missing, "open refresh bead lookup")
	}
	if s.file == nil {
		missing = append(missing, "filer")
	}
	if len(missing) != 0 {
		return fmt.Errorf("image pins patrol: no %s: a tick needs each to tell whether a pin trails, and to file the bead that says so", strings.Join(missing, ", "))
	}
	return nil
}

// planner returns the planner the tick asks: the injected one, or PlanImagePins
// when the tick carries none.
func (s imagePinsTickSeams) planner() func(pins []ImagePinState, open *ImagePinRefreshBead, now time.Time) ImagePinPlan {
	if s.plan == nil {
		return PlanImagePins
	}
	return s.plan
}

// runImagePinsTick runs one tick. It returns the id of the bead it filed ("" when
// nothing was due), the planner's items unchanged, and the first error, with the
// items the planner raised before that error. The tick raises none itself: the
// registration bead raises and clears them (gt-9oap8n), so an item dropped here
// never reaches an operator.
func runImagePinsTick(ctx context.Context, s imagePinsTickSeams) (string, []ImagePinItem, error) {
	if err := s.check(); err != nil {
		return "", nil, err
	}
	data, err := s.readPins()
	if err != nil {
		return "", nil, fmt.Errorf("image pins patrol: reading the pins file: %w", err)
	}
	pins, err := s.readTips(ctx, data)
	if err != nil {
		return "", nil, fmt.Errorf("image pins patrol: reading each pin's source tip: %w", err)
	}
	if len(pins) == 0 {
		return "", nil, errors.New("image pins patrol: the pins read named no pin: a pins file names its four pins, so an empty read is not a town with nothing to do")
	}
	open, err := s.openBead()
	if err != nil {
		return "", nil, err
	}
	plan := s.planner()(pins, open, s.now())
	if len(plan.File) == 0 {
		return "", plan.Items, nil
	}
	id, err := s.file(plan.File)
	if err != nil {
		return "", plan.Items, fmt.Errorf("image pins patrol: filing the refresh bead: %w", err)
	}
	if id == "" {
		return "", plan.Items, errors.New("image pins patrol: the filer returned no bead id: the refresh bead it filed cannot be named")
	}
	return id, plan.Items, nil
}
