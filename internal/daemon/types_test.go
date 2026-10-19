package daemon

import (
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// decodePatrols decodes daemon.json text the way the daemon loads it.
func decodePatrols(t *testing.T, text string) *DaemonPatrolConfig {
	t.Helper()
	var cfg DaemonPatrolConfig
	if err := agentconfig.DecodeJSONFile("daemon.json", []byte(text), &cfg); err != nil {
		t.Fatalf("decoding %s: %v", text, err)
	}
	return &cfg
}

// A hand-written spec_dispatch block that tunes the dispatcher but omits
// "enabled" leaves it on: the field is documented as on unless disabled.
func TestIsPatrolEnabledSpecDispatchOmittedEnabled(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`{"patrols":{"spec_dispatch":{"interval":"30s"}}}`,
		`{"patrols":{"spec_dispatch":{"max_per_tick":2}}}`,
		`{"patrols":{"spec_dispatch":{}}}`,
	} {
		if !IsPatrolEnabled(decodePatrols(t, text), "spec_dispatch") {
			t.Errorf("%s: spec_dispatch off, want on", text)
		}
	}
}

func TestIsPatrolEnabledSpecDispatchExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg := decodePatrols(t, `{"patrols":{"spec_dispatch":{"enabled":false}}}`)
	if IsPatrolEnabled(cfg, "spec_dispatch") {
		t.Error(`"enabled": false: spec_dispatch on, want off`)
	}
	cfg = decodePatrols(t, `{"patrols":{"spec_dispatch":{"enabled":true,"interval":"30s"}}}`)
	if !IsPatrolEnabled(cfg, "spec_dispatch") {
		t.Error(`"enabled": true: spec_dispatch off, want on`)
	}
}
