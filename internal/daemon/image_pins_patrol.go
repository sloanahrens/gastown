package daemon

// The image pins patrol tick (gt-ouyejg): one tick reads the pins file and each
// pin's source tip, asks the planner, and files the one refresh bead the
// planner says is due. The tick writes neither pins file, so the refresh itself
// stays a polecat's landing. Everything the tick reads or files is a seam, so a
// unit test drives every branch with no file, network or git.
//
// This file also registers the patrol (gt-01lcq2): the ticker the daemon starts
// it on, the interval its config entry names, the town's own seams the trigger
// hands the tick, and the attention collector that publishes what the tick
// found.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/buildsrc"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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

	// imagePinsGitTimeout bounds one pin's ls-remote and one path count (its
	// fetch, its is-shallow read, its unshallow and its rev-list, each
	// separately), and imagePinsHTTPTimeout one compare request and
	// imagePinsBdTimeout one bd call.
	imagePinsGitTimeout  = 2 * time.Minute
	imagePinsHTTPTimeout = 30 * time.Second
	imagePinsBdTimeout   = 30 * time.Second

	// imagePinsJoinWait bounds how long Run waits for a tick still running
	// after the ticker stopped: longer than the attention tick's shutdown
	// bound, since a pins tick is often mid-network-read. A tick left running
	// past it only finishes its own reads and the one bead it files, and the
	// next start needs neither.
	imagePinsJoinWait = 10 * time.Second

	// imagePinHoldTicks is the consecutive failed ticks a pin reads from its last
	// good state: two is an hour at the default cadence (gt-tvtrx1).
	imagePinHoldTicks = 2
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

// imagePinsAttention is the image pins patrol's last tick as the attention
// collectors read it: the items the planner raised and the error that ended the
// tick, if any. The tick writes it on its own goroutine and the attention
// heartbeat reads it, so a mutex guards it. It is memory only: the next tick
// re-derives it after a restart, and the collectors read it only while the
// patrol is active.
type imagePinsAttention struct {
	mu      sync.Mutex
	items   []ImagePinItem
	failure error
}

// record stores one tick's outcome, replacing the previous one: each tick
// reports what it found rather than accumulating, so an item clears at the
// first tick that does not raise it.
func (s *imagePinsAttention) record(items []ImagePinItem, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]ImagePinItem(nil), items...)
	s.failure = err
}

// observed renders the last tick as attention items: one per planner item,
// keyed by its key and its pin, and the tick's error as image-pins-unreadable,
// so a tick that read nothing is an alarm rather than silence. A planner key
// with no kind is an error, never an item dropped: a reading the collectors
// cannot name is one no queue would show.
func (s *imagePinsAttention) observed() ([]attention.Item, error) {
	s.mu.Lock()
	items, failure := append([]ImagePinItem(nil), s.items...), s.failure
	s.mu.Unlock()

	out := make([]attention.Item, 0, len(items)+1)
	for _, it := range items {
		kind, ok := imagePinKind(it.Key)
		if !ok {
			return nil, fmt.Errorf("image pins patrol: the planner returned the item key %q, which has no attention kind, so no queue can show it", it.Key)
		}
		out = append(out, attention.Item{
			Key:      imagePinItemKey(it),
			Kind:     kind,
			Severity: it.Severity,
			Summary:  it.Summary,
		})
	}
	if failure != nil {
		out = append(out, attention.Item{
			Key:      imagePinsUnreadableKey,
			Kind:     attention.KindImagePinsUnreadable,
			Severity: attention.SeverityMedium,
			Summary:  fmt.Sprintf("the image pins patrol's tick failed, so no pin was read: %v", failure),
		})
	}
	return out, nil
}

// imagePinItemKey is one planner item's attention key: the item's key and, for
// a per-pin item, its pin, so two pins behind by the stale bound are two items.
func imagePinItemKey(it ImagePinItem) string {
	if it.Pin == "" {
		return it.Key
	}
	return it.Key + ":" + it.Pin
}

// imagePinKind is the attention kind of the planner item a key names. The
// planner's three keys are the patrol's three conditions; any other key has no
// kind, and observed refuses it rather than publishing an item nobody owns.
func imagePinKind(key string) (attention.Kind, bool) {
	switch key {
	case imagePinsStaleKey:
		return attention.KindImagePinsStale, true
	case imagePinsUnreadableKey:
		return attention.KindImagePinsUnreadable, true
	case imagePinsRefreshStuckKey:
		return attention.KindImagePinsRefreshStuck, true
	}
	return "", false
}

