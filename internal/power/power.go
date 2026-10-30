// Package power implements the town's power states: full, standby and off.
//
// The states were the operator script ~/.claude/tools/gt-power.sh; this
// package ports them, with the script's state machine, ordering decisions,
// flag names and exit codes kept intact. The orderings are the contract:
//
//   - the hold is set before the ESTOP, never after. The hold stops every
//     automatic dispatcher; the ESTOP also makes the supervisor refuse
//     restarts, so setting it first would refuse the restart a polecat needs
//     while the drain is still waiting for it.
//   - a drain only waits. Nothing is killed and gt down, gt shutdown and
//     --nuke are never run (they delete polecat worktrees); a drain that
//     times out names what still runs and exits 3.
//   - off changes nothing when Dolt is down: the landing queues cannot be
//     read, so there is no evidence the town drained.
package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// State is a requested power state.
type State string

const (
	// StateFull is infra up, no ESTOP, no hold: dispatch runs.
	StateFull State = "full"
	// StateStandby is infra up, hold and ESTOP set: no dispatch, no restarts.
	StateStandby State = "standby"
	// StateOff is standby's drain, then the daemon, watchers and Forgejo
	// stopped. Dolt stays up unless --dolt.
	StateOff State = "off"
)

// StateFileName is the last requested state, at the town root under the name
// the operator script used.
const StateFileName = "power-state"

// StatePath returns the last-requested-state file for a town.
func StatePath(townRoot string) string {
	return filepath.Join(townRoot, StateFileName)
}

// DefaultForgejoURL is where the Forgejo stack answers its health probe; the
// compose service name resolves inside the stack's network and on the host.
const DefaultForgejoURL = "http://forgejo:3000"

// DaemonLabel is the daemon's launchd job, read from the supervisor's
// constant so the label cannot drift from the plist gt installs.
const DaemonLabel = templates.LaunchdLabel

// WatcherLabels are the LaunchAgents the states bring up with the daemon.
var WatcherLabels = []string{"com.gastown.deepseek-spend-watch", "com.gastown.town-stats"}

// ReleaseBotPattern is the cloud release-bot LaunchDaemon. While one is
// installed the cloud release bot and patrol poll Forgejo, so stopping the
// stack pauses deploys and makes those jobs log read errors.
const ReleaseBotPattern = "local.gt-cloud.releasebot*"

// Exit codes, kept from the operator script.
const (
	// ExitUsage is a rejected invocation or an unreadable configuration.
	ExitUsage = 2
	// ExitDrainTimeout is a drain that gave up waiting; nothing was killed.
	ExitDrainTimeout = 3
	// ExitNotHealthy is a part that did not come up healthy.
	ExitNotHealthy = 4
)

// ExitError carries the process exit code a state reports.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }

func (e *ExitError) Unwrap() error { return e.Err }

