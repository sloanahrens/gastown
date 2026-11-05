package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// pollutedEnv is an environment a daemon started from an agent session would
// carry: another agent's identity in every identity variable.
func pollutedEnv() []string {
	env := []string{"PATH=/usr/bin"}
	for _, k := range agentconfig.IdentityEnvVars {
		env = append(env, k+"=gastown/crew/sloan")
	}
	return env
}

// assertDaemonIdentity fails unless env names the daemon as its actor and
// carries no other identity variable except those in allowed.
func assertDaemonIdentity(t *testing.T, what string, env []string, allowed ...string) {
	t.Helper()
	call := cliCall{env: env}
	if got := call.getenv("BD_ACTOR"); got != daemonActor {
		t.Errorf("%s: BD_ACTOR = %q, want %q", what, got, daemonActor)
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "BD_ACTOR" && slices.Contains(agentconfig.IdentityEnvVars, k) && !slices.Contains(allowed, kv) {
			t.Errorf("%s: carries another identity: %s", what, kv)
		}
	}
}

// TestWithPolecatImageTrimContext pins the one place the plugin's trim context
// is published: the installer's environment, never this process's, and only
// when the sweep is on and runs on an engine of its own (gt-bxp17).
func TestWithPolecatImageTrimContext(t *testing.T) {
	t.Parallel()
	runner := func(backend, context string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
			Enabled: true,
			Runner:  &agentconfig.TierSweepRunnerConfig{Backend: backend, Context: context},
		}}}
	}
	for _, tc := range []struct {
		name string
		cfg  *DaemonPatrolConfig
		want string
	}{
		{name: "a docker engine", cfg: runner(agentconfig.TierSweepRunnerDocker, "colima-gt-control"), want: "colima-gt-control"},
		{name: "a kubernetes engine", cfg: runner(agentconfig.TierSweepRunnerKubernetes, "colima-gt-control"), want: "colima-gt-control"},
		{name: "no patrol config"},
		{name: "no patrols", cfg: &DaemonPatrolConfig{}},
		{name: "no tier sweep", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{name: "a disabled sweep", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Runner: &agentconfig.TierSweepRunnerConfig{
			Backend: agentconfig.TierSweepRunnerDocker, Context: "colima-gt-control",
		}}}}},
		{name: "no runner", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}}},
		{name: "an unknown backend", cfg: runner("podman", "colima-gt-control")},
		{name: "an empty context", cfg: runner(agentconfig.TierSweepRunnerDocker, "")},
		{name: "a context docker would read as another option", cfg: runner(agentconfig.TierSweepRunnerDocker, "-rf")},
		{name: "a context that is a path", cfg: runner(agentconfig.TierSweepRunnerDocker, "colima/gt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{patrolConfig: tc.cfg, logger: log.New(io.Discard, "", 0)}
			env := d.withPolecatImageTrimContext([]string{"PATH=/usr/bin"})
			got := cliCall{env: env}.getenv(polecatImageTrimContextEnv)
			if got != tc.want {
				t.Errorf("%s = %q, want %q", polecatImageTrimContextEnv, got, tc.want)
			}
			if tc.want == "" && len(env) != 1 {
				t.Errorf("env = %q, want the input alone", env)
			}
		})
	}
}

// TestWithPolecatImageTrimContextReplacesAnInheritedValue pins that a value the
// daemon inherited does not survive beside the one it sets: the last entry
// wins for a child that reads the environment (gt-bxp17).
func TestWithPolecatImageTrimContextReplacesAnInheritedValue(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
			Enabled: true,
			Runner:  &agentconfig.TierSweepRunnerConfig{Backend: agentconfig.TierSweepRunnerDocker, Context: "colima-gt-control"},
		}}},
		logger: log.New(io.Discard, "", 0),
	}
	env := d.withPolecatImageTrimContext([]string{polecatImageTrimContextEnv + "=stale"})
	var values []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == polecatImageTrimContextEnv {
			values = append(values, v)
		}
	}
	if len(values) != 1 || values[0] != "colima-gt-control" {
		t.Errorf("%s entries = %q, want only the configured context", polecatImageTrimContextEnv, values)
	}
}