// collectImagePins returns the patrol's items of one kind from its last tick.
// One collector per kind keeps a kind's items its own, as collectors() requires;
// a daemon whose patrol is off carries no read, so the collector returns
// nothing and the queue clears what a switched-on patrol raised.
func (s *attentionSources) collectImagePins(kind attention.Kind) func(context.Context) ([]attention.Item, error) {
	return func(context.Context) ([]attention.Item, error) {
		if s.imagePins == nil {
			return nil, nil
		}
		items, err := s.imagePins()
		if err != nil {
			return nil, err
		}
		out := make([]attention.Item, 0, len(items))
		for _, it := range items {
			if it.Kind == kind {
				out = append(out, it)
			}
		}
		return out, nil
	}
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
// logged and raised, never taken for a town with nothing due: it raises
// image-pins-unreadable through the attention collector, and the items the
// planner raised before the failure are published with it.
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
	// The collector publishes the tick's outcome, failure included: the items
	// the planner raised before it, and the error that ended it.
	d.imagePinsAttention.record(items, err)
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

// stopImagePinsPatrol stops the patrol's ticker and joins the tick it may have
// running, bounded, so Run's return leaves no tick reading the town after the
// daemon is gone. A tick still running at the bound is logged and left: its
// reads are bounded by imagePinsTickTimeout, and a shutdown does not wait for
// one.
func (d *Daemon) stopImagePinsPatrol(stop func()) {
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.imagePinsCycles.Wait()
		close(done)
	}()
	bound := d.imagePinsJoinWait
	if bound <= 0 {
		bound = imagePinsJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("image_pins: a tick is still running %s after the ticker stopped; Run leaves without it", bound)
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
		file: func(pins []ImagePinState, filedAt time.Time) (string, error) {
			return imagePinsFileRefreshBead(d.imagePinsBeads().Create, pins, filedAt)
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
// Forgejo and git, then fills each trailing pin's Age through the same git. A
// compare that cannot be built is the read's error: a tick with no compare
// cannot tell a fresh pin from a trailing one, so it reports that rather than a
// file of fresh pins. A failed read comes back as its last good state until its
// failures pass imagePinHoldTicks, so one failed read raises no item.
func (d *Daemon) readImagePinTips(ctx context.Context, data []byte) ([]ImagePinState, error) {
	cmp, err := d.imagePinsCompare()
	if err != nil {
		return nil, err
	}
	ownerOf := func() (string, error) { return config.ResolveForgeOwner(d.config.TownRoot) }
	pins, err := parseImagePins(data, ownerOf)
	var states []ImagePinState
	if err == nil {
		states, err = ReadImagePinTips(ctx, data, ownerOf, d.imagePinsTipGit(ctx), d.imagePinsPathCount(ctx), cmp)
	}
	if err != nil {
		return nil, err
	}
	urls := make(map[string]string, len(pins))
	for _, pin := range pins {
		urls[pin.Name] = pin.URL
	}
	fillImagePinAges(states, func(name string) string { return urls[name] }, d.imagePinsAge(ctx), d.logger)
	d.holdFailedImagePins(states)
	return states, nil
}

// holdFailedImagePins reads a pin whose read failed from its last good state for
// its first imagePinHoldTicks failures at the same pinned commit, then keeps the
// error; a good read starts the count over, a pin with no good read or a failed
// read at a moved commit keeps its error at once, and each held pin is logged
// instead of queued as an item.
func (d *Daemon) holdFailedImagePins(states []ImagePinState) {
	if d.imagePinsLast == nil {
		d.imagePinsLast, d.imagePinsFails = map[string]ImagePinState{}, map[string]int{}
	}
	for i := range states {
		st := &states[i]
		if st.Err == nil {
			d.imagePinsLast[st.Name], d.imagePinsFails[st.Name] = *st, 0
			continue
		}
		last, ok := d.imagePinsLast[st.Name]
		// The held state is the last good read of the commit it was read at: a
		// failed read after the file moved that pin is kept at once, and its
		// count starts over, since a count is for one commit (gt-ki0ir9).
		if !ok || last.Pin != st.Pin {
			d.imagePinsFails[st.Name] = 0
			continue
		}
		if d.imagePinsFails[st.Name] >= imagePinHoldTicks {
			continue
		}
		d.imagePinsFails[st.Name]++
		d.logger.Printf("image_pins: image pin %s cannot be read (%d of %d held ticks), so this tick reads its last good state: %v", st.Name, d.imagePinsFails[st.Name], imagePinHoldTicks, st.Err)
		*st = last
	}
}

// fillImagePinAges fills each trailing pin's Age from ageOf: a state read
// without error whose Tip differs from its Pin is measured against that tip, at
// the URL urlOf names for it. A pin at its tip has not aged against it, and a
// state with an Err is already unreadable, so neither makes a call. A failed age
// read is that pin's AgeErr with no Age, logged once through logger: the age
// rule's input is missing, while the pin's commit count still reads.
func fillImagePinAges(states []ImagePinState, urlOf func(name string) string, ageOf func(url, pin, tip string) (time.Duration, error), logger *log.Logger) {
	for i := range states {
		st := &states[i]
		if st.Err != nil || st.Tip == st.Pin {
			continue
		}
		age, err := ageOf(urlOf(st.Name), st.Pin, st.Tip)
		if err != nil {
			st.AgeErr = err
			// An Age left on the state is cleared with the failure, so the
			// planner's age rule cannot fire on an age this read did not take.
			st.Age = 0
			logger.Printf("image_pins: the age of image pin %s could not be read: %v", st.Name, err)
			continue
		}
		st.Age = age
	}
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

// imagePinsPathCount is the production PinPathCount: git's count of the commits
// from the pin to the tip that touch the paths, or an error when git cannot count.
func (d *Daemon) imagePinsPathCount(ctx context.Context) PinPathCount {
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, "devops"))
	return func(url, pin, tip string, paths []string) (int, error) {
		// Both shas are refused before any git call: the fetch runs first, and an
		// option-shaped pin or tip is spliced into the rev-list argument below.
		if !pinShaForm.MatchString(pin) {
			return 0, fmt.Errorf("git rev-list --count: pin %q is not a full 40-character lowercase hex sha", pin)
		}
		if !pinShaForm.MatchString(tip) {
			return 0, fmt.Errorf("git rev-list --count: tip %q is not a full 40-character lowercase hex sha", tip)
		}
		if _, err := d.imagePinsGit(ctx, repo, "fetch", "--filter=blob:none", url, "+main:refs/gt/pin-tip"); err != nil {
			return 0, err
		}
		// A shallow clone's boundary commit has no local parent, so rev-list
		// reads it as a root touching every path. Only a shallow clone is
		// unshallowed: git refuses --unshallow on a complete one. Any answer
		// but true or false is an error, never read as either clone.
		shallow, err := d.imagePinsGit(ctx, repo, "rev-parse", "--is-shallow-repository")
		if err != nil {
			return 0, err
		}
		switch strings.TrimSpace(string(shallow)) {
		case "true":
			if _, err := d.imagePinsGit(ctx, repo, "fetch", "--unshallow", "--filter=blob:none", url, "+main:refs/gt/pin-tip"); err != nil {
				return 0, err
			}
		case "false":
		default:
			return 0, fmt.Errorf("git rev-parse --is-shallow-repository answered %q, not true or false", strings.TrimSpace(string(shallow)))
		}
		argv := append([]string{"rev-list", "--count", pin + ".." + tip, "--"}, paths...)
		out, err := d.imagePinsGit(ctx, repo, argv...)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0, fmt.Errorf("git rev-list --count %s..%s answered a non-number", sha12(pin), sha12(tip))
		}
		if n < 0 {
			return 0, fmt.Errorf("git rev-list --count %s..%s answered a negative count %d", sha12(pin), sha12(tip), n)
		}
		return n, nil
	}
}

// imagePinsAge is the production age read: the tip's committer time minus the
// pin's, fetched the way imagePinsPathCount fetches. A pin or tip that is not 40
// lowercase hex is refused before any git call. A failed git call, a non-numeric
// answer or a negative age (the pin newer than its tip) is an error (gt-qi51ka).
func (d *Daemon) imagePinsAge(ctx context.Context) func(url, pin, tip string) (time.Duration, error) {
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, "devops"))
	return func(url, pin, tip string) (time.Duration, error) {
		// Both shas are refused before any git call: they reach git show below.
		if !pinShaForm.MatchString(pin) {
			return 0, fmt.Errorf("git show -s --format=%%ct: pin %q is not a full 40-character lowercase hex sha", pin)
		}
		if !pinShaForm.MatchString(tip) {
			return 0, fmt.Errorf("git show -s --format=%%ct: tip %q is not a full 40-character lowercase hex sha", tip)
		}
		if _, err := d.imagePinsGit(ctx, repo, "fetch", "--filter=blob:none", url, "+main:refs/gt/pin-tip"); err != nil {
			return 0, err
		}
		out, err := d.imagePinsGit(ctx, repo, "show", "-s", "--format=%ct", pin, tip)
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 {
			return 0, fmt.Errorf("git show -s --format=%%ct %s %s answered %d timestamps, not the two commits' committer times", sha12(pin), sha12(tip), len(fields))
		}
		pinSec, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("git show -s --format=%%ct %s %s answered a non-number for the pin", sha12(pin), sha12(tip))
		}
		tipSec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("git show -s --format=%%ct %s %s answered a non-number for the tip", sha12(pin), sha12(tip))
		}
		// Through time.Time, so an absurd pair saturates instead of wrapping.
		age := time.Unix(tipSec, 0).Sub(time.Unix(pinSec, 0))
		if age < 0 {
			return 0, fmt.Errorf("git show -s --format=%%ct %s %s: the pin's committer time is after the tip's, a negative age %s", sha12(pin), sha12(tip), age)
		}
		return age, nil
	}
}