func exitError(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// Options is everything a state takes from the host and the caller. The
// zero value is not usable: the command fills the paths and RealSeams; tests
// fill the same fields with fakes.
type Options struct {
	TownRoot   string
	HomeDir    string
	ForgejoDir string
	ForgejoURL string
	// LaunchDaemonsDir holds the system-wide LaunchDaemons whose release bot
	// gates off. Default /Library/LaunchDaemons.
	LaunchDaemonsDir string
	// GTCommand is the binary every nested gt invocation runs (gt dolt, gt
	// estop, gt thaw, gt polecat). The command passes its own executable.
	// Each nested call runs with TownRoot as its working directory (the
	// script's cd "$GT_ROOT"), so the caller's cwd never decides which town
	// they act on.
	GTCommand string
	UID       int

	DryRun     bool
	Timeout    time.Duration
	WithDolt   bool
	Yes        bool
	ForceCloud bool

	Run        func(ctx context.Context, dir, name string, args ...string) (string, error)
	LookPath   func(name string) (string, error)
	HTTPStatus func(url string) (int, error)
	Glob       func(pattern string) ([]string, error)
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
	// NewSandboxBackend builds the backend a sandboxed polecat is stopped
	// through, from its registry record's profile. RealSeams fills it with
	// session.NewOpenShellBackend; nil leaves a sandbox unstopped and
	// reported, so a test that forgets the fake cannot reach the host's
	// openshell.
	NewSandboxBackend func(townRoot string, p session.Profile) (session.Backend, error)
	// RingKey reads the key the Ring is signed with. An error, or a nil
	// seam, leaves the Ring keyless: the status report says so.
	RingKey func(townRoot string) ([]byte, error)

	Out    io.Writer
	ErrOut io.Writer
}

// RealSeams fills the exec, HTTP, glob, clock and sleep seams with the real
// host's. A caller sets the paths and flags first, then calls RealSeams.
func RealSeams(o *Options) {
	if o.Run == nil {
		o.Run = runCommand
	}
	if o.LookPath == nil {
		o.LookPath = defaultLookPath
	}
	if o.HTTPStatus == nil {
		o.HTTPStatus = httpStatus
	}
	if o.Glob == nil {
		o.Glob = filepath.Glob
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.ErrOut == nil {
		o.ErrOut = os.Stderr
	}
	if o.NewSandboxBackend == nil {
		o.NewSandboxBackend = session.NewOpenShellBackend
	}
	if o.RingKey == nil {
		o.RingKey = unwiredRingKey
	}
	if o.LaunchDaemonsDir == "" {
		o.LaunchDaemonsDir = "/Library/LaunchDaemons"
	}
	if o.ForgejoURL == "" {
		o.ForgejoURL = envOr("FORGEJO_URL", DefaultForgejoURL)
	}
	if o.UID == 0 {
		o.UID = os.Getuid()
	}
}

// unwiredRingKey is RealSeams' key reader until gt ring names where the key
// lives (gt-6mlik.2): with no reference to read, the Ring reads as keyless.
func unwiredRingKey(string) ([]byte, error) {
	return nil, errors.New("no ring key reference is configured")
}

// engine is one state's run: the options plus the output helpers.
type engine struct {
	o Options
	// stranded names the sandboxes a stop could not reach or stop.
	stranded []string
}

func newEngine(o Options) *engine {
	return &engine{o: o}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultLookPath(name string) (string, error) {
	return executableLookPath(name)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// say prints one progress line.
func (e *engine) say(format string, args ...any) {
	fmt.Fprintf(e.o.Out, "gt power: "+format+"\n", args...)
}

// warn prints one problem line.
func (e *engine) warn(format string, args ...any) {
	fmt.Fprintf(e.o.ErrOut, "gt power: "+format+"\n", args...)
}

// run runs one state-changing command, or prints it under --dry-run.
func (e *engine) run(ctx context.Context, dir, name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] %s\n", line)
		return nil
	}
	fmt.Fprintf(e.o.Out, "  + %s\n", line)
	out, err := e.o.Run(ctx, dir, name, args...)
	if err != nil {
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			return fmt.Errorf("%s: %w: %s", line, err, trimmed)
		}
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
}

// exec runs a read-only probe and returns its output.
func (e *engine) exec(ctx context.Context, dir, name string, args ...string) (string, error) {
	return e.o.Run(ctx, dir, name, args...)
}

// probeTimeoutCap is the longest one probe may run, and probeTimeoutFloor the
// shortest a probe that starts at or past the deadline gets. Without the cap a
// probe that hangs (docker info on a wedged engine) holds the wait past its own
// timeout until the operator presses Ctrl-C.
const (
	probeTimeoutCap   = 30 * time.Second
	probeTimeoutFloor = time.Second
)

// waitUntil polls probe every three seconds until it reports true, the
// timeout passes, or ctx ends. Each probe runs on a context that ends at the
// wait's deadline, or after probeTimeoutCap if that is sooner, so a hung probe
// reads as not ready instead of blocking the wait.
func (e *engine) waitUntil(ctx context.Context, desc string, timeout time.Duration, probe func(context.Context) bool) error {
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] wait up to %s for: %s\n", timeout, desc)
		return nil
	}
	deadline := e.o.Now().Add(timeout)
	for !e.probeOnce(ctx, deadline, probe) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.o.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for %s", desc)
		}
		if err := e.o.Sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
	e.say("%s: ok", desc)
	return nil
}

// probeOnce runs probe on a context bounded by the wait's deadline.
func (e *engine) probeOnce(ctx context.Context, deadline time.Time, probe func(context.Context) bool) bool {
	bound := deadline.Sub(e.o.Now())
	if bound <= 0 {
		bound = probeTimeoutFloor
	}
	pctx, cancel := context.WithTimeout(ctx, min(bound, probeTimeoutCap))
	defer cancel()
	return probe(pctx)
}

// loaded reports whether launchd knows the job.
func (e *engine) loaded(ctx context.Context, label string) bool {
	_, err := e.exec(ctx, e.o.TownRoot, "launchctl", "print", fmt.Sprintf("gui/%d/%s", e.o.UID, label))
	return err == nil
}

// doltUp reports whether the town's Dolt server is running. The output is
// captured whole before it is matched: matching on a pipe with pipefail turns
// a closed pipe into a false "down".
func (e *engine) doltUp(ctx context.Context) bool {
	out, err := e.exec(ctx, e.o.TownRoot, e.o.GTCommand, "dolt", "status")
	return err == nil && strings.Contains(out, "is running")
}

