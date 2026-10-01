package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockSessionLister allows deterministic testing of orphan session detection.
type mockSessionLister struct {
	sessions []string
	err      error
}

func (m *mockSessionLister) ListSessions() ([]string, error) {
	return m.sessions, m.err
}

func TestNewOrphanSessionCheck(t *testing.T) {
	t.Parallel()
	check := NewOrphanSessionCheck()

	if check.Name() != "orphan-sessions" {
		t.Errorf("expected name 'orphan-sessions', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true for session check")
	}
}

func TestNewOrphanProcessCheck(t *testing.T) {
	t.Parallel()
	check := NewOrphanProcessCheck()

	if check.Name() != "orphan-processes" {
		t.Errorf("expected name 'orphan-processes', got %q", check.Name())
	}

	// OrphanProcessCheck should NOT be fixable - it's informational only
	if check.CanFix() {
		t.Error("expected CanFix to return false for process check (informational only)")
	}
}

// cannedProcesses is a process table: tmux's pids, ps -eo pid,ppid,args
// output and each pid's parent.
type cannedProcesses struct {
	tmux    map[int]bool
	table   string
	parents map[int]int
}

func (p cannedProcesses) tmuxPIDs() (map[int]bool, error) { return p.tmux, nil }
func (p cannedProcesses) processTable() ([]byte, error)   { return []byte(p.table), nil }
func (p cannedProcesses) parentPID(pid int) (int, error) {
	if ppid, ok := p.parents[pid]; ok {
		return ppid, nil
	}
	return 0, errors.New("no such process")
}

func TestOrphanProcessCheck_Run(t *testing.T) {
	t.Parallel()
	const table = `  PID  PPID ARGS
  100     1 tmux new-session -d
  200   100 /bin/zsh
  300   200 claude --dangerously-skip-permissions
  400   900 /usr/local/bin/claude --dangerously-skip-permissions --resume
  500   900 claude
  900     1 /bin/zsh -l
`
	parents := map[int]int{200: 100, 100: 1, 900: 1}
	for _, tc := range []struct {
		name    string
		table   string
		want    CheckStatus
		message string
	}{
		{"no runtime processes", "  PID  PPID ARGS\n  500   900 claude\n", StatusOK, "No runtime processes found"},
		{"all inside tmux", "  PID  PPID ARGS\n  300   200 claude --dangerously-skip-permissions\n", StatusOK, "All 1 runtime processes are inside tmux"},
		{"one outside tmux", table, StatusWarning, "Found 1 runtime process(es) running outside tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := NewOrphanProcessCheck()
			check.procs = cannedProcesses{tmux: map[int]bool{100: true}, table: tc.table, parents: parents}
			result := check.Run(&CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()})
			if result.Status != tc.want || result.Message != tc.message {
				t.Fatalf("Run = %v %q, want %v %q", result.Status, result.Message, tc.want, tc.message)
			}
			if tc.want != StatusWarning {
				return
			}
			// Informational: two explanation lines and the process, no fix.
			if len(result.Details) != 3 || !strings.Contains(result.Details[2], "PID 400") {
				t.Errorf("Details = %q", result.Details)
			}
			if result.FixHint != "" {
				t.Errorf("expected no FixHint for informational check, got %q", result.FixHint)
			}
		})
	}
}

func TestOrphanProcessCheck_MessageContent(t *testing.T) {
	t.Parallel()
	// Verify the check description is correct
	check := NewOrphanProcessCheck()

	expectedDesc := "Detect runtime processes outside tmux"
	if check.Description() != expectedDesc {
		t.Errorf("expected description %q, got %q", expectedDesc, check.Description())
	}
}

func TestIsCrewSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		session string
		want    bool
	}{
		{"gt-crew-joe", true},  // gastown crew (prefix: gt)
		{"bd-crew-max", true},  // beads crew (prefix: bd)
		{"nif-crew-a", true},   // niflheim crew (prefix: nif)
		{"gt-polecat1", false}, // polecat, not crew
		{"hq-mayor", false},
		{"other-session", false},
		{"gt-crew", false}, // "crew" is a polecat name, not crew role (no name after crew-)
	}

	for _, tt := range tests {
		t.Run(tt.session, func(t *testing.T) {
			got := isCrewSession(testPrefixRegistry(), tt.session)
			if got != tt.want {
				t.Errorf("isCrewSession(%q) = %v, want %v", tt.session, got, tt.want)
			}
		})
	}
}

