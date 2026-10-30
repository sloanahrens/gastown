package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/tracker/api"
)

// bdHandshakeGatedCommands are the commands that run the town: they start
// the daemon, start agent sessions, or spawn polecats. Each refuses to run
// unless the town config files parse (townconfig.Check, gt-fcxe9.10, gt-y3pgh.1)
// and the town's store passes the startup handshake (runTownStartHandshake).
// Every other command, including every read-only one, runs against whatever
// bd is installed. TestBDHandshakeGatedCommandsExist pins that each path
// exists, so a rename cannot drop a command out of the gate.
var bdHandshakeGatedCommands = map[string]bool{
	"gt up":              true,
	"gt daemon start":    true,
	"gt daemon run":      true,
	"gt daemon restart":  true,
	"gt sling":           true,
	"gt crew start":      true,
	"gt crew restart":    true,
	"gt session start":   true,
	"gt session restart": true,
	"gt scheduler run":   true,
	"gt formula run":     true,
}

// bdHandshakeTownVerbs are the command names that start or dispatch things.
// Every command with one of these names must be gated or listed in
// bdHandshakeNotTownRunning with the reason, so a new start/run command
// cannot silently skip the gate (TestBDHandshakeClassifiesEveryTownVerb).
var bdHandshakeTownVerbs = map[string]bool{
	"up": true, "start": true, "restart": true, "run": true, "spawn": true,
	"sling": true, "resume": true,
}

// bdHandshakeNotTownRunning are start/run-named commands that start no agent
// session, daemon or polecat, with the reason each is exempt.
var bdHandshakeNotTownRunning = map[string]string{
	"gt agent resume":      "clears a pause flag; starts no session",
	"gt scheduler resume":  "clears a pause flag; dispatch goes through gt sling (gated)",
	"gt plugin run":        "runs one plugin gate; starts no agent session",
	"gt reaper run":        "closes stale beads through bd; starts nothing",
	"gt metrics run":       "the hourly town-metrics plugin: snapshot and analysis only, no session, daemon or polecat; the plugin it replaces was never gated",
	"gt slot run":          "wraps a test suite in the container slot",
	"gt host agent run":    "writes the host probe file a container daemon reads; starts no session, daemon or polecat",
	"gt daemon scratch up": "ACCEPTED RISK: starts the scratch project's own containers from flags, before the scratch town exists, so there is no town config or bd for the handshake to read; the live town is named only to be refused and nothing of it is started. The command path never executes bd (pinned by TestDaemonScratchNeverRunsBD), so this exemption cannot grow into a path that needs the handshake",
	"gt spec sling":        "runs one sling a gt spec dispatch tick planned, which the tick made itself with no gate; only the daemon runs it, and the daemon passed the gate at start",
	"gt dolt start":        "starts the Dolt server, which recovery needs when bd is broken",
	"gt dolt restart":      "restarts the Dolt server, which recovery needs when bd is broken",
	"gt bird start":        "starts claude, not a town session, and must run when bd is missing or outdated (gt-qipl5)",
	"gt bird spawn":        "opens the tab a gt bird start runs in, so it starts claude and not a town session, and must run when bd is missing or outdated (gt-b58mk)",
}

// bdHandshakeTimeout bounds the whole handshake: the version and schema reads,
// and the retries the schema read makes while Dolt is still starting
// (gt-2gctr).
const bdHandshakeTimeout = 30 * time.Second

// trackerProbeTimeout bounds the tracker's readiness read, so a tracker that
// accepts a connection and then stalls is refused inside the handshake's
// budget.
const trackerProbeTimeout = 5 * time.Second

// trackerProbeBodyLimit bounds the readiness body read: the answer is two
// fields, and a probe is never a way to pull a large body through the gate.
const trackerProbeBodyLimit = 64 << 10

// trackerReadyStatus is the status trackerd reports when it is ready. The
// status field decides, not the status code: a 200 whose report says anything
// else is a trackerd that answers but is not ready, and it is refused.
const trackerReadyStatus = "ready"

// townStartGate is the startup gate for town-running commands and agent
// session starts: the town config files must parse (checked on every call,
// so a file broken while the daemon runs stops its next session start), then
// the town's store must pass its handshake. A pass of the handshake is
// remembered for the life of the gate; a refusal is not: the daemon starts
// sessions for hours, and once the operator installs the right bd, Dolt
// recovers or trackerd comes back, the next session start must see it.
type townStartGate struct {
	// townRoot finds the town the command runs in ("" outside one).
	townRoot func() string
	// config reports a town config file under townRoot that does not parse.
	config func(townRoot string) error
	// handshake runs the startup handshake against townRoot's store.
	handshake func(ctx context.Context, townRoot string) error

	mu     sync.Mutex
	passed bool
}

