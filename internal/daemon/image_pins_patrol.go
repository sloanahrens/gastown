package daemon

// The image pins patrol tick (gt-ouyejg): one tick reads the pins file and each
// pin's source tip, asks the planner, and files the one refresh bead the
// planner says is due. The tick writes neither pins file, so the refresh itself
// stays a polecat's landing. Everything the tick reads or files is a seam, so a
// unit test drives every branch with no file, network or git.
//
// This file also registers the patrol (gt-01lcq2): the ticker the daemon starts
// it on, the interval its config entry names, and the town's own seams the
// trigger hands the tick. The patrol raises none of the planner's items itself;
// the next bead publishes them (gt-9oap8n).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/buildsrc"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// defaultImagePinsInterval is the period between two ticks: the pins move
	// at most a few times a day, and one tick is four tip reads and up to four
	// compares, so a half hour keeps a fresh pin from waiting long.
	defaultImagePinsInterval = 30 * time.Minute

	// minImagePinsInterval bounds a configured interval up: a shorter one only
	// rereads the same tips and loads git and the forge.
	minImagePinsInterval = time.Minute

	// imagePinsTickTimeout bounds a whole tick, the gt checkout's sync
	// included, so a stuck read frees the patrol's single-flight guard.
	imagePinsTickTimeout = 10 * time.Minute

	// imagePinsGitTimeout bounds one pin's ls-remote, and imagePinsHTTPTimeout
	// one compare request and imagePinsBdTimeout one bd call.
	imagePinsGitTimeout  = 2 * time.Minute
	imagePinsHTTPTimeout = 30 * time.Second
	imagePinsBdTimeout   = 30 * time.Second
)

// imagePinsInterval is the period between two ticks: the configured
// patrols.image_pins.interval, clamped up to minImagePinsInterval, or
// defaultImagePinsInterval when the key is absent or does not parse.
func imagePinsInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.ImagePins != nil {
		if d, err := time.ParseDuration(config.Patrols.ImagePins.Interval); err == nil && d > 0 {
			return max(d, minImagePinsInterval)
		}
	}
	return defaultImagePinsInterval
}

// startImagePinsTicker starts the image_pins patrol's own ticker when the patrol
// is active, and returns a nil channel and stop func when it is not: the pins
// move on a cadence of their own, and a town that names no key starts nothing
// (gt-01lcq2). The interval is the patrol's configured one, or its default.
func (d *Daemon) startImagePinsTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive("image_pins") {
		return nil, nil
	}
	interval := imagePinsInterval(d.patrol())
	newTicker := d.newImagePinsTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	ticks, stop := newTicker(interval)
	d.logger.Printf("Image pins ticker started (interval %v)", interval)
	return ticks, stop
}

// startImagePinsTick runs one tick on its own goroutine: a tick syncs the gt
// checkout and makes up to eight network reads, which must not hold the main
// select loop. A tick that arrives while the previous one still runs is skipped
// and logged, since two ticks at once would read the same tips twice and race
// the filer for the one refresh bead.
func (d *Daemon) startImagePinsTick() {
	if !d.imagePinsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("image_pins: previous tick still running, skipping")
		return
	}
	d.imagePinsCycles.Add(1)
	go func() {
		defer d.imagePinsCycles.Done()
		defer d.imagePinsRunning.Store(false)
		d.triggerImagePins()
	}()
}

// triggerImagePins runs one tick over the town's own seams. A failed tick is
// logged and never taken for a town with nothing due: the planner's items and
// the failure belong to the attention collector the next bead adds (gt-9oap8n),
// so here every error ends the tick in the log.
func (d *Daemon) triggerImagePins() {
	if d.config == nil || !d.isPatrolActive("image_pins") {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, imagePinsTickTimeout)
	defer cancel()
	build := d.imagePinsSeams
	if d.newImagePinsSeams != nil {
		build = d.newImagePinsSeams
	}
	id, items, err := runImagePinsTick(ctx, build(ctx))
	if err != nil {
		d.logger.Printf("image_pins: %v", err)
		return
	}
	if id != "" {
		d.logger.Printf("image_pins: filed the refresh bead %s", id)
	}
	if len(items) != 0 {
		d.logger.Printf("image_pins: the planner raised %d attention item(s)", len(items))
	}
}

// imagePinsSeams builds one tick's production seams for the town: the pins file
// from the gt source checkout at the repository's main, each pin's tip through
// git and the town's Forgejo, and the refresh bead in the gt source rig's own
// database, since the bead moves that rig's files.
func (d *Daemon) imagePinsSeams(ctx context.Context) imagePinsTickSeams {
	return imagePinsTickSeams{
		now:      func() time.Time { return d.clk().Now() },
		readPins: func() ([]byte, error) { return d.readImagePinsFile(ctx) },
		readTips: func(_ context.Context, data []byte) ([]ImagePinState, error) {
			return d.readImagePinTips(ctx, data)
		},
		openBead: func() (*ImagePinRefreshBead, error) {
			return imagePinsOpenRefreshBead(d.imagePinsBeads().List)
		},
		file: func(pins []ImagePinState) (string, error) {
			return imagePinsFileRefreshBead(d.imagePinsBeads().Create, pins)
		},
	}
}