// TestWithPolecatImageTrimContextRefusesADisabledSweep pins the town's disabled
// list, which IsPatrolEnabled alone does not read (gt-bxp17).
func TestWithPolecatImageTrimContextRefusesADisabledSweep(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
			Enabled: true,
			Runner:  &agentconfig.TierSweepRunnerConfig{Backend: agentconfig.TierSweepRunnerDocker, Context: "colima-gt-control"},
		}}},
		disabledPatrols: map[string]bool{"tier_sweep": true},
		logger:          log.New(io.Discard, "", 0),
	}
	if env := d.withPolecatImageTrimContext([]string{"PATH=/usr/bin"}); len(env) != 1 {
		t.Errorf("env = %q, want the input alone for a disabled sweep", env)
	}
}

// TestWithPolecatImageTrimContextHonoursContainerOff pins the container rule the
// caller applies: container_off skips the sweep only in the container daemon,
// so the launchd daemon still hands the installer its context (gt-h3ujp).
func TestWithPolecatImageTrimContextHonoursContainerOff(t *testing.T) {
	t.Parallel()
	const trim = polecatImageTrimContextEnv + "=colima-gt-control"
	for _, tc := range []struct {
		name         string
		containerOff []string
		container    bool
		want         []string
	}{
		{name: "container daemon skips a listed sweep", containerOff: []string{"tier_sweep"}, container: true, want: []string{"PATH=/usr/bin"}},
		{name: "launchd daemon runs a listed sweep", containerOff: []string{"tier_sweep"}, want: []string{"PATH=/usr/bin", trim}},
		{name: "container daemon runs an unlisted sweep", containerOff: []string{"other_patrol"}, container: true, want: []string{"PATH=/usr/bin", trim}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{
				patrolConfig: &DaemonPatrolConfig{
					Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
						Enabled: true,
						Runner:  &agentconfig.TierSweepRunnerConfig{Backend: agentconfig.TierSweepRunnerDocker, Context: "colima-gt-control"},
					}},
					ContainerOffSetting: tc.containerOff,
				},
				containerMode: tc.container,
				logger:        log.New(io.Discard, "", 0),
			}
			if got := d.withPolecatImageTrimContext([]string{"PATH=/usr/bin"}); !slices.Equal(got, tc.want) {
				t.Errorf("env = %q, want %q", got, tc.want)
			}
		})
	}
}

// trimEnvRunner builds a patrol config whose tier sweep runs on backend with
// context, enabled or not.
func trimEnvRunner(enabled bool, backend, context string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
		Enabled: enabled,
		Runner:  &agentconfig.TierSweepRunnerConfig{Backend: backend, Context: context},
	}}}
}

// TestPolecatImageTrimEnvActiveReturnsOneEntry pins the entry gt update hands
// the installer: the sweep's own engine, as a single KEY=context element
// (gt-o4sk8.22).
func TestPolecatImageTrimEnvActiveReturnsOneEntry(t *testing.T) {
	t.Parallel()
	env, err := PolecatImageTrimEnv(trimEnvRunner(true, agentconfig.TierSweepRunnerDocker, "colima-gt-control"))
	if err != nil {
		t.Fatalf("PolecatImageTrimEnv() error = %v, want nil", err)
	}
	want := []string{polecatImageTrimContextEnv + "=colima-gt-control"}
	if !slices.Equal(env, want) {
		t.Errorf("PolecatImageTrimEnv() = %q, want %q", env, want)
	}
}

// TestPolecatImageTrimEnvNoEntryWhenInactiveOrEmpty pins that a sweep that is
// off, has no engine of its own or names no context contributes nothing
// (gt-o4sk8.22).
func TestPolecatImageTrimEnvNoEntryWhenInactiveOrEmpty(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *DaemonPatrolConfig
	}{
		{name: "no patrol config"},
		{name: "no patrols", cfg: &DaemonPatrolConfig{}},
		{name: "no tier sweep", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{name: "a disabled sweep", cfg: trimEnvRunner(false, agentconfig.TierSweepRunnerDocker, "colima-gt-control")},
		{name: "no runner", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}}},
		{name: "an unknown backend", cfg: trimEnvRunner(true, "podman", "colima-gt-control")},
		{name: "an empty context", cfg: trimEnvRunner(true, agentconfig.TierSweepRunnerDocker, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := PolecatImageTrimEnv(tc.cfg)
			if err != nil {
				t.Errorf("PolecatImageTrimEnv() error = %v, want nil", err)
			}
			if env != nil {
				t.Errorf("PolecatImageTrimEnv() = %q, want nil", env)
			}
		})
	}
}

