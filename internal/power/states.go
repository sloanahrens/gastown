package power

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/session"
)

// Status prints every part and the derived state, and changes nothing.
func Status(ctx context.Context, o Options) error {
	e := newEngine(o)
	r := e.read(ctx)
	pending := "?"
	if r.pendingOK {
		pending = strconv.Itoa(r.pending)
	}
	compose := "DOWN"
	if r.forgejo {
		compose = "ok"
	}
	line := func(label, value string) {
		fmt.Fprintf(o.Out, "%-28s %s\n", label, value)
	}
	line("state", derive(r))
	line("dolt :3307", yesNo(r.dolt, "up", "DOWN"))
	// The tracker prints beside Dolt only when a rig's route names it: a town
	// with no rig on the tracker has none to report. A town config that does
	// not resolve the endpoint says so and names the cause rather than
	// printing a guessed port, and an unreadable route table leaves "no rig
	// on the tracker" unknown.
	switch tr := r.tracker; {
	case tr.routeErr != nil:
		line("tracker", fmt.Sprintf("? (cannot read the route table: %v)", tr.routeErr))
	case !tr.on:
		// No rig is on the tracker, so there is no tracker line.
	case tr.configErr != nil:
		line("tracker", fmt.Sprintf("? (malformed tracker config: %v)", tr.configErr))
	default:
		line(fmt.Sprintf("tracker :%d", tr.port), yesNo(tr.up, "up", "DOWN"))
	}
	line("docker engine", yesNo(r.docker, "up", "DOWN"))
	line("forgejo healthz", fmt.Sprintf("%s (compose services running: %d)", compose, r.compose))
	line(fmt.Sprintf("daemon (%s)", DaemonLabel), yesNo(r.daemon, "loaded", "NOT loaded"))
	for _, label := range WatcherLabels {
		line(label, yesNo(r.watchers[label], "loaded", "NOT loaded"))
	}
	line("ESTOP file", yesNo(r.estop, "present", "absent"))
	line("hold file", yesNo(r.hold, "present", "absent"))
	fmt.Fprintln(o.Out, e.ringLine())
	line("landings pending", pending)
	line("polecats working", r.workingCount())
	line("dolt gc running", strconv.Itoa(boolInt(r.gc)))
	line("last requested", r.last)
	line("cloud-deploy daemons", fmt.Sprintf("%d plist(s), outside the states; see %s",
		r.cloud, filepath.Join(o.TownRoot, "devops", "wizard-src", "cloud-check.sh")))
	return nil
}

// ringLine is the Ring's line in the report: what its signed lease says at the
// engine's clock, or why it cannot be trusted. It is information only, since
// derive never reads it, so no power state changes with the Ring.
func (e *engine) ringLine() string {
	var key []byte
	if e.o.RingKey != nil {
		key, _ = e.o.RingKey(e.o.TownRoot)
	}
	now := e.o.Now()
	st, err := ring.Read(e.o.TownRoot, key, now)
	switch {
	case errors.Is(err, ring.ErrNoKey):
		return "ring: key unavailable"
	case err != nil:
		return fmt.Sprintf("ring: unreadable (%v)", err)
	case st.Kind == ring.Valid:
		return "ring: valid, wanes in " + spanText(st.Record.ExpiresAt.Sub(now))
	case st.Kind == ring.Waned:
		return "ring: waned " + spanText(now.Sub(st.Record.ExpiresAt)) + " ago"
	default:
		return "ring: " + st.String()
	}
}