// readImagePinsFile reads the pins file from the gt source checkout at the
// repository's main: the checkout rebuild_gt builds from, whose main the landing
// worker's push moves. A checkout that cannot be resolved and a file that cannot
// be read are errors, never an empty file the pin reader would refuse as a
// different failure.
func (d *Daemon) readImagePinsFile(ctx context.Context) ([]byte, error) {
	ensure := d.imagePinsEnsureFn
	if ensure == nil {
		ensure = buildsrc.Ensure
	}
	checkout, err := ensure(ctx, d.config.TownRoot, rebuildGTSourceRig, "")
	if err != nil {
		return nil, fmt.Errorf("the gt checkout the pins file is read from: %w", err)
	}
	path := filepath.Join(checkout, filepath.FromSlash(daemonimage.PinsPath))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// readImagePinTips reads every pin's source tip and its lag, through the town's
// Forgejo and git. A compare that cannot be built is the read's error: a tick
// with no compare cannot tell a fresh pin from a trailing one, so it reports
// that rather than a file of fresh pins.
func (d *Daemon) readImagePinTips(ctx context.Context, data []byte) ([]ImagePinState, error) {
	cmp, err := d.imagePinsCompare()
	if err != nil {
		return nil, err
	}
	ownerOf := func() (string, error) { return config.ResolveForgeOwner(d.config.TownRoot) }
	return ReadImagePinTips(ctx, data, ownerOf, d.imagePinsTipGit(ctx), cmp)
}

// imagePinsTipGit is the one git read a pin's tip takes: ls-remote of the
// source's main, bounded by ctx and never waiting on a credential prompt. The
// error names the call, not git's output, which a failed fetch can fill with the
// URL it was given.
func (d *Daemon) imagePinsTipGit(ctx context.Context) PinTipGit {
	return func(url string) ([]byte, error) {
		callCtx, cancel := context.WithTimeout(ctx, imagePinsGitTimeout)
		defer cancel()
		cmd := exec.CommandContext(callCtx, "git", "ls-remote", "--refs", url, pinTipRef)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		stdout, _, err := d.runCmd(cmd)
		if err != nil {
			switch {
			case errors.Is(callCtx.Err(), context.DeadlineExceeded):
				return nil, fmt.Errorf("git ls-remote %s did not answer within %s: %w", pinTipRef, imagePinsGitTimeout, err)
			case callCtx.Err() != nil:
				return nil, fmt.Errorf("git ls-remote %s stopped before it answered: %w", pinTipRef, err)
			}
			return nil, fmt.Errorf("git ls-remote %s: %w", pinTipRef, err)
		}
		return stdout, nil
	}
}

// imagePinsCompare builds the compare a pin's lag is counted with: the town's
// Forgejo, read with the viewer token. A town whose rigs name no Forgejo, and a
// client that cannot be built, are errors: the reader has no way to count a lag
// without one, and a guessed answer would read as a fresh pin.
func (d *Daemon) imagePinsCompare() (ImagePinCompare, error) {
	origin, err := d.townForgejoBaseURL()
	if err != nil {
		return nil, fmt.Errorf("image pins patrol: the town's Forgejo: %w", err)
	}
	// An origin of "" passes here: APIBaseFromRemoteURL refuses it as a URL
	// with no scheme and host, which is this reader's no-Forgejo answer.
	apiBase, err := land.APIBaseFromRemoteURL(origin)
	if err != nil {
		return nil, fmt.Errorf("image pins patrol: %w", err)
	}
	return imagePinsCompareFor(apiBase, d.imagePinsTokenFile)
}

// imagePinsCompareFor builds the town's viewer client for apiBase. An empty
// tokenPath reads the viewer role's own token file; a test names one under its
// temporary directory.
func imagePinsCompareFor(apiBase, tokenPath string) (ImagePinCompare, error) {
	opts := []forgejo.Option{
		forgejo.WithBaseURL(apiBase),
		forgejo.WithHTTPClient(&http.Client{Timeout: imagePinsHTTPTimeout}),
	}
	if tokenPath != "" {
		opts = append(opts, forgejo.WithTokenFile(tokenPath))
	}
	client, err := forgejo.NewClient(config.ForgejoRoleViewer, opts...)
	if err != nil {
		return nil, fmt.Errorf("image pins patrol: the %s token: %w", config.ForgejoRoleViewer, err)
	}
	return client, nil
}

// imagePinsBeads opens the gt source rig's database: the refresh bead names
// files in that rig, so it is filed and looked up there and nowhere else.
func (d *Daemon) imagePinsBeads() beads.Client {
	return beads.NewPinned(filepath.Join(d.config.TownRoot, rebuildGTSourceRig, ".beads"),
		beads.WithBin(d.bdPathOrDefault())).WithTimeout(imagePinsBdTimeout).WithProcessGroup()
}

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
