package daemon

import (
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

	d.dispatchQueuedWork()
	_, _ = d.runSpecDispatchCommand()
	_, _ = d.readDispatchCheck()
	_ = d.restartPolecatSession(supervisor.SeatFor("gastown", constants.RolePolecat, "ruby"))
	r := &execScheduledSlingRunner{townRoot: townRoot, bdPath: "bd", gtPath: "gt", execCmd: gt.run}
	_ = r.sling(t.Context(), "gt-run1", docAuditEntry)

	// The convoy feeder's dispatch is the one gt the daemon used to run that no
	// longer runs at all: it goes through the in-process engine (gt-638go.7),
	// and the bd and tmux calls that engine makes inherit this process's
	// environment — which PublishIdentity already gives the daemon's identity.
	calls := gt.recorded()
	want := []string{"scheduler run", "spec dispatch", "daemon dispatch-check", "session restart", "sling gt-run1"}
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
	p := &plugin.Plugin{Name: "rebuild-gt", Path: "/town/plugins/rebuild-gt"}
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
