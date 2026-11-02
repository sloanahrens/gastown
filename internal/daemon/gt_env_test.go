package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
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
