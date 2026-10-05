package townstatus

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
)

func TestDiscoverRigAgents_UsesRigPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	})

	r := &rig.Rig{
		Name: "beads",
		Path: filepath.Join(townRoot, "beads"),
	}

	allAgentBeads := map[string]*beads.Issue{
		"bd-beads-crew-max": {
			ID:         "bd-beads-crew-max",
			AgentState: "running",
			HookBead:   "bd-hook",
		},
	}
	allHookBeads := map[string]*beads.Issue{
		"bd-hook": {ID: "bd-hook", Title: "Pinned"},
	}

	agents := discoverRigAgents(testRegistry(), map[string]bool{}, r, []string{"max"}, allAgentBeads, allHookBeads, nil, true)
	if len(agents) != 1 {
		t.Fatalf("discoverRigAgents() returned %d agents, want 1", len(agents))
	}

	if agents[0].State != "running" {
		t.Fatalf("agent state = %q, want %q", agents[0].State, "running")
	}
	if !agents[0].HasWork {
		t.Fatalf("agent HasWork = false, want true")
	}
	if agents[0].WorkTitle != "Pinned" {
		t.Fatalf("agent WorkTitle = %q, want %q", agents[0].WorkTitle, "Pinned")
	}
}

func TestDiscoverRigAgents_ZombieSessionNotRunning(t *testing.T) {
	t.Parallel()
	// Verify that a session in allSessions with value=false (zombie: tmux alive,
	// agent dead) results in agent.Running=false. This is the core fix for gt-bd6i3.
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	})

	r := &rig.Rig{
		Name: "gastown",
		Path: filepath.Join(townRoot, "gastown"),
	}

	// allSessions has the crew session but marked as zombie (false).
	// This simulates a tmux session that exists but whose agent process has died.
	allSessions := map[string]bool{
		session.CrewSessionName(testRegistry().PrefixForRig("gastown"), "max"): false, // zombie: tmux exists, agent dead
	}

	agents := discoverRigAgents(testRegistry(), allSessions, r, []string{"max"}, nil, nil, nil, true)
	for _, a := range agents {
		if a.Role == "crew" {
			if a.Running {
				t.Fatal("zombie crew session (allSessions=false) should show as not running")
			}
			return
		}
	}
	t.Fatal("crew agent not found in results")
}

func TestDiscoverRigAgents_MissingSessionNotRunning(t *testing.T) {
	t.Parallel()
	// Verify that a session not in allSessions at all results in agent.Running=false.
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	})

	r := &rig.Rig{
		Name: "gastown",
		Path: filepath.Join(townRoot, "gastown"),
	}

	// Empty sessions map - no tmux sessions exist at all
	allSessions := map[string]bool{}

	agents := discoverRigAgents(testRegistry(), allSessions, r, []string{"max"}, nil, nil, nil, true)
	for _, a := range agents {
		if a.Role == "crew" {
			if a.Running {
				t.Fatal("crew with no tmux session should show as not running")
			}
			return
		}
	}
	t.Fatal("crew agent not found in results")
}

// TestReadGateSlotHolderSkipsDockerProbe pins gt-a8kx at its regression site.
// gateSlotHolder feeds the status line from every `gt status` and every
// --watch tick and carries nothing but the holder, so it must stay a flock
// read. StatusPoolLocksOnly's own test cannot catch a revert of just this call
// site, and neither can a check made while a slot is HELD — StatusPool skips
// its cross-check then too, because a holder's containers are not "unwrapped"
// ones. Only the idle gate separates the two, and the idle gate is the common
// case this bug was about. The call site reads through
// slot.StatusPoolLocksOnly, whose TestStatusPoolLocksOnlySkipsDockerProbe pins
// that it never lists containers; this test pins the reading itself.
func TestReadGateSlotHolderSkipsDockerProbe(t *testing.T) {
	t.Parallel()
	stubNoContainers(t)
	t.Run("idle gate", func(t *testing.T) {
		townRoot := t.TempDir()
		if got := gateSlotHolder(townRoot); got != nil {
			t.Fatalf("gateSlotHolder = %+v, want nil with no slot held", got)
		}
	})

	t.Run("held slot", func(t *testing.T) {
		townRoot := t.TempDir()

		handle, err := slot.Acquire(townRoot, "gastown/refinery", time.Second)
		if err != nil {
			t.Fatalf("slot.Acquire: %v", err)
		}
		t.Cleanup(func() { _ = handle.Release() })

		got := gateSlotHolder(townRoot)
		if got == nil {
			t.Fatal("gateSlotHolder = nil, want the refinery's hold")
		}
		if got.Role != "gastown/refinery" || got.PID != os.Getpid() {
			t.Fatalf("gateSlotHolder = %+v, want role gastown/refinery at pid %d", got, os.Getpid())
		}
	})
}

