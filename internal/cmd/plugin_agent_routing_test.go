package cmd

import (
	"encoding/json"
	"testing"

	"github.com/steveyegge/gastown/internal/plugin"
)

// gt plugin list --json is how an operator checks which preset a plugin routes
// to, so the field has to survive the summary marshalling.
func TestOutputPluginListJSON_IncludesAgent(t *testing.T) {
	plugins := []*plugin.Plugin{
		{Name: "judgment", Agent: "claude-sonnet", Location: plugin.LocationTown, Path: "/p"},
		{Name: "script-runner", Location: plugin.LocationTown, Path: "/q"},
	}

	var summaries []plugin.PluginSummary
	if err := json.Unmarshal([]byte(captureOutput(func() {
		if err := outputPluginListJSON(plugins); err != nil {
			t.Errorf("outputPluginListJSON: %v", err)
		}
	})), &summaries); err != nil {
		t.Fatalf("unmarshalling plugin list output: %v", err)
	}

	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2", len(summaries))
	}
	if summaries[0].Agent != "claude-sonnet" {
		t.Errorf("agent = %q, want %q", summaries[0].Agent, "claude-sonnet")
	}
	if summaries[1].Agent != "" {
		t.Errorf("agent = %q for a plugin without the key, want empty", summaries[1].Agent)
	}
}