// spanText words a span for the ring line: whole hours, or minutes under one.
func spanText(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// trackerStatus is the tracker's part of the report. A town with no rig on
// the tracker has no tracker to print.
type trackerStatus struct {
	// routeErr is a route table that could not be read. It leaves "no rig on
	// the tracker" unknown, so it is reported rather than read as none.
	routeErr error
	// on reports that some rig's route names the tracker backend.
	on bool
	// configErr is a town config that does not resolve the endpoint. The
	// port is unknown, so the report carries the cause; port and up mean
	// something only when it is nil.
	configErr error
	// port is the port the town's endpoint resolves to.
	port int
	// up reports that GET /healthz on that endpoint answered 200.
	up bool
}

// readTracker reads the tracker's part of the report. The endpoint is never
// probed for a town with no rig on the tracker: nothing there would use it.
func (e *engine) readTracker() trackerStatus {
	on, err := e.rigOnTracker()
	if err != nil {
		return trackerStatus{routeErr: err}
	}
	if !on {
		return trackerStatus{}
	}
	ep, err := config.ResolveTrackerEndpoint(e.o.TownRoot)
	if err != nil {
		return trackerStatus{on: true, configErr: err}
	}
	return trackerStatus{on: true, port: ep.Port, up: e.trackerOK(ep)}
}

// rigOnTracker reports whether some rig's route names the tracker backend. An
// unreadable route table is an error, never "no rig on the tracker": that
// would print a clean report for a town whose routes could not be read.
func (e *engine) rigOnTracker() (bool, error) {
	routes, err := beads.LoadRoutes(filepath.Join(e.o.TownRoot, ".beads"))
	if err != nil {
		return false, err
	}
	for _, r := range routes {
		if r.BackendName() == beads.BackendTracker {
			return true, nil
		}
	}
	return false, nil
}

// trackerOK reports whether the tracker's health endpoint answers 200.
func (e *engine) trackerOK(ep config.TrackerEndpoint) bool {
	status, err := e.o.HTTPStatus(ep.URL() + "/healthz")
	return err == nil && status == http.StatusOK
}

func yesNo(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Full brings every part up, clears the ESTOP and the hold, and records the
// state. The hold goes down first so nothing dispatches while the stack comes
// up.
func Full(ctx context.Context, o Options) error {
	e := newEngine(o)
	e.say("-> full")
	if err := e.setHold(); err != nil {
		return err
	}
	if err := e.ensureInfra(ctx); err != nil {
		return err
	}
	if !o.DryRun {
		if err := e.waitUntil(ctx, "daemon loaded", 30*time.Second, func(ctx context.Context) bool {
			return e.loaded(ctx, DaemonLabel)
		}); err != nil {
			return exitError(ExitNotHealthy, "daemon is not loaded; hold left in place: %v", err)
		}
		if err := e.waitUntil(ctx, "dolt up", 10*time.Second, e.doltUp); err != nil {
			return exitError(ExitNotHealthy, "Dolt is not up; hold left in place: %v", err)
		}
	}
	if err := e.clearEstop(ctx); err != nil {
		return err
	}
	if err := e.clearHold(); err != nil {
		return err
	}
	if err := e.record(string(StateFull)); err != nil {
		return err
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "  [dry-run] would then check: gt status Health\n")
		e.say("dry-run complete: nothing was changed")
		return nil
	}
	if err := o.Sleep(ctx, 5*time.Second); err != nil {
		return err
	}
	if out, err := e.exec(ctx, o.TownRoot, o.GTCommand, "status"); err == nil {
		if line := healthLine(out); line != "" {
			e.say("%s", line)
		}
	}
	e.say("full reached; dispatch resumes on the next 60 s tick")
	return nil
}

// healthLine is the first "Health" line of gt status output.
func healthLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Health") {
			return l
		}
	}
	return ""
}

// Standby sets the hold, brings the parts up, drains, then sets the ESTOP.
func Standby(ctx context.Context, o Options) error {
	e := newEngine(o)
	e.say("-> standby")
	if err := e.setHold(); err != nil {
		return err
	}
	if err := e.ensureInfra(ctx); err != nil {
		return err
	}
	if err := e.drain(ctx); err != nil {
		return err
	}
	if err := e.setEstop(ctx, StateStandby); err != nil {
		return err
	}
	e.stopSandboxes()
	e.jobhostDown(ctx)
	e.reportStrandedSandboxes()
	if err := e.record(string(StateStandby)); err != nil {
		return err
	}
	if o.DryRun {
		e.say("dry-run complete: nothing was changed")
		return nil
	}
	e.say("standby reached")
	return nil
}