func TestTryStatusDetailLockContention(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	release, ok := tryDetailLock(townRoot)
	if !ok {
		t.Fatal("first status detail lock should be acquired")
	}

	if release2, ok := tryDetailLock(townRoot); ok {
		release2()
		t.Fatal("second status detail lock should fail while first is held")
	}

	release()

	release3, ok := tryDetailLock(townRoot)
	if !ok {
		t.Fatal("status detail lock should be reusable after release")
	}
	release3()
}

func TestIsKnownAgent(t *testing.T) {
	t.Parallel()

	// All agent presets should be recognized
	for _, name := range config.ListAgentPresets() {
		t.Run(name+"_known", func(t *testing.T) {
			if !isKnownAgent(name) {
				t.Errorf("isKnownAgent(%q) = false, want true", name)
			}
		})
	}

	// Non-agents should not be recognized
	for _, name := range []string{"bash", "node", ""} {
		t.Run(name+"_unknown", func(t *testing.T) {
			if isKnownAgent(name) {
				t.Errorf("isKnownAgent(%q) = true, want false", name)
			}
		})
	}
}

func TestIsAgentWrapper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base string
		want bool
	}{
		{"node", true},
		{"bun", true},
		{"npx", true},
		{"bunx", true},
		{"claude", false},
		{"pi", false},
		{"bash", false},
	}

	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			if got := isAgentWrapper(tt.base); got != tt.want {
				t.Errorf("isAgentWrapper(%q) = %v, want %v", tt.base, got, tt.want)
			}
		})
	}
}

func TestParseRuntimeInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{
			name:    "claude with model",
			cmdline: "claude\x00--model\x00opus\x00--dangerously-skip-permissions",
			want:    "claude/opus",
		},
		{
			name:    "pi with model",
			cmdline: "pi\x00-e\x00gastown-hooks.js\x00--model\x00google-antigravity/gemini-3-flash",
			want:    "pi/google-antigravity/gemini-3-flash",
		},
		{
			name:    "cgroup-wrap then claude",
			cmdline: "cgroup-wrap\x00claude\x00--model\x00opus\x00--dangerously-skip-permissions",
			want:    "claude/opus",
		},
		{
			name:    "opencode with -m flag",
			cmdline: "opencode\x00-m\x00kimi-for-coding/kimi-k2.5",
			want:    "opencode/kimi-for-coding/kimi-k2.5",
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRuntimeInfo(tt.cmdline)
			if got != tt.want {
				t.Errorf("parseRuntimeInfo(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestBuildInfoFromConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rc   *config.RuntimeConfig
		want string
	}{
		{
			name: "claude with model",
			rc:   &config.RuntimeConfig{Command: "claude", Args: []string{"--model", "opus"}},
			want: "claude/opus",
		},
		{
			name: "cgroup-wrap claude",
			rc:   &config.RuntimeConfig{Command: "cgroup-wrap", Args: []string{"claude", "--model", "opus"}},
			want: "claude/opus",
		},
		{
			name: "pi bare",
			rc:   &config.RuntimeConfig{Command: "pi", Args: []string{"-e", "hooks.js"}},
			want: "pi",
		},
		{
			name: "opencode with -m",
			rc:   &config.RuntimeConfig{Command: "opencode", Args: []string{"-m", "gpt-5"}},
			want: "opencode/gpt-5",
		},
		{
			name: "empty command",
			rc:   &config.RuntimeConfig{Command: ""},
			want: "claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildInfoFromConfig(tt.rc)
			if got != tt.want {
				t.Errorf("buildInfoFromConfig(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsAgentCmdline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"claude direct", "claude\x00--model\x00opus", true},
		{"node wrapper with claude", "node\x00/path/to/claude\x00--model\x00opus", true},
		{"retired pi runtime", "pi\x00-e\x00hooks.js", false},
		{"bash not agent", "bash\x00-c\x00echo hi", false},
		{"node without agent", "node\x00/path/to/server.js", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentCmdline(tt.cmdline)
			if got != tt.want {
				t.Errorf("isAgentCmdline(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestCountRunningAgents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status TownStatus
		want   int
	}{
		{
			name:   "empty status",
			status: TownStatus{},
			want:   0,
		},
		{
			name: "town-level agents only",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "deacon", Running: true},
					{Name: "dog", Running: false},
				},
			},
			want: 1,
		},
		{
			name: "rig agents only",
			status: TownStatus{
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: true},
							{Name: "witness", Running: true},
						},
					},
				},
			},
			want: 2,
		},
		{
			name: "mixed town-level and rig agents",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "deacon", Running: true},
				},
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: true},
							{Name: "witness", Running: false},
						},
					},
					{
						Agents: []AgentRuntime{
							{Name: "polecat-2", Running: true},
						},
					},
				},
			},
			want: 3,
		},
		{
			name: "all not running",
			status: TownStatus{
				Agents: []AgentRuntime{
					{Name: "deacon", Running: false},
				},
				Rigs: []RigStatus{
					{
						Agents: []AgentRuntime{
							{Name: "polecat-1", Running: false},
						},
					},
				},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountRunningAgents(tt.status)
			if got != tt.want {
				t.Errorf(
					"CountRunningAgents() = %d, want %d",
					got, tt.want,
				)
			}
		})
	}
}