func TestOrphanSessionCheck_IsValidSession(t *testing.T) {
	t.Parallel()
	check := NewOrphanSessionCheck()
	validRigs := []string{"gastown", "beads"}
	mayorSession := "hq-mayor"

	tests := []struct {
		session string
		want    bool
	}{
		// Town-level sessions
		{"hq-mayor", true},

		// Valid rig sessions (using rig prefixes)
		{"gt-polecat1", true}, // gastown polecat (prefix: gt)
		{"gt-crew-joe", true}, // gastown crew
		{"bd-polecat2", true}, // beads polecat (prefix: bd)
		{"bd-crew-max", true}, // beads crew

		// Invalid rig sessions (unknown prefix/rig)
		{"zz-crew-max", false}, // unknown prefix
		{"xx-polecat1", false}, // unknown prefix

		// Non-GT sessions fail format validation
		{"other-session", false},
	}

	for _, tt := range tests {
		t.Run(tt.session, func(t *testing.T) {
			got := check.isValidSession(testPrefixRegistry(), tt.session, validRigs, mayorSession)
			if got != tt.want {
				t.Errorf("isValidSession(%q) = %v, want %v", tt.session, got, tt.want)
			}
		})
	}
}

// TestOrphanSessionCheck_IsValidSession_EdgeCases tests edge cases that have caused
// false positives in production - sessions incorrectly detected as orphans.
func TestOrphanSessionCheck_IsValidSession_EdgeCases(t *testing.T) {
	t.Parallel()
	check := NewOrphanSessionCheck()
	validRigs := []string{"gastown", "niflheim", "grctool", "7thsense", "pulseflow"}
	mayorSession := "hq-mayor"

	tests := []struct {
		name    string
		session string
		want    bool
		reason  string
	}{
		// Crew sessions with various name formats (using rig prefixes)
		{
			name:    "crew_simple_name",
			session: "gt-crew-max",
			want:    true,
			reason:  "simple crew name should be valid",
		},
		{
			name:    "crew_with_numbers",
			session: "nif-crew-codex1",
			want:    true,
			reason:  "crew name with numbers should be valid",
		},
		{
			name:    "crew_alphanumeric",
			session: "grc-crew-grc1",
			want:    true,
			reason:  "alphanumeric crew name should be valid",
		},
		{
			name:    "crew_short_name",
			session: "7s-crew-ss1",
			want:    true,
			reason:  "short crew name should be valid",
		},
		{
			name:    "crew_pf1",
			session: "pf-crew-pf1",
			want:    true,
			reason:  "pf1 crew name should be valid",
		},

		// Polecat sessions (any name after prefix should be accepted)
		{
			name:    "polecat_hash_style",
			session: "gt-abc123def",
			want:    true,
			reason:  "polecat with hash-style name should be valid",
		},
		{
			name:    "polecat_descriptive",
			session: "nif-fix-auth-bug",
			want:    true,
			reason:  "polecat with descriptive name should be valid",
		},

		// Sessions that should be detected as orphans
		{
			name:    "unknown_prefix_crew",
			session: "zz-crew-max",
			want:    false,
			reason:  "unknown prefix/rig should be orphan",
		},
		{
			name:    "malformed_no_dash",
			session: "x",
			want:    false,
			reason:  "session without dash should be invalid",
		},

		// Edge case: hyphenated rig names are no longer ambiguous because
		// each rig has a distinct prefix. E.g., rig "foo-bar" uses prefix "fb",
		// so its crew session is simply "fb-crew-<name>".
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := check.isValidSession(testPrefixRegistry(), tt.session, validRigs, mayorSession)
			if got != tt.want {
				t.Errorf("isValidSession(%q) = %v, want %v: %s", tt.session, got, tt.want, tt.reason)
			}
		})
	}
}

// TestOrphanSessionCheck_GetValidRigs verifies rig detection from filesystem.
func TestOrphanSessionCheck_GetValidRigs(t *testing.T) {
	t.Parallel()
	check := NewOrphanSessionCheck()
	townRoot := t.TempDir()

	// Setup: create mayor directory (required for getValidRigs to proceed)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("failed to create mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("failed to create rigs.json: %v", err)
	}

	// Create some rigs with polecats/crew directories
	createRigDir := func(name string, hasCrew, hasPolecats bool) {
		rigPath := filepath.Join(townRoot, name)
		os.MkdirAll(rigPath, 0755)
		if hasCrew {
			os.MkdirAll(filepath.Join(rigPath, "crew"), 0755)
		}
		if hasPolecats {
			os.MkdirAll(filepath.Join(rigPath, "polecats"), 0755)
		}
	}

	createRigDir("gastown", true, true)
	createRigDir("niflheim", true, false)
	createRigDir("grctool", false, true)
	createRigDir("not-a-rig", false, false) // No crew or polecats

	rigs := check.getValidRigs(townRoot)

	// Should find gastown, niflheim, grctool but not "not-a-rig"
	expected := map[string]bool{
		"gastown":  true,
		"niflheim": true,
		"grctool":  true,
	}

	for _, rig := range rigs {
		if !expected[rig] {
			t.Errorf("unexpected rig %q in result", rig)
		}
		delete(expected, rig)
	}

	for rig := range expected {
		t.Errorf("expected rig %q not found in result", rig)
	}
}