// TestPolecatImageTrimEnvRejectsAnInvalidContext pins the input guard: a
// context that is not one Docker context name is refused, and the error names
// it so the typo is findable (gt-o4sk8.22).
func TestPolecatImageTrimEnvRejectsAnInvalidContext(t *testing.T) {
	t.Parallel()
	env, err := PolecatImageTrimEnv(trimEnvRunner(true, agentconfig.TierSweepRunnerDocker, "-rf"))
	if err == nil {
		t.Fatal("PolecatImageTrimEnv() error = nil, want a refusal")
	}
	if env != nil {
		t.Errorf("PolecatImageTrimEnv() = %q, want nil", env)
	}
	if !strings.Contains(err.Error(), `"-rf"`) {
		t.Errorf("error = %q, want it to name the refused context", err)
	}
}

// TestUnsafeTrimContextIsRefusedAndLogged is the alarm for the input guard: a
// town whose runner context is not one Docker context name publishes nothing to
// the plugin, and the log names the value it refused so the typo is findable
// (gt-bxp17).
func TestUnsafeTrimContextIsRefusedAndLogged(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := &Daemon{
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
			Enabled: true,
			Runner:  &agentconfig.TierSweepRunnerConfig{Backend: agentconfig.TierSweepRunnerDocker, Context: "-rf"},
		}}},
		logger: log.New(&logged, "", 0),
	}
	env := d.withPolecatImageTrimContext([]string{"PATH=/usr/bin"})
	if got := (cliCall{env: env}).getenv(polecatImageTrimContextEnv); got != "" {
		t.Errorf("%s = %q, want nothing published for an unsafe name", polecatImageTrimContextEnv, got)
	}
	want := `tier_sweep runner context "-rf" is not one Docker context name; the polecat-image plugin will not trim it`
	if got := logged.String(); !strings.Contains(got, want) {
		t.Errorf("log = %q, want it to name the refused value: %q", got, want)
	}
}

func TestDaemonGTEnvReplacesTheIdentity(t *testing.T) {
	t.Parallel()
	got := daemonGTEnv(pollutedEnv())
	assertDaemonIdentity(t, "daemonGTEnv", got)
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Errorf("daemonGTEnv dropped a non-identity variable: %q", got)
	}
	assertDaemonIdentity(t, "daemonGTEnv(nil)", daemonGTEnv(nil))
}

// TestDaemonGTExecSitesCarryTheDaemonIdentity drives each gt the daemon runs
// through the execCmd seam and checks that every one names the daemon as its
// actor, so the usage log and what gt writes never read "unknown" or the
// launching agent (gt-kyik6).
func TestDaemonGTExecSitesCarryTheDaemonIdentity(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(nil)
	townRoot := t.TempDir()
	d := &Daemon{
		ctx:     t.Context(),
		config:  &Config{TownRoot: townRoot},
		logger:  log.New(io.Discard, "", 0),
		gtPath:  "gt",
		execCmd: gt.run,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true},
		}},
	}

	// Scheduled dispatch forks nothing since gt-638go.9; it runs the scheduler
	// in process, so it contributes no gt call here.
	d.dispatchQueuedWork()
	_, _ = d.runSpecDispatchCommand(time.Minute)
	_ = d.restartPolecatSession(supervisor.SeatFor("gastown", constants.RolePolecat, "ruby"))
	r := &execScheduledSlingRunner{townRoot: townRoot, bdPath: "bd", gtPath: "gt", execCmd: gt.run}
	_ = r.sling(t.Context(), "gt-run1", "", docAuditEntry)

	// The convoy feeder's dispatch is the one gt the daemon used to run that no
	// longer runs at all: it goes through the in-process engine (gt-638go.7),
	// and the bd and tmux calls that engine makes inherit this process's
	// environment — which PublishIdentity already gives the daemon's identity.
	calls := gt.recorded()
	want := []string{"spec dispatch", "session restart", "sling gt-run1"}
	if len(calls) != len(want) {
		t.Fatalf("recorded %d gt calls, want %d: %+v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		argv := strings.Join(c.args, " ")
		if !strings.HasPrefix(argv, want[i]) {
			t.Errorf("call %d = gt %s, want gt %s...", i, argv, want[i])
		}
		assertDaemonIdentity(t, "gt "+argv, c.env)
	}
}

