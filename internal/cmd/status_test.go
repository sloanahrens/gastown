package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/townstatus"
)

func TestRenderAgentDetails_UsesRigPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	})

	agent := townstatus.AgentRuntime{
		Name:    "max",
		Address: "beads/crew/max",
		Role:    "crew",
		Running: true,
	}

	var buf bytes.Buffer
	renderAgentDetails(&buf, agent, "", nil, townRoot)
	output := buf.String()

	if !strings.Contains(output, "bd-beads-crew-max") {
		t.Fatalf("output %q does not contain rig-prefixed bead ID", output)
	}
}

func TestBuildStatusIndicator_ZombieShowsStopped(t *testing.T) {
	t.Parallel()
	// Verify that a zombie agent (Running=false) shows ○ (stopped), not ● (running)
	agent := townstatus.AgentRuntime{Running: false}
	indicator := buildStatusIndicator(agent)
	if strings.Contains(indicator, "●") {
		t.Fatal("zombie agent (Running=false) should not show ● indicator")
	}
}

func TestBuildStatusIndicator_AliveShowsRunning(t *testing.T) {
	t.Parallel()
	// Verify that an alive agent (Running=true) shows ● (running)
	agent := townstatus.AgentRuntime{Running: true}
	indicator := buildStatusIndicator(agent)
	if strings.Contains(indicator, "○") {
		t.Fatal("alive agent (Running=true) should not show ○ indicator")
	}
}

func TestBuildStatusIndicator_DNDMutedShowsBadge(t *testing.T) {
	t.Parallel()
	agent := townstatus.AgentRuntime{Running: true, NotificationLevel: beads.NotifyMuted}
	indicator := buildStatusIndicator(agent)
	if !strings.Contains(indicator, "🔕") {
		t.Fatalf("expected muted indicator to include 🔕, got %q", indicator)
	}
}

func TestOutputStatusText_IncludesDNDSection(t *testing.T) {
	t.Parallel()
	status := townstatus.TownStatus{
		Name:     "gt",
		Location: "/tmp/gt",
		DND: &townstatus.DNDInfo{
			Enabled: true,
			Level:   beads.NotifyMuted,
			Agent:   "hq-deacon",
		},
	}

	var buf bytes.Buffer
	if err := outputStatusText(&buf, status); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "DND:") {
		t.Fatalf("expected DND section in status output, got: %q", out)
	}
	if !strings.Contains(out, "on") {
		t.Fatalf("expected DND state 'on' in status output, got: %q", out)
	}
}

func TestOutputStatusText_ContainerSlot(t *testing.T) {
	t.Parallel()
	held := townstatus.TownStatus{
		Name:     "gt",
		Location: "/tmp/gt",
		Slot: &townstatus.SlotInfo{
			Role:       "gastown/refinery",
			PID:        4242,
			AcquiredAt: time.Now().Add(-90 * time.Second),
		},
	}

	var buf bytes.Buffer
	if err := outputStatusText(&buf, held); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Container suite running:") {
		t.Fatalf("expected container-slot line when Slot is set, got: %q", out)
	}
	if !strings.Contains(out, "gastown/refinery") {
		t.Fatalf("expected holder role in status output, got: %q", out)
	}

	// Free slot: the line must NOT appear — a status render that always
	// prints it (e.g. from a nil-Slot zero value) would be indistinguishable
	// from "always holding the slot" to a reader.
	free := townstatus.TownStatus{Name: "gt", Location: "/tmp/gt"}
	buf.Reset()
	if err := outputStatusText(&buf, free); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	if strings.Contains(buf.String(), "Container suite running:") {
		t.Fatalf("did not expect container-slot line when Slot is nil, got: %q", buf.String())
	}
}

func TestValidateStatusWatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		jsonOut  bool
		interval int
		wantErr  string
	}{
		{"zero interval", false, 0, "positive"},
		{"negative interval", false, -5, "positive"},
		{"json combo", true, 2, "cannot be used together"},
		{"valid", false, 2, ""},
	}
	for _, tt := range tests {
		err := validateStatusWatch(tt.jsonOut, tt.interval)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error %v, want one mentioning %q", tt.name, err, tt.wantErr)
		}
	}
}

// TestBeadStatePausedStillShows pins the decision on gt-ahik/om-kgx0:
// `gt deacon pause` is a separate, existing feature that writes
// agent_state=paused straight to the deacon bead with no agentpause marker
// file, so agent.Paused (the marker-driven field) never covers it. Both
// status renderers must keep showing it via the bead-state switch, or a
// deacon paused that way silently disappears from `gt status`.
func TestBeadStatePausedStillShows(t *testing.T) {
	t.Parallel()
	agent := townstatus.AgentRuntime{Address: "deacon/", State: "paused"}

	if indicator := buildStatusIndicator(agent); !strings.Contains(indicator, "paused") {
		t.Errorf("buildStatusIndicator(%+v) = %q, want it to mention paused", agent, indicator)
	}

	var buf bytes.Buffer
	renderAgentDetails(&buf, agent, "", nil, t.TempDir())
	if out := buf.String(); !strings.Contains(out, "[paused]") {
		t.Errorf("renderAgentDetails output = %q, want it to contain [paused]", out)
	}
}

// gt-fcxe9.1: a session whose liveness query failed is shown as running (it
// is not known dead) and named on its own line, so the failure is visible.
func TestOutputStatusText_LivenessUnknown(t *testing.T) {
	t.Parallel()
	st := townstatus.TownStatus{Name: "gt", Location: "/tmp/gt", LivenessUnknown: []string{"gt-gastown-witness"}}
	var buf bytes.Buffer
	if err := outputStatusText(&buf, st); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Agent liveness unknown") || !strings.Contains(out, "gt-gastown-witness") {
		t.Fatalf("expected an unknown-liveness line naming the session, got: %q", out)
	}
	buf.Reset()
	if err := outputStatusText(&buf, townstatus.TownStatus{Name: "gt", Location: "/tmp/gt"}); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	if strings.Contains(buf.String(), "Agent liveness unknown") {
		t.Fatalf("unknown-liveness line printed with nothing unknown: %q", buf.String())
	}
}

// The commits-per-day meter marks the Dolt part of the Services line only for
// databases over the limit, busiest first.
func TestOutputStatusText_DoltCommitMarker(t *testing.T) {
	t.Parallel()

	dolt := &townstatus.DoltInfo{Running: true, PID: 7, Port: 3307, CommitsPerDayWarn: 500, CommitsLastDay: []doltserver.DBCommits{
		{Database: "be", Commits: 17},
		{Database: "gt", Commits: 3048},
		{Database: "hq", Commits: 2041},
	}}
	var buf bytes.Buffer
	if err := outputStatusText(&buf, townstatus.TownStatus{Name: "gt", Location: "/tmp/gt", Dolt: dolt}); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "⚠ commits/24h gt=3048 hq=2041 > 500") || strings.Contains(out, "be=17") {
		t.Fatalf("want the over-limit marker for gt and hq only, got: %q", out)
	}

	under := &townstatus.DoltInfo{Running: true, PID: 7, Port: 3307, CommitsPerDayWarn: 500, CommitsLastDay: []doltserver.DBCommits{
		{Database: "gt", Commits: 120},
	}}
	buf.Reset()
	if err := outputStatusText(&buf, townstatus.TownStatus{Name: "gt", Location: "/tmp/gt", Dolt: under}); err != nil {
		t.Fatalf("outputStatusText error: %v", err)
	}
	if strings.Contains(buf.String(), "commits/24h") {
		t.Fatalf("no marker expected under the limit, got: %q", buf.String())
	}
}