// TestOrphanSessionCheck_FixProtectsCrewSessions verifies that Fix() never kills crew sessions.
func TestOrphanSessionCheck_FixProtectsCrewSessions(t *testing.T) {
	t.Parallel()
	check := NewOrphanSessionCheck()

	// Simulate cached orphan sessions including a crew session
	check.orphanSessions = []string{
		"gt-crew-max",     // Crew - should be protected
		"gt-polecat1",     // Not crew - would be killed
		"nif-crew-codex1", // Crew - should be protected
	}

	// Verify isCrewSession correctly identifies crew sessions
	for _, sess := range check.orphanSessions {
		if sess == "gt-crew-max" || sess == "nif-crew-codex1" {
			if !isCrewSession(testPrefixRegistry(), sess) {
				t.Errorf("isCrewSession(%q) should return true for crew session", sess)
			}
		} else {
			if isCrewSession(testPrefixRegistry(), sess) {
				t.Errorf("isCrewSession(%q) should return false for non-crew session", sess)
			}
		}
	}
}

// TestIsCrewSession_ComprehensivePatterns tests the crew session detection pattern thoroughly.
func TestIsCrewSession_ComprehensivePatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		session string
		want    bool
		reason  string
	}{
		// Valid crew patterns (new format: <prefix>-crew-<name>)
		{"gt-crew-joe", true, "gastown crew session"},
		{"bd-crew-max", true, "beads crew session"},
		{"nif-crew-codex1", true, "niflheim crew with numbers in name"},
		{"grc-crew-grc1", true, "grctool crew with alphanumeric name"},
		{"7s-crew-ss1", true, "rig starting with number"},

		// Invalid crew patterns
		{"gt-polecat-abc", false, "polecat name, not crew"},
		{"hq-mayor", false, "mayor is not crew"},
		{"", false, "empty string"},
		{"gt-morsov", false, "polecat, not crew"},
	}

	for _, tt := range tests {
		t.Run(tt.session, func(t *testing.T) {
			got := isCrewSession(testPrefixRegistry(), tt.session)
			if got != tt.want {
				t.Errorf("isCrewSession(%q) = %v, want %v: %s", tt.session, got, tt.want, tt.reason)
			}
		})
	}
}

// TestOrphanSessionCheck_HQSessions tests that hq-* sessions are properly recognized as valid.
func TestOrphanSessionCheck_HQSessions(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("create mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("create rigs.json: %v", err)
	}

	lister := &mockSessionLister{
		sessions: []string{
			"hq-mayor", // valid: headquarters mayor session
		},
	}
	check := NewOrphanSessionCheckWithSessionLister(lister)
	result := check.Run(&CheckContext{TownRoot: townRoot, sessionPrefixes: testPrefixRegistry()})

	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK for valid hq sessions, got %v: %s", result.Status, result.Message)
	}
	if result.Message != "All 1 Gas Town sessions are valid" {
		t.Fatalf("unexpected message: %q", result.Message)
	}
	if len(check.orphanSessions) != 0 {
		t.Fatalf("expected no orphan sessions, got %v", check.orphanSessions)
	}
}

// TestOrphanSessionCheck_Run_Deterministic tests the full Run path with a mock session
// lister, ensuring deterministic behavior without depending on real tmux state.
func TestOrphanSessionCheck_Run_Deterministic(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("create mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("create rigs.json: %v", err)
	}

	// Create rig directories to make them "valid"
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "polecats"), 0o755); err != nil {
		t.Fatalf("create gastown rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "beads", "crew"), 0o755); err != nil {
		t.Fatalf("create beads rig: %v", err)
	}

	lister := &mockSessionLister{
		sessions: []string{
			"gt-polecat1",    // valid: gastown rig exists (prefix "gt")
			"gt-crew-max",    // valid: gastown rig exists
			"bd-crew-joe",    // valid: beads rig exists (prefix "bd")
			"hq-mayor",       // valid: hq-mayor is recognized
			"zz-polecat1",    // ignored: unknown prefix, not a gastown session
			"xx-crew-joe",    // ignored: unknown prefix, not a gastown session
			"random-session", // ignored: unknown prefix, not a gastown session
		},
	}
	check := NewOrphanSessionCheckWithSessionLister(lister)
	result := check.Run(&CheckContext{TownRoot: townRoot, sessionPrefixes: testPrefixRegistry()})

	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", result.Status, result.Message)
	}

	if len(check.orphanSessions) != 0 {
		t.Fatalf("expected 0 orphans (unknown prefixes are ignored), got %d: %v", len(check.orphanSessions), check.orphanSessions)
	}
}

func TestGasTownRuntimeYOLO(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  string
		args string
		want bool
	}{
		{"claude_gt", "claude", "/x/claude --dangerously-skip-permissions foo", true},
		{"claude_personal", "claude", "/x/claude foo", false},
		{"claude_code_gt", "claude-code", "claude-code --dangerously-skip-permissions", true},
		{"retired_runtime", "codex", "codex --dangerously-skip-permissions", false},
		{"unknown", "vim", "vim foo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gasTownRuntimeYOLO(tt.cmd, tt.args); got != tt.want {
				t.Errorf("gasTownRuntimeYOLO(%q, %q) = %v, want %v", tt.cmd, tt.args, got, tt.want)
			}
		})
	}
}
