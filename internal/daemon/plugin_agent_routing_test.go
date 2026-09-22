package daemon

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/plugin"
)

// agentCapturingSM records the startup options the dispatcher passes, so a
// test can see the agent preset the dog session will run.
type agentCapturingSM struct {
	opts []dog.SessionStartOptions
}

func (s *agentCapturingSM) Start(_ string, opts dog.SessionStartOptions) error {
	s.opts = append(s.opts, opts)
	return nil
}

func newDispatchTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		ctx:    context.Background(),
	}
	d.findDogFn = func() *dog.Dog { return &dog.Dog{Name: "alpha"} }
	return d
}

// A plugin that names an agent routes its dog session to that preset; a plugin
// without the key leaves the override empty so the session keeps resolving
// role_agents.dog.
func TestDispatchPluginToDog_PluginAgentReachesSessionOptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
	}{
		{"named preset overrides the dog role", "claude-sonnet"},
		{"no agent key keeps role_agents.dog", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatchTestDaemon(t)
			sm := &agentCapturingSM{}
			p := &plugin.Plugin{Name: "github-sheriff", Agent: tc.agent}

			got, noDog := d.dispatchPluginToDog(p, &fakeMgr{}, sm, &fakeRouter{}, p.FormatMailBody())
			if noDog {
				t.Fatal("dispatch deferred: no dispatchable dog")
			}
			if got != "alpha" {
				t.Fatalf("dispatched to %q, want %q", got, "alpha")
			}
			if len(sm.opts) != 1 {
				t.Fatalf("session starts = %d, want 1", len(sm.opts))
			}
			if sm.opts[0].AgentOverride != tc.agent {
				t.Errorf("AgentOverride = %q, want %q", sm.opts[0].AgentOverride, tc.agent)
			}
			if sm.opts[0].WorkDesc != "plugin:github-sheriff" {
				t.Errorf("WorkDesc = %q, want the plugin work description", sm.opts[0].WorkDesc)
			}
		})
	}
}

// A script plugin the daemon ran itself and that failed is handed to a dog;
// that dog must run the plugin's preset too.
func TestScriptPluginFailureHandoff_KeepsPluginAgent(t *testing.T) {
	d := newDispatchTestDaemon(t)
	mgr, sm, router, rec := &fakeMgr{}, &agentCapturingSM{}, &fakeRouter{}, &fakeRecorder{}

	p := scriptPlugin(t, "handoff", "exit 7\n")
	p.Agent = "claude-opus"

	d.startScriptPlugin(p, mgr, sm, router, rec)

	waitFor(t, func() bool { return len(sm.opts) == 1 })
	if sm.opts[0].AgentOverride != "claude-opus" {
		t.Errorf("handoff AgentOverride = %q, want %q", sm.opts[0].AgentOverride, "claude-opus")
	}
	if len(router.sent) != 1 {
		t.Errorf("handoff mail sent = %d, want 1", len(router.sent))
	}
}