func TestDaemonNotifiersRunGtAsTheDaemon(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: "/town"}, gtPath: "gt"}
	m := &DoltServerManager{townRoot: "/town"}
	for name, n := range map[string]notify.Notifier{"daemon": d.notify(), "dolt server": m.notify()} {
		cli, ok := n.(*notify.CLI)
		if !ok || cli.Env == nil {
			t.Fatalf("%s notifier = %+v, want a notify.CLI with an explicit env", name, n)
		}
		assertDaemonIdentity(t, name+" notifier", cli.Env())
		// Nudges are delivered in-process, sent from the town root as the
		// daemon (gt-22hdp.16).
		nudger, ok := cli.Nudger.(*notify.TownNudger)
		if !ok || nudger.Dir != "/town" || nudger.Env == nil {
			t.Fatalf("%s notifier nudger = %+v, want an in-process TownNudger from /town with an explicit env", name, cli.Nudger)
		}
		assertDaemonIdentity(t, name+" nudger", nudger.Env())
	}
}

func TestPluginScriptRunsAsTheDaemonPlugin(t *testing.T) {
	t.Parallel()
	bash := newFakeCLI(nil)
	p := &plugin.Plugin{Name: "tool-updater", Path: "/town/plugins/tool-updater"}
	runPluginScript(context.Background(), bash.run, scriptEnv{environ: pollutedEnv()}, p, "/town", time.Second)
	calls := bash.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want the one script run", len(calls))
	}
	assertDaemonIdentity(t, "plugin script", calls[0].env, "GT_ROLE=daemon/plugin")
	if got := calls[0].getenv("GT_ROLE"); got != "daemon/plugin" {
		t.Errorf("GT_ROLE = %q, want daemon/plugin", got)
	}
}