// imagePinsGit runs one git command in repo, bounded by imagePinsGitTimeout and
// never waiting on a credential prompt. The error names the command and carries
// git's first stderr line only: the rest of the output can repeat the URL it was
// given, and pinRepo has already refused a URL that carries a credential.
func (d *Daemon) imagePinsGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, imagePinsGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, "git", append([]string{"--git-dir=" + repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, stderr, err := d.runCmd(cmd)
	if err != nil {
		if first := strings.TrimSpace(firstLine(string(stderr))); first != "" {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, first)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
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

// imagePinsBeads opens a client for the gt source rig's database directory: the
// refresh bead names files in that rig, so its bd calls run there. They carry the
// town's mutation routing environment over that rig's .beads, so BEADS_DIR names
// the rig's .beads — the store the call reads, which the bd gateway forwarder
// places the call in — and a refresh bead created with no ID prefix files into
// the rig's database, not the town's (gt-dl9ug0). An inherited database pin is
// stripped, so prefix routing chooses the database and the town's named Dolt
// endpoint the server (gt-ff9cxi). The wrapper is NewPlain because the bd
// deadline and the process group are asked on the plain wrapper, which panics
// when handed any other (gt-fu9c8a).
func (d *Daemon) imagePinsBeads() beads.Client {
	opts := []beads.Option{beads.WithBin(d.bdPathOrDefault())}
	if d.imagePinsBeadRunner != nil {
		opts = append(opts, beads.WithEnvRunner(d.imagePinsBeadRunner))
	}
	rigBeadsDir := filepath.Join(d.config.TownRoot, rebuildGTSourceRig, ".beads")
	return beads.NewPlain(rigBeadsDir,
		bdMutationRoutingEnvOver(os.Environ(), d.config.TownRoot, rigBeadsDir), opts...).
		WithTimeout(imagePinsBdTimeout).WithProcessGroup()
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
	// file files the refresh bead for the pins the planner named, read at
	// filedAt, and returns its id.
	file func(pins []ImagePinState, filedAt time.Time) (string, error)
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
	// One clock read, so the open bead's age and the filed bead's filing time
	// are measured against the same instant.
	now := s.now()
	plan := s.planner()(pins, open, now)
	if len(plan.File) == 0 {
		return "", plan.Items, nil
	}
	id, err := s.file(plan.File, now)
	if err != nil {
		return "", plan.Items, fmt.Errorf("image pins patrol: filing the refresh bead: %w", err)
	}
	if id == "" {
		return "", plan.Items, errors.New("image pins patrol: the filer returned no bead id: the refresh bead it filed cannot be named")
	}
	return id, plan.Items, nil
}