// processTownStartGate is the gate of this process: the cwd's town (or
// GT_TOWN_ROOT / GT_ROOT), its config files, and the store its own route names.
var processTownStartGate = &townStartGate{
	townRoot:  detectTownRootFromCwd,
	config:    checkTownConfig,
	handshake: runTownStartHandshake,
}

// runTownStartHandshake runs the startup handshake for the store the town's
// own route names: the bd handshake for a Dolt town, the tracker's for a town
// on the tracker (gt-p85pw.110).
func runTownStartHandshake(ctx context.Context, townRoot string) error {
	// Read the town's own store, never whatever beads database the current
	// directory happens to resolve to.
	if townRoot == "" {
		return fmt.Errorf("%w: not in a Gas Town workspace, so the town database's schema level cannot be read", deps.ErrBDHandshake)
	}
	return townHandshakeFor(ctx, townRoot, bdTownHandshake, trackerTownHandshake, os.Stderr)
}

// townHandshakeFor is runTownStartHandshake with both handshakes passed in, so
// a test drives the route decision with neither a bd nor a tracker, and with
// w taking the warning for a route table the gate cannot read.
func townHandshakeFor(ctx context.Context, townRoot string, bd, tracker func(context.Context, string) error, w io.Writer) error {
	backend := townRouteBackend(townRoot, w)
	switch backend {
	case beads.BackendDolt:
		return bd(ctx, townRoot)
	case beads.BackendTracker:
		return tracker(ctx, townRoot)
	}
	return fmt.Errorf("the town's route names the backend %q, which is neither %q nor %q",
		backend, beads.BackendDolt, beads.BackendTracker)
}

// townRouteBackend is the backend the town's own route names: the route whose
// .beads directory is the town's, read here as internal/beads/backend reads a
// rig's. A table that names no such route is Dolt, as a rig the table does not
// name is Dolt, and a town with no route table at all has always been Dolt: a
// missing table is not a broken one.
//
// A table that cannot be read is Dolt as well, with the read error going to w:
// a town whose routes.jsonl is corrupt keeps the gate it had before a route
// could name the tracker, instead of being refused for every town-running
// command. That is not a hole, because the Dolt path is the bd handshake, so a
// tracker town behind an unreadable table still fails closed, refused for a
// missing bd build rather than for the table. Only a table that parses and
// names a backend that is neither Dolt nor the tracker is returned as written,
// and the caller refuses it: that backend was written down, and this gate does
// not know what it speaks.
func townRouteBackend(townRoot string, w io.Writer) string {
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(townRoot))
	if err != nil {
		warnUnreadableRouteOnce(w, townRoot, err)
		return beads.BackendDolt
	}
	townBeads := filepath.Clean(beads.GetTownBeadsPath(townRoot))
	for _, r := range routes {
		if filepath.Clean(filepath.Join(townRoot, r.Path, ".beads")) == townBeads {
			return r.BackendName()
		}
	}
	return beads.BackendDolt
}

// unreadableRouteWarned records the towns whose unreadable route table this
// process has already reported, so a town that keeps failing its handshake
// names the read error once rather than on every gated command.
var unreadableRouteWarned sync.Map

// warnUnreadableRouteOnce writes the route table read error to w, once per
// town. The gate falls back to the bd handshake for such a town, so the
// warning is the only place the read error is visible.
func warnUnreadableRouteOnce(w io.Writer, townRoot string, err error) {
	if _, seen := unreadableRouteWarned.LoadOrStore(filepath.Clean(townRoot), struct{}{}); seen {
		return
	}
	fmt.Fprintf(w, "Warning: the route table at %s cannot be read (%v); the start gate runs the bd handshake, as it did before a route could name the tracker\n",
		filepath.Join(beads.GetTownBeadsPath(townRoot), beads.RoutesFileName), err)
}

// bdTownHandshake is the bd handshake for the town's own database: the bd on
// PATH must be a fork build whose schema level equals the database's.
func bdTownHandshake(ctx context.Context, townRoot string) error {
	return checkTownBDHandshake(ctx, deps.NewHandshakeStore(ctx, townRoot))
}

// checkTownBDHandshake is bdTownHandshake on a store the caller passes, so a
// unit test drives the refusal from a bd report it writes and runs no bd.
func checkTownBDHandshake(ctx context.Context, store deps.Store) error {
	path, _ := exec.LookPath("bd")
	_, err := deps.CheckBDHandshake(ctx, path, store)
	return err
}

// trackerTownHandshake is the tracker's handshake for a town whose own route
// names that backend: a reachable, migrated trackerd passes.
func trackerTownHandshake(ctx context.Context, townRoot string) error {
	return trackerTownHandshakeWith(ctx, townRoot, nil)
}