// Off runs standby's drain, then stops the daemon, the watchers and the
// Forgejo stack. Dolt stays up unless WithDolt.
func Off(ctx context.Context, o Options) error {
	e := newEngine(o)
	if !o.DryRun && !o.Yes {
		return exitError(ExitUsage, "off stops the daemon, the watchers and Forgejo: re-run with --yes (or --dry-run to preview)")
	}
	if bots := e.releaseBots(); len(bots) > 0 {
		e.say("the cloud release bot and patrol poll Forgejo every 30 min: while it is stopped they log forgejo_read errors and deploys pause. Prefer 'standby' while any app is live.")
		if !o.DryRun && !o.ForceCloud {
			return exitError(ExitUsage, "re-run with --force-cloud to stop Forgejo anyway")
		}
	}
	e.say("-> off")
	if err := e.setHold(); err != nil {
		return err
	}
	// This refusal also runs under --dry-run: a preview that walked past it
	// would show a stop the real command refuses to perform.
	if !e.doltUp(ctx) {
		return exitError(ExitNotHealthy, "Dolt is down, so the landing queues cannot be read and the drain cannot be verified: run 'gt power standby' first")
	}
	if err := e.drain(ctx); err != nil {
		return err
	}
	if err := e.setEstop(ctx, StateOff); err != nil {
		return err
	}
	e.stopSandboxes()
	for _, label := range append([]string{DaemonLabel}, WatcherLabels...) {
		if !e.loaded(ctx, label) {
			continue
		}
		if err := e.run(ctx, "", "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", o.UID, label)); err != nil {
			return err
		}
	}
	running, probeErr := e.composeProbe(ctx)
	if running != 0 || probeErr != nil {
		if err := e.run(ctx, o.ForgejoDir, "docker", "compose", "stop"); err != nil {
			return err
		}
	}
	if o.WithDolt {
		e.say("stopping Dolt: bd (including the ~/.claude beads DB) stops working until 'gt power full' or 'gt dolt start'")
		if err := e.run(ctx, o.TownRoot, o.GTCommand, "dolt", "stop"); err != nil {
			return err
		}
	}
	e.jobhostDown(ctx)
	e.reportStrandedSandboxes()
	if probeErr != nil {
		// The stop went out blind, so nothing confirms the stack is down. The
		// Dolt stop and the jobhost hook above still ran: they do not depend on it.
		return exitError(ExitNotHealthy, "could not read the Forgejo stack (%v): 'docker compose stop' was issued but off is not recorded; re-run 'gt power off' to confirm", probeErr)
	}
	if err := e.record(string(StateOff)); err != nil {
		return err
	}
	if o.DryRun {
		e.say("dry-run complete: nothing was changed")
		return nil
	}
	e.say("off reached. The Forgejo stack stays stopped across a reboot; 'gt power full' starts it again. Cloud-deploy daemons untouched.")
	return nil
}

// BootHold is the login job: it writes the hold so a reboot lands in
// standby.
func BootHold(ctx context.Context, o Options) error {
	e := newEngine(o)
	if err := e.setHold(); err != nil {
		return err
	}
	if err := e.record("standby (boot)"); err != nil {
		return err
	}
	if o.DryRun {
		e.say("dry-run complete: nothing was changed")
	}
	return nil
}

// ensureInfra brings Docker, the Forgejo stack, Dolt, the watchers and the
// daemon up.
func (e *engine) ensureInfra(ctx context.Context) error {
	if !e.dockerUp(ctx) {
		if err := e.run(ctx, "", "open", "-a", "Docker"); err != nil {
			return err
		}
		if err := e.waitUntil(ctx, "docker engine", 240*time.Second, e.dockerUp); err != nil {
			return exitError(ExitNotHealthy, "Docker did not start; start Docker Desktop")
		}
	}
	if e.composeRunning(ctx) < 4 {
		if err := e.run(ctx, e.o.ForgejoDir, "docker", "compose", "up", "-d"); err != nil {
			return err
		}
	}
	if err := e.waitUntil(ctx, "forgejo healthz 200", 180*time.Second, e.forgejoOK); err != nil {
		return exitError(ExitNotHealthy, "Forgejo not healthy")
	}
	if !e.doltUp(ctx) {
		if err := e.run(ctx, e.o.TownRoot, e.o.GTCommand, "dolt", "start"); err != nil {
			return err
		}
	}
	if err := e.waitUntil(ctx, "dolt up", 60*time.Second, e.doltUp); err != nil {
		return exitError(ExitNotHealthy, "Dolt not up")
	}
	launchAgents := filepath.Join(e.o.HomeDir, "Library", "LaunchAgents")
	for _, label := range append(append([]string{}, WatcherLabels...), DaemonLabel) {
		if e.loaded(ctx, label) {
			continue
		}
		plist := filepath.Join(launchAgents, label+".plist")
		if err := e.run(ctx, "", "launchctl", "bootstrap", fmt.Sprintf("gui/%d", e.o.UID), plist); err != nil {
			return err
		}
	}
	return nil
}