// doltEndpointTown is a town whose Dolt server answers to the host's loopback
// and to the name the daemon's container reaches it by.
func doltEndpointTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town","version":2,"name":"t","dolt":{"host":"127.0.0.1","container_host":"host.docker.internal","port":4407}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// In container mode a plugin's bd has no endpoint of its own; unset, it asks
// 127.0.0.1 and reaches the container instead of the town's server. The
// runner gives every plugin child the endpoint the daemon's own bd calls use
// (gt-8txy8).
func TestPluginEnvDoltEndpoint(t *testing.T) {
	t.Parallel()
	townRoot := doltEndpointTown(t)
	p := scriptPlugin(t, "town-metrics", "exit 0\n")
	bash := scriptBash(cliReply{})
	res := runPluginScript(context.Background(), bash.run,
		scriptEnv{environ: []string{"PATH=/usr/bin"}, inContainer: true}, p, townRoot, 5*time.Second)
	if !res.ok() {
		t.Fatalf("expected success, got %+v", res)
	}
	calls := bash.recorded()
	if len(calls) != 1 {
		t.Fatalf("ran %d processes, want 1", len(calls))
	}
	for key, want := range map[string]string{
		"BEADS_DOLT_SERVER_HOST": "host.docker.internal",
		"BEADS_DOLT_SERVER_PORT": "4407",
	} {
		if got := calls[0].getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// An endpoint the daemon environment already names wins over the town's whole,
// and outside container mode the environment is the daemon's own, unchanged
// (gt-8txy8).
func TestPluginEnvDoltEndpointExisting(t *testing.T) {
	t.Parallel()
	townRoot := doltEndpointTown(t)
	p := scriptPlugin(t, "kept", "exit 0\n")

	t.Run("a daemon endpoint wins whole", func(t *testing.T) {
		environ := []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_HOST=stale.example", "BEADS_DOLT_SERVER_PORT=9999"}
		bash := scriptBash(cliReply{})
		res := runPluginScript(context.Background(), bash.run,
			scriptEnv{environ: environ, inContainer: true}, p, townRoot, 5*time.Second)
		if !res.ok() {
			t.Fatalf("expected success, got %+v", res)
		}
		calls := bash.recorded()
		if len(calls) != 1 {
			t.Fatalf("ran %d processes, want 1", len(calls))
		}
		if got := calls[0].getenv("BEADS_DOLT_SERVER_HOST"); got != "stale.example" {
			t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want the daemon's value", got)
		}
		if got := calls[0].getenv("BEADS_DOLT_SERVER_PORT"); got != "9999" {
			t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want the daemon's port", got)
		}
		if got := calls[0].getenv("BEADS_DOLT_PORT"); got != "" {
			t.Errorf("BEADS_DOLT_PORT = %q, want the town's port not mixed into the daemon's endpoint", got)
		}
	})

	t.Run("a partial daemon endpoint is refused, not split", func(t *testing.T) {
		environ := []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_HOST=stale.example"}
		bash := scriptBash(cliReply{})
		res := runPluginScript(context.Background(), bash.run,
			scriptEnv{environ: environ, inContainer: true}, p, townRoot, 5*time.Second)
		if res.ok() {
			t.Fatalf("a host with no port must not run against the town's port: %+v", res)
		}
		if n := len(bash.recorded()); n != 0 {
			t.Fatalf("%d processes ran, want none", n)
		}
		if !strings.Contains(res.status(), "BEADS_DOLT_SERVER_PORT") {
			t.Errorf("refusal does not name the missing port: %s", res.status())
		}
	})

	t.Run("outside container mode the child env is unchanged", func(t *testing.T) {
		environ := []string{"PATH=/usr/bin", "HOME=/home/daemon"}
		bash := scriptBash(cliReply{})
		res := runPluginScript(context.Background(), bash.run,
			scriptEnv{environ: environ}, p, townRoot, 5*time.Second)
		if !res.ok() {
			t.Fatalf("expected success, got %+v", res)
		}
		calls := bash.recorded()
		if len(calls) != 1 {
			t.Fatalf("ran %d processes, want 1", len(calls))
		}
		want := append(daemonGTEnv(environ),
			"GT_ROOT="+townRoot,
			"GT_TOWN_ROOT="+townRoot,
			"GT_PLUGIN_NAME="+p.Name,
			"GT_PLUGIN_RUNNER=daemon",
			"GT_ROLE=daemon/plugin",
		)
		if got := calls[0].env; !slices.Equal(got, want) {
			t.Errorf("child env = %q, want %q", got, want)
		}
	})
}

// A town with no endpoint and a daemon environment that sets neither variable
// cannot give bd a server: the run is refused before the process starts, and
// the receipt names what is missing instead of reporting a green run
// (gt-8txy8).
func TestPluginEnvDoltEndpointUnresolved(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "no-endpoint", "exit 0\n")
	bash := scriptBash(cliReply{})
	res := runPluginScript(context.Background(), bash.run,
		scriptEnv{environ: []string{"PATH=/usr/bin"}, inContainer: true}, p, t.TempDir(), 5*time.Second)
	if res.ok() {
		t.Fatalf("a plugin with no Dolt endpoint must not run: %+v", res)
	}
	if n := len(bash.recorded()); n != 0 {
		t.Fatalf("%d processes ran, want none", n)
	}
	var rec plugin.PluginRunRecord
	completeScriptRun(p, res, scriptRunHooks{
		record: func(r plugin.PluginRunRecord) error { rec = r; return nil },
		logf:   t.Logf,
	})
	if rec.Result != plugin.ResultFailure {
		t.Errorf("receipt result = %q, want a failure", rec.Result)
	}
	for _, key := range []string{"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if !strings.Contains(rec.Body, key) {
			t.Errorf("receipt body does not name %s:\n%s", key, rec.Body)
		}
	}
}