// trackerTownHandshakeWith is trackerTownHandshake with the probe's transport
// passed in: nil dials the tracker over the default transport, and a test
// fakes the answer, so a unit test opens no socket (testpolicy no-network).
func trackerTownHandshakeWith(ctx context.Context, townRoot string, transport http.RoundTripper) error {
	endpoint, err := config.ResolveTrackerEndpoint(townRoot)
	if err != nil {
		return fmt.Errorf("%w: the town's tracker endpoint cannot be read: %w", deps.ErrTrackerHandshake, err)
	}
	return deps.CheckTrackerHandshake(ctx, townTrackerProbe{url: endpoint.URL(), transport: transport}, api.SchemaLevel)
}

// townTrackerProbe reads the town tracker's readiness over /readyz, the probe
// trackerd serves without a token, so the gate asks it from an operator's
// shell before any seat holds a credential.
type townTrackerProbe struct {
	url string
	// transport answers the probe; nil resolves the tracker over
	// http.DefaultTransport.
	transport http.RoundTripper
}

var _ deps.Tracker = townTrackerProbe{}

// URL names the tracker in a refusal.
func (p townTrackerProbe) URL() string { return p.url }

// trackerProbeClient is the client every probe read goes through: rt, with nil
// resolving the tracker itself, bounded by trackerProbeTimeout so a tracker
// that accepts a connection and then stalls is refused inside the handshake's
// budget. A test passes a fake transport through the same call, so the client
// a probe uses is the production one and no test opens a socket.
func trackerProbeClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: trackerProbeTimeout}
}

// Readyz asks the tracker whether it is ready. Every failure names the
// readiness answer, never a database cause: trackerd's body says what failed
// and keeps why in its own log.
func (p townTrackerProbe) Readyz(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/readyz", nil)
	if err != nil {
		return 0, fmt.Errorf("the tracker at %s cannot be asked: %w", p.url, err)
	}
	resp, err := trackerProbeClient(p.transport).Do(req)
	if err != nil {
		return 0, fmt.Errorf("the tracker did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, trackerProbeBodyLimit))
	if err != nil {
		return 0, fmt.Errorf("the tracker's answer could not be read: %w", err)
	}
	var answer struct {
		Status      string `json:"status"`
		SchemaLevel int    `json:"schema_level"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, fmt.Errorf("the tracker answered %d with a body that is not its readiness report", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if answer.Message != "" {
			return 0, fmt.Errorf("the tracker is not ready: %s", answer.Message)
		}
		return 0, fmt.Errorf("the tracker answered %d to its readiness probe", resp.StatusCode)
	}
	if answer.Status != trackerReadyStatus {
		return 0, fmt.Errorf("the tracker answered %d but its readiness report says %q, not %q",
			resp.StatusCode, answer.Status, trackerReadyStatus)
	}
	if answer.SchemaLevel <= 0 {
		return 0, errors.New("the tracker reported no schema level")
	}
	return answer.SchemaLevel, nil
}

// requiresBDHandshake reports whether cmd is a town-running command.
func requiresBDHandshake(cmd *cobra.Command) bool {
	return cmd != nil && bdHandshakeGatedCommands[cmd.CommandPath()]
}

// gateTownCommand runs gate for a town-running cmd and passes every other
// command through.
func gateTownCommand(cmd *cobra.Command, gate *townStartGate) error {
	if !requiresBDHandshake(cmd) {
		return nil
	}
	return gate.require()
}

// requireHandshake returns the handshake's refusal, if any. A pass is
// cached; a refusal is re-checked on the next call.
func (g *townStartGate) requireHandshake() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.passed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bdHandshakeTimeout)
	defer cancel()
	if err := g.handshake(ctx, g.townRoot()); err != nil {
		return err
	}
	g.passed = true
	return nil
}

// checkTownConfig checks the config files of the town at townRoot. Outside a
// town ("") it refuses: files it cannot find are not files that parse.
func checkTownConfig(townRoot string) error {
	if townRoot == "" {
		return errors.New("not in a Gas Town workspace, so the town config files cannot be checked")
	}
	// Load, not Check: a town-running command needs a whole town, so a root
	// found by its mayor/ directory alone (no mayor/town.json) is refused.
	town, err := townconfig.Load(townRoot)
	if err != nil {
		return err
	}
	warnLiteralSecretsOnce(os.Stderr, town)
	return nil
}

// require is the gate: the town config check first, then the handshake.
func (g *townStartGate) require() error {
	if err := g.config(g.townRoot()); err != nil {
		return err
	}
	return g.requireHandshake()
}

// installSessionGate makes every agent-session start in this process run the
// town config check and the handshake first (bdgate.Require in session.StartSession and the role
// managers' Start). It covers session starts the command list above does not
// name, and the daemon's own restarts. Only Execute installs it, so package
// tests that call Start directly run ungated.
func installSessionGate() {
	bdgate.Set(processTownStartGate.require)
}