// dockerUp reports whether the Docker engine answers.
func (e *engine) dockerUp(ctx context.Context) bool {
	_, err := e.exec(ctx, "", "docker", "info")
	return err == nil
}

// forgejoOK reports whether the stack's health endpoint answers 200.
func (e *engine) forgejoOK(ctx context.Context) bool {
	status, err := e.o.HTTPStatus(strings.TrimRight(e.o.ForgejoURL, "/") + "/api/healthz")
	return err == nil && status == http.StatusOK
}

// composeRunning counts the stack's running services; a failed probe reads as
// none, which suits the status report and the bring-up. Off must tell the two
// apart and uses composeProbe.
func (e *engine) composeRunning(ctx context.Context) int {
	n, _ := e.composeProbe(ctx)
	return n
}

// composeProbe counts the stack's running services and returns the probe's
// error when it could not tell.
func (e *engine) composeProbe(ctx context.Context) (int, error) {
	out, err := e.exec(ctx, e.o.ForgejoDir, "docker", "compose", "ps", "--status", "running", "-q")
	if err != nil {
		return 0, err
	}
	return len(nonBlankLines(out)), nil
}

func nonBlankLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// workingPolecats lists the town's polecats with work in flight as "rig/name":
// a working one, and a spawning one still inside its grace, whose session
// is not up yet and would otherwise read as drained.
func (e *engine) workingPolecats(ctx context.Context) ([]string, error) {
	out, err := e.exec(ctx, e.o.TownRoot, e.o.GTCommand, "polecat", "list", "--all", "--json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Rig   string `json:"rig"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parsing gt polecat list --json: %w", err)
	}
	var working []string
	for _, r := range rows {
		if r.State == "working" || r.State == "spawning" {
			working = append(working, r.Rig+"/"+r.Name)
		}
	}
	return working, nil
}

// pendingLandings sums the daemon's recorded landing queues from the health
// report, the same record gt status prints. ok is false when no landing field
// was read or any one of them could not be: an unread queue is not an empty
// one, and another rig's "0 pending" does not stand in for it. A report the
// daemon has stopped writing (older than the health stale age) is not read
// either: its zero is a count from before the daemon stopped, not a drained
// queue.
func (e *engine) pendingLandings() (int, bool) {
	report, err := townhealth.Read(e.o.TownRoot)
	if err != nil {
		return 0, false
	}
	_, stale, err := config.LoadOperationalConfig(e.o.TownRoot).GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	// Reject a stale report on its age: Effective() reports unknown for one,
	// so comparing its result to the recorded verdict sees nothing when that
	// verdict was already unknown (gt-ylgp4).
	if e.o.Now().Sub(report.At) >= stale {
		return 0, false
	}
	total, found := 0, false
	for _, f := range report.Fields {
		if f.Name != townhealth.FieldLanding {
			continue
		}
		fields := strings.Fields(f.Value)
		if len(fields) == 0 {
			return 0, false
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, false
		}
		total += n
		found = true
	}
	return total, found
}

// gcActive reports whether the daemon holds a live scheduled-maintenance
// pause on Dolt. An unreadable marker counts as active: a bootout during a
// gc would cancel it.
func (e *engine) gcActive() bool {
	data, err := os.ReadFile(doltpause.Path(e.o.TownRoot))
	if err != nil {
		return !os.IsNotExist(err)
	}
	var m doltpause.Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return true
	}
	return doltpause.Current(e.o.TownRoot, e.o.Now()) != nil &&
		strings.HasPrefix(m.Actor, "daemon/scheduled_maintenance")
}

// holdOn reports whether the operator hold exists. Only a successful stat is
// evidence of one: an unreadable path is not a hold, and reporting it as one
// would make setHold skip the write that raises the hold and clearHold skip
// the removal that lifts it.
func (e *engine) holdOn() bool {
	_, err := os.Stat(dispatch.HoldFilePath(e.o.TownRoot))
	return err == nil
}

// estopOn reports whether the town ESTOP sentinel exists.
func (e *engine) estopOn() bool {
	_, err := os.Stat(estop.FilePath(e.o.TownRoot))
	return err == nil
}

// setHold writes the operator hold.
func (e *engine) setHold() error {
	if e.holdOn() {
		return nil
	}
	path := dispatch.HoldFilePath(e.o.TownRoot)
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] touch %s\n", path)
		return nil
	}
	fmt.Fprintf(e.o.Out, "  + touch %s\n", path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return fmt.Errorf("writing the hold %s: %w", path, err)
	}
	return nil
}