// drain waits for the landing queues, the polecats and any Dolt gc to
// finish. Unknown readings are not a drained town: a queue that cannot be
// read is not an empty one.
func (e *engine) drain(ctx context.Context) error {
	// Dolt down is not a reason to skip the wait: without it the landing
	// queues cannot be read at all, so the town is not drained, and a stop
	// that treats the unreadable queues as empty is fail-open. The hold is
	// already set by the caller, and nothing has been stopped yet.
	if !e.o.DryRun && !e.doltUp(ctx) {
		return exitError(ExitDrainTimeout,
			"Dolt is unreachable, so the landing queues cannot be read and the drain cannot be verified. Nothing was killed and the hold stays set; start Dolt and re-run")
	}
	if e.o.DryRun {
		p, pok := e.pendingLandings()
		ps := "?"
		if pok {
			ps = strconv.Itoa(p)
		}
		working, werr := e.workingPolecats(ctx)
		ws := "?"
		if werr == nil {
			ws = strconv.Itoa(len(working))
		}
		fmt.Fprintf(e.o.Out, "  [dry-run] drain: landings pending=%s, polecats working=%s, dolt gc running=%d, timeout %s\n",
			ps, ws, boolInt(e.gcActive()), e.o.Timeout)
		return nil
	}
	deadline := e.o.Now().Add(e.o.Timeout)
	for {
		p, pok := e.pendingLandings()
		working, werr := e.workingPolecats(ctx)
		g := e.gcActive()
		if pok && p == 0 && werr == nil && len(working) == 0 && !g {
			e.say("drained: no pending landings, no working polecats, no Dolt gc running")
			return nil
		}
		ps, ws := "?", "?"
		if pok {
			ps = strconv.Itoa(p)
		}
		if werr == nil {
			ws = strconv.Itoa(len(working))
		}
		if !e.o.Now().Before(deadline) {
			for _, name := range working {
				e.warn("  %s working", name)
			}
			if g {
				e.warn("  a Dolt maintenance gc is running (daemon/dolt.pause); a bootout now would cancel it")
			}
			return exitError(ExitDrainTimeout,
				"drain timed out after %s: landings pending=%s, polecats working=%s. ESTOP and hold stay set; nothing was killed",
				e.o.Timeout, ps, ws)
		}
		e.say("draining: landings pending=%s, polecats working=%s, dolt gc running=%d", ps, ws, boolInt(g))
		if err := e.o.Sleep(ctx, 30*time.Second); err != nil {
			return err
		}
	}
}

// stopSandboxes stops every live openshell sandbox, once the drain has
// finished and the ESTOP is set: the ESTOP is what makes the supervisor
// refuse restarts, so a failed estop leaves the sandboxes untouched. The
// sandbox registry is the list of live sandboxes; a tmux polecat has no
// record and is never touched. A sandbox that cannot be read or stopped is
// logged and remembered, and the state stands: it keeps running with its
// workspace, and reportStrandedSandboxes names it so the operator can stop
// or delete it by hand. Under --dry-run it only prints what it would stop.
func (e *engine) stopSandboxes() {
	for _, name := range session.ListSandboxSessions(e.o.TownRoot) {
		rec, ok, err := session.ReadSandboxRecord(e.o.TownRoot, name)
		if err != nil {
			e.warn("sandbox %s: cannot read its record, not stopped: %v", name, err)
			e.stranded = append(e.stranded, name)
			continue
		}
		if !ok {
			continue
		}
		if e.o.DryRun {
			fmt.Fprintf(e.o.Out, "  [dry-run] stop sandbox %s\n", name)
			continue
		}
		fmt.Fprintf(e.o.Out, "  + stop sandbox %s\n", name)
		if err := e.stopSandbox(name, rec); err != nil {
			e.warn("sandbox %s: stop failed, the state stands: %v", name, err)
			e.stranded = append(e.stranded, name)
		}
	}
}

// stopSandbox stops one sandbox through the injected backend. A missing
// NewSandboxBackend is an error rather than a default: a caller that skipped
// RealSeams must not reach the host's real openshell by accident.
func (e *engine) stopSandbox(name string, rec session.SandboxRecord) error {
	if e.o.NewSandboxBackend == nil {
		return errors.New("no sandbox backend is configured")
	}
	b, err := e.o.NewSandboxBackend(e.o.TownRoot, session.Profile{Name: rec.Profile, Gateway: rec.Gateway, Image: rec.Image})
	if err != nil {
		return fmt.Errorf("building the backend: %w", err)
	}
	return b.Stop(name)
}

// reportStrandedSandboxes ends the output with one loud line when a sandbox
// is still running, so a stop failure is not lost among the per-step lines.
func (e *engine) reportStrandedSandboxes() {
	if len(e.stranded) == 0 {
		return
	}
	e.warn("WARNING: %d sandbox(es) may still be running: %s. The power state stands; stop or delete them by hand.",
		len(e.stranded), strings.Join(e.stranded, ", "))
}

// clearEstop clears a town ESTOP through gt thaw, which also nudges the
// sessions the stop held.
func (e *engine) clearEstop(ctx context.Context) error {
	if !e.estopOn() {
		return nil
	}
	return e.run(ctx, e.o.TownRoot, e.o.GTCommand, "thaw")
}