func TestExtractBaseName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cmdline string
		want    string
	}{
		{"claude\x00--model\x00opus", "claude"},
		{"/usr/bin/node\x00/path/pi", "node"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := extractBaseName(tt.cmdline)
			if got != tt.want {
				t.Errorf("extractBaseName(%q) = %q, want %q", tt.cmdline, got, tt.want)
			}
		})
	}
}

// TestAgentMarkerTriple covers the address → marker-coordinate mapping gt
// status uses to read pause markers. Every marker-backed agent is rig-level:
// the town-level roles the mapping was extended for (mayor, gt-wisp-6ajo;
// deacon, gt-4k3fj.6.1) have all retired, so a bare role no longer resolves to
// a marker at all.
func TestAgentMarkerTriple(t *testing.T) {
	t.Parallel()
	tests := []struct {
		address         string
		rig, role, name string
		ok              bool
	}{
		{"gastown/flint", "gastown", constants.RolePolecat, "flint", true},
		{"gastown/polecats/flint", "gastown", constants.RolePolecat, "flint", true},
		{"gastown/witness", "gastown", constants.RolePolecat, "witness", true}, // witness role retired
		{"gastown/crew/opal", "gastown", constants.RoleCrew, "opal", true},
		// Not addressable agents: no marker, no reason to look.
		{"overseer", "", "", "", false},
		{"mayor/", "", "", "", false},  // mayor role retired (gt-rwp7z)
		{"deacon/", "", "", "", false}, // deacon role retired (gt-4k3fj.6.1)
		{"", "", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			rig, role, name, ok := MarkerTriple(tt.address)
			if ok != tt.ok || rig != tt.rig || role != tt.role || name != tt.name {
				t.Errorf("MarkerTriple(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tt.address, rig, role, name, ok, tt.rig, tt.role, tt.name, tt.ok)
			}
		})
	}
}

// TestApplyPauseMarkerNamesAgent is the end-to-end check for the status line:
// pause an agent the way `gt agent pause` does, then confirm the address gt
// status uses resolves to the same marker and carries the reason.
func TestApplyPauseMarkerNamesAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := agentpause.Pause(townRoot, "gastown", "polecat", "flint", "filesystem scan", "human", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	agent := AgentRuntime{Address: "gastown/flint"}
	applyPauseMarker(&agent, townRoot)
	if agent.PausedReason != "filesystem scan" {
		t.Errorf("rig agent PausedReason = %q, want %q", agent.PausedReason, "filesystem scan")
	}

	// Town-level roles have retired, so the status address "mayor/" no longer
	// names an agent: a stale marker under the retired role must not resurface
	// as a pause on the status line (gt-rwp7z).
	if err := agentpause.Pause(townRoot, "", "mayor", "", "operator hold", "human", ""); err != nil {
		t.Fatalf("Pause retired mayor: %v", err)
	}
	retired := AgentRuntime{Address: "mayor/"}
	applyPauseMarker(&retired, townRoot)
	if retired.PausedReason != "" {
		t.Errorf("retired mayor PausedReason = %q, want empty (town-level role retired)", retired.PausedReason)
	}

	// An unpaused agent must not pick up a reason from anywhere.
	idle := AgentRuntime{Address: "gastown/witness"}
	applyPauseMarker(&idle, townRoot)
	if idle.PausedReason != "" {
		t.Errorf("unpaused witness PausedReason = %q, want empty", idle.PausedReason)
	}
}