// clearHold removes the operator hold.
func (e *engine) clearHold() error {
	path := dispatch.HoldFilePath(e.o.TownRoot)
	if e.o.DryRun {
		// The caller sets the hold before it clears it, so a preview names
		// the removal even though the dry run never wrote the file.
		fmt.Fprintf(e.o.Out, "  [dry-run] rm -f %s\n", path)
		return nil
	}
	// Remove without asking holdOn first: a hold path that cannot be read
	// must surface the removal error rather than leave the hold up while the
	// state reports success.
	fmt.Fprintf(e.o.Out, "  + rm -f %s\n", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the hold %s: %w", path, err)
	}
	return nil
}

// setEstop sets the town ESTOP through gt estop, whose sentinel format the
// dispatch hold and the supervisor read.
func (e *engine) setEstop(ctx context.Context, state State) error {
	if e.estopOn() {
		return nil
	}
	return e.run(ctx, e.o.TownRoot, e.o.GTCommand, "estop", "-r", "gt-power: "+string(state))
}

// record writes the last requested state.
func (e *engine) record(state string) error {
	line := fmt.Sprintf("%s %s\n", state, e.o.Now().UTC().Format(time.RFC3339))
	if e.o.DryRun {
		return nil
	}
	if err := os.WriteFile(StatePath(e.o.TownRoot), []byte(line), 0o644); err != nil {
		return fmt.Errorf("recording the power state: %w", err)
	}
	return nil
}

// lastRequested reads the last requested state, "unknown" when none is
// recorded.
func (e *engine) lastRequested() string {
	data, err := os.ReadFile(StatePath(e.o.TownRoot))
	if err != nil {
		return "unknown"
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		return s
	}
	return "unknown"
}

// releaseBots lists the cloud release-bot LaunchDaemons.
func (e *engine) releaseBots() []string {
	paths, err := e.o.Glob(filepath.Join(e.o.LaunchDaemonsDir, ReleaseBotPattern))
	if err != nil {
		return nil
	}
	return paths
}

// jobhostDown runs the devops kind cluster's shutdown hook when jobhost is on
// PATH, and does nothing when it is not. A hook that fails is reported and
// does not undo the state that was reached.
func (e *engine) jobhostDown(ctx context.Context) {
	if _, err := e.o.LookPath("jobhost"); err != nil {
		return
	}
	if err := e.run(ctx, "", "jobhost", "down"); err != nil {
		e.warn("%v (jobhost down is a hook; the power state stands)", err)
	}
}

// readings is one snapshot of every part a state or status reads.
type readings struct {
	dolt       bool
	docker     bool
	forgejo    bool
	compose    int
	daemon     bool
	watchers   map[string]bool
	estop      bool
	hold       bool
	pending    int
	pendingOK  bool
	working    []string
	workingErr error
	gc         bool
	last       string
	cloud      int
	tracker    trackerStatus
}

func (r readings) workingCount() string {
	if r.workingErr != nil {
		return "?"
	}
	return strconv.Itoa(len(r.working))
}

func (e *engine) read(ctx context.Context) readings {
	r := readings{
		estop:    e.estopOn(),
		hold:     e.holdOn(),
		last:     e.lastRequested(),
		cloud:    len(e.releaseBots()),
		watchers: map[string]bool{},
	}
	r.dolt = e.doltUp(ctx)
	r.docker = e.dockerUp(ctx)
	r.forgejo = e.forgejoOK(ctx)
	r.compose = e.composeRunning(ctx)
	r.daemon = e.loaded(ctx, DaemonLabel)
	for _, label := range WatcherLabels {
		r.watchers[label] = e.loaded(ctx, label)
	}
	r.pending, r.pendingOK = e.pendingLandings()
	r.working, r.workingErr = e.workingPolecats(ctx)
	r.gc = e.gcActive()
	r.tracker = e.readTracker()
	return r
}

// derive is the state machine the operator script documented: infra up with
// neither brake is full; infra up with either brake is standby (draining
// while a polecat still works); the stack and daemon both down is off.
func derive(r readings) string {
	up := r.dolt && r.forgejo && r.daemon
	if up && !r.estop && !r.hold {
		return "full"
	}
	if up && r.estop && !r.hold {
		return "standby (partial: ESTOP set, hold missing)"
	}
	if up && r.hold && !r.estop {
		return "standby (partial: hold set, no ESTOP; drained or draining)"
	}
	if up && (r.estop || r.hold) {
		if n := len(r.working); r.workingErr == nil && n > 0 {
			return fmt.Sprintf("standby (draining: %d polecat(s) working)", n)
		}
		return "standby"
	}
	if !r.forgejo && !r.daemon {
		if r.dolt {
			return "off"
		}
		return "off (dolt down)"
	}
	return "mixed"
}
