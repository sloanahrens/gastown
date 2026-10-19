package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/session"
)

// fakeCollisionLister is a tmux whose ListSessions answers sessions, err.
type fakeCollisionLister struct {
	sessions []string
	err      error
}

func (f fakeCollisionLister) ListSessions() ([]string, error) { return f.sessions, f.err }

// liveCrewLockTown is a town root holding one lock, held by this test process,
// for crew member alice of rig gastown.
func liveCrewLockTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	runtimeDir := filepath.Join(townRoot, "gastown", "crew", "alice", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(lock.LockInfo{PID: os.Getpid(), AcquiredAt: time.Now(), SessionID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

func TestWriteAgentsFixReportListsOrphanedLocks(t *testing.T) {
	t.Parallel()
	report := &CollisionReport{
		Collisions: 1,
		Issues: []CollisionIssue{{
			Type:      "orphaned",
			WorkerDir: "/town/gastown/crew/alice",
			Message:   "Lock exists (PID 42) but no tmux session 'gt-crew-alice'",
			PID:       42,
		}},
	}
	var out bytes.Buffer
	writeAgentsFixReport(&out, report)

	got := out.String()
	header := strings.Index(got, "1 collision(s) require manual intervention")
	if header < 0 {
		t.Fatalf("missing the manual-intervention header:\n%s", got)
	}
	if listed := strings.Index(got, "Lock exists (PID 42) but no tmux session 'gt-crew-alice'"); listed < header {
		t.Errorf("the orphaned issue must be listed under the header:\n%s", got)
	}
}

func TestWriteAgentsFixReportNoCollisionsPrintsNothing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	writeAgentsFixReport(&out, &CollisionReport{StaleLocks: 2})
	if out.Len() != 0 {
		t.Errorf("a report with no collisions must print nothing, got:\n%s", out.String())
	}
}

func TestBuildCollisionReportListSessionsError(t *testing.T) {
	t.Parallel()
	townRoot := liveCrewLockTown(t)
	boom := errors.New("tmux: protocol version mismatch")

	report, err := buildCollisionReport(fakeCollisionLister{err: boom}, session.NewPrefixRegistry(), townRoot)
	if !errors.Is(err, boom) {
		t.Fatalf("buildCollisionReport error = %v, want it to wrap %v", err, boom)
	}
	if report != nil && report.Collisions != 0 {
		t.Errorf("unknown session state must not be reported as %d collision(s)", report.Collisions)
	}
}

// With no tmux server ListSessions answers nil, nil: the lock's session is
// genuinely absent, so the live lock is an orphan.
func TestBuildCollisionReportNoSessionsOrphansLiveLock(t *testing.T) {
	t.Parallel()
	townRoot := liveCrewLockTown(t)

	report, err := buildCollisionReport(fakeCollisionLister{}, session.NewPrefixRegistry(), townRoot)
	if err != nil {
		t.Fatalf("buildCollisionReport: %v", err)
	}
	if report.Collisions != 1 || len(report.Issues) != 1 || report.Issues[0].Type != "orphaned" {
		t.Errorf("want one orphaned issue, got collisions=%d issues=%+v", report.Collisions, report.Issues)
	}
}

func TestAgentsCmd_DefaultRunE(t *testing.T) {
	t.Parallel()
	// After the fix, `gt agents` (no subcommand) should run the list function,
	// not the interactive popup menu. Verify the actual function pointer.
	if agentsCmd.RunE == nil {
		t.Fatal("agentsCmd.RunE is nil")
	}

	gotPtr := reflect.ValueOf(agentsCmd.RunE).Pointer()
	wantPtr := reflect.ValueOf(runAgentsList).Pointer()
	if gotPtr != wantPtr {
		t.Errorf("agentsCmd.RunE points to wrong function (got %v, want runAgentsList %v)", gotPtr, wantPtr)
	}
}

func TestAgentsMenuCmd_Exists(t *testing.T) {
	t.Parallel()
	found := false
	for _, sub := range agentsCmd.Commands() {
		if sub.Use == "menu" {
			found = true
			break
		}
	}
	if !found {
		t.Error("agentsMenuCmd not registered as subcommand of agentsCmd")
	}
}

func TestAgentsMenuCmd_RunE(t *testing.T) {
	t.Parallel()
	var menuCmd *cobra.Command
	for _, sub := range agentsCmd.Commands() {
		if sub.Use == "menu" {
			menuCmd = sub
			break
		}
	}
	if menuCmd == nil {
		t.Fatal("agentsMenuCmd not found")
	}
	if menuCmd.RunE == nil {
		t.Fatal("agentsMenuCmd.RunE is nil")
	}
}

func TestAgentsListCmd_StillRegistered(t *testing.T) {
	t.Parallel()
	found := false
	for _, sub := range agentsCmd.Commands() {
		if sub.Use == "list" {
			found = true
			break
		}
	}
	if !found {
		t.Error("agentsListCmd not registered as subcommand of agentsCmd")
	}
}

func TestAgentsCmd_ShortDescription(t *testing.T) {
	t.Parallel()
	if agentsCmd.Short == "Switch between Gas Town agent sessions" {
		t.Error("agentsCmd.Short still describes popup menu behavior; should describe listing")
	}
}

func TestCategorizeSession_AllTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantType AgentType
	}{
		// Rig-level sessions require a registered prefix. Use "gt" which is
		// commonly registered in the default PrefixRegistry.
		{"crew", "gt-crew-max", AgentCrew},
		{"polecat", "gt-furiosa", AgentPolecat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categorizeSession(cmdTestRegistry(), tt.input)
			if got == nil {
				t.Fatalf("categorizeSession(%q) = nil, want type %d", tt.input, tt.wantType)
			}
			if got.Type != tt.wantType {
				t.Errorf("categorizeSession(%q).Type = %d, want %d", tt.input, got.Type, tt.wantType)
			}
		})
	}
}

func TestCategorizeSession_InvalidName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"random string", "not-a-gastown-session"},
		{"bare word", "foobar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categorizeSession(cmdTestRegistry(), tt.input)
			if got != nil {
				t.Errorf("categorizeSession(%q) = %+v, want nil", tt.input, got)
			}
		})
	}
}

func TestCategorizeSession_Overseer(t *testing.T) {
	t.Parallel()
	got := categorizeSession(cmdTestRegistry(), "hq-overseer")
	if got != nil {
		t.Errorf("categorizeSession(%q) = %+v, want nil (overseer is not a display agent)", "hq-overseer", got)
	}
}

func TestCategorizeSession_EmptyString(t *testing.T) {
	t.Parallel()
	got := categorizeSession(cmdTestRegistry(), "")
	if got != nil {
		t.Errorf("categorizeSession(%q) = %+v, want nil", "", got)
	}
}

func TestShortcutKey_Range(t *testing.T) {
	t.Parallel()
	tests := []struct {
		index int
		want  string
	}{
		{0, "1"},
		{1, "2"},
		{8, "9"},
		{9, "a"},
		{10, "b"},
		{34, "z"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := shortcutKey(tt.index)
			if got != tt.want {
				t.Errorf("shortcutKey(%d) = %q, want %q", tt.index, got, tt.want)
			}
		})
	}
}

func TestShortcutKey_BeyondRange(t *testing.T) {
	t.Parallel()
	tests := []int{35, 36, 100}
	for _, idx := range tests {
		got := shortcutKey(idx)
		if got != "" {
			t.Errorf("shortcutKey(%d) = %q, want empty string", idx, got)
		}
	}
}

func TestDisplayLabel_AllTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		agent       AgentSession
		wantContain string
	}{
		{"crew", AgentSession{Name: "gt-crew-max", Type: AgentCrew, Rig: "gastown", AgentName: "max"}, "crew/max"},
		{"polecat", AgentSession{Name: "gt-furiosa", Type: AgentPolecat, Rig: "gastown", AgentName: "furiosa"}, "furiosa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label := tt.agent.displayLabel()
			if label == "" {
				t.Errorf("displayLabel() for %s returned empty string", tt.name)
			}
			if !strings.Contains(label, tt.wantContain) {
				t.Errorf("displayLabel() = %q, want substring %q", label, tt.wantContain)
			}
		})
	}
}

// --- filterAndSortSessions tests ---

func TestFilterAndSortSessions_NoSessions(t *testing.T) {
	t.Parallel()
	got := filterAndSortSessions(cmdTestRegistry(), nil, true)
	if len(got) != 0 {
		t.Errorf("filterAndSortSessions(nil) returned %d agents, want 0", len(got))
	}

	got = filterAndSortSessions(cmdTestRegistry(), []string{}, true)
	if len(got) != 0 {
		t.Errorf("filterAndSortSessions([]) returned %d agents, want 0", len(got))
	}
}

func TestFilterAndSortSessions_AllFiltered(t *testing.T) {
	t.Parallel()
	input := []string{
		"my-tmux-session",
		"dev-workspace",
		"random-thing",
	}
	got := filterAndSortSessions(cmdTestRegistry(), input, true)
	if len(got) != 0 {
		t.Errorf("filterAndSortSessions(non-gastown names) returned %d agents, want 0", len(got))
	}
}

func TestFilterAndSortSessions_PolecatFiltering(t *testing.T) {
	t.Parallel()
	input := []string{
		"gt-furiosa", // polecat
		"gt-crew-max",
	}

	// With polecats excluded
	got := filterAndSortSessions(cmdTestRegistry(), input, false)
	for _, a := range got {
		if a.Type == AgentPolecat {
			t.Errorf("polecat %q present when includePolecats=false", a.Name)
		}
	}
	if len(got) != 1 {
		t.Errorf("filterAndSortSessions(includePolecats=false) returned %d agents, want 1", len(got))
	}

	// With polecats included
	got = filterAndSortSessions(cmdTestRegistry(), input, true)
	hasPolecat := false
	for _, a := range got {
		if a.Type == AgentPolecat {
			hasPolecat = true
		}
	}
	if !hasPolecat {
		t.Error("no polecat found when includePolecats=true")
	}
	if len(got) != 2 {
		t.Errorf("filterAndSortSessions(includePolecats=true) returned %d agents, want 2", len(got))
	}
}

func TestFilterAndSortSessions_RetiredSessionFiltered(t *testing.T) {
	t.Parallel()
	input := []string{
		"gt-crew-max",
		"hq-deacon", // retired role session: should always be excluded
	}

	got := filterAndSortSessions(cmdTestRegistry(), input, true)
	for _, a := range got {
		if a.Name == "hq-deacon" {
			t.Error("hq-deacon session should be filtered out")
		}
	}
	if len(got) != 1 {
		t.Errorf("filterAndSortSessions with a retired deacon session returned %d agents, want 1 (retired sessions are not agents)", len(got))
	}
}

func TestFilterAndSortSessions_SortOrder(t *testing.T) {
	t.Parallel()
	input := []string{
		"gt-crew-zed",   // crew (gastown)
		"gt-furiosa",    // polecat (gastown)
		"mr-crew-bob",   // crew (myrig)
		"gt-crew-alpha", // crew (gastown)
	}

	got := filterAndSortSessions(cmdTestRegistry(), input, true)

	// Expected order:
	// 1. gastown/crew/alpha (rig "gastown" < "myrig", alpha < zed)
	// 2. gastown/crew/zed
	// 3. gastown/polecat/furiosa (polecat last within rig)
	// 4. myrig/crew/bob
	wantOrder := []struct {
		wantType AgentType
		wantName string
	}{
		{AgentCrew, "gt-crew-alpha"},
		{AgentCrew, "gt-crew-zed"},
		{AgentPolecat, "gt-furiosa"},
		{AgentCrew, "mr-crew-bob"},
	}

	if len(got) != len(wantOrder) {
		t.Fatalf("filterAndSortSessions returned %d agents, want %d", len(got), len(wantOrder))
	}

	for i, want := range wantOrder {
		if got[i].Type != want.wantType {
			t.Errorf("position %d: type = %d, want %d (session %q)", i, got[i].Type, want.wantType, got[i].Name)
		}
		if got[i].Name != want.wantName {
			t.Errorf("position %d: name = %q, want %q", i, got[i].Name, want.wantName)
		}
	}
}

func TestFilterAndSortSessions_CombinedFiltering(t *testing.T) {
	t.Parallel()
	input := []string{
		"hq-deacon",      // retired role session: always filtered
		"gt-furiosa",     // polecat: filtered when includePolecats=false
		"random-session", // non-gastown: always filtered
		"gt-crew-max",
	}

	got := filterAndSortSessions(cmdTestRegistry(), input, false)
	if len(got) != 1 {
		t.Fatalf("filterAndSortSessions(combined, polecats=false) returned %d agents, want 1 (crew)", len(got))
	}
	if got[0].Type != AgentCrew {
		t.Errorf("position 0: type = %d, want AgentCrew", got[0].Type)
	}

	got = filterAndSortSessions(cmdTestRegistry(), input, true)
	if len(got) != 2 {
		t.Fatalf("filterAndSortSessions(combined, polecats=true) returned %d agents, want 2 (crew + polecat)", len(got))
	}
}

// TestDisplayLabel_PersonalSession verifies the display format for non-GT sessions.
func TestDisplayLabel_PersonalSession(t *testing.T) {
	t.Parallel()
	agent := AgentSession{Name: "fix-tmux", Type: AgentPersonal}
	label := agent.displayLabel()
	if !strings.Contains(label, "fix-tmux") {
		t.Errorf("personal session label should contain session name, got: %q", label)
	}
	if !strings.Contains(label, AgentTypeColors[AgentPersonal]) {
		t.Errorf("personal session label should use AgentPersonal color, got: %q", label)
	}
}

// TestBuildMenuAction_PerSessionSocket verifies that buildMenuAction uses the
// session's own socket, not a global town socket.
func TestBuildMenuAction_PerSessionSocket(t *testing.T) {
	t.Parallel()
	// GT session on the gt socket
	action := buildMenuAction("gt", "gt-crew-max")
	if !strings.Contains(action, "-L gt") {
		t.Errorf("GT session action should use -L gt, got: %s", action)
	}

	// Personal session on the default socket
	action = buildMenuAction("default", "fix-tmux")
	if !strings.Contains(action, "-L default") {
		t.Errorf("personal session action should use -L default, got: %s", action)
	}
	if !strings.Contains(action, "fix-tmux") {
		t.Errorf("personal session action should target fix-tmux, got: %s", action)
	}
}

// TestBuildMenuAction_CrossSocket verifies that menu actions handle
// cross-socket switching. When the town socket is set, the action must:
// 1. Try switch-client first (works when user is on the same socket, no flicker)
// 2. Fall back to detach+reattach (works cross-socket)
// 3. Include the -L <socket> flag so tmux targets the correct server
func TestBuildMenuAction_CrossSocket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		townSocket  string
		session     string
		wantContain []string // substrings that must be present
		wantMissing []string // substrings that must NOT be present
	}{
		{
			name:       "with town socket — cross-socket aware",
			townSocket: "gt",
			session:    "gt-crew-max",
			wantContain: []string{
				"-L gt",         // targets the town socket
				"switch-client", // fast path (same socket)
				"detach-client", // fallback (cross-socket)
				"gt-crew-max",   // session name
			},
		},
		{
			name:       "empty socket — same-server switch only",
			townSocket: "",
			session:    "hq-deacon",
			wantContain: []string{
				"switch-client",
				"hq-deacon",
			},
			wantMissing: []string{
				"detach-client", // no cross-socket fallback needed
				"-L ",           // no socket flag
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := buildMenuAction(tt.townSocket, tt.session)
			for _, want := range tt.wantContain {
				if !strings.Contains(action, want) {
					t.Errorf("buildMenuAction(%q, %q) = %q\n  missing substring: %q",
						tt.townSocket, tt.session, action, want)
				}
			}
			for _, notWant := range tt.wantMissing {
				if strings.Contains(action, notWant) {
					t.Errorf("buildMenuAction(%q, %q) = %q\n  should not contain: %q",
						tt.townSocket, tt.session, action, notWant)
				}
			}
		})
	}
}

// --- AgentTest type tests ---

func TestAgentTestColor_Exists(t *testing.T) {
	t.Parallel()
	color, ok := AgentTypeColors[AgentTest]
	if !ok {
		t.Fatal("AgentTypeColors missing entry for AgentTest")
	}
	if color == "" {
		t.Error("AgentTest color should not be empty")
	}
	if !strings.Contains(color, "yellow") {
		t.Errorf("AgentTest color = %q, expected yellow (dim)", color)
	}
}

func TestDisplayLabel_TestSession(t *testing.T) {
	t.Parallel()
	agent := AgentSession{Name: "test-session-1", Type: AgentTest, Socket: "gt-test-tmux-12345"}
	label := agent.displayLabel()
	if !strings.Contains(label, "test-session-1") {
		t.Errorf("test session label should contain session name, got: %q", label)
	}
	if !strings.Contains(label, AgentTypeColors[AgentTest]) {
		t.Errorf("test session label should use AgentTest color, got: %q", label)
	}
}

func TestSocketDisplayName_TestSocket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		socket string
		want   string
	}{
		{"test-tmux socket", "gt-test-tmux-12345", "testing"},
		{"test-cmd socket", "gt-test-cmd-67890", "testing"},
		{"test-config socket", "gt-test-config-111", "testing"},
		{"non-test socket", "my-custom-socket", "my-custom-socket"},
		{"default socket", "default", "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := socketDisplayName(tt.socket)
			if got != tt.want {
				t.Errorf("socketDisplayName(%q) = %q, want %q", tt.socket, got, tt.want)
			}
		})
	}
}

func TestTestSocketPackage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		socket string
		want   string
	}{
		{"gt-test-tmux-12345", "tmux"},
		{"gt-test-cmd-67890", "cmd"},
		// constants.TestSocketName appends both a unique nanosecond field and
		// the owning pid, so the tail has more than one numeric field.
		{"gt-test-tmux-1758012345678901234-12345", "tmux"},
		{"gt-test-dog-stale-1758012345678901234-12345", "dog-stale"},
		{"gt-test-91506", "91506"},
		{"gt-test-sentinel", "sentinel"},
		{"my-custom-socket", "my-custom-socket"},
	}
	for _, tt := range tests {
		if got := testSocketPackage(tt.socket); got != tt.want {
			t.Errorf("testSocketPackage(%q) = %q, want %q", tt.socket, got, tt.want)
		}
	}
}

func TestBuildMenuAction_TestSocket(t *testing.T) {
	t.Parallel()
	action := buildMenuAction("gt-test-tmux-12345", "test-session")
	if !strings.Contains(action, "-L gt-test-tmux-12345") {
		t.Errorf("test socket action should use -L gt-test-tmux-12345, got: %s", action)
	}
	if !strings.Contains(action, "test-session") {
		t.Errorf("test socket action should target test-session, got: %s", action)
	}
	if !strings.Contains(action, "switch-client") {
		t.Errorf("test socket action should try switch-client first, got: %s", action)
	}
	if !strings.Contains(action, "detach-client") {
		t.Errorf("test socket action should have cross-socket fallback, got: %s", action)
	}
}

func TestGuessSessionFromWorkerDir(t *testing.T) {
	t.Parallel()
	townRoot := "/town"

	tests := []struct {
		name      string
		workerDir string
		want      string
	}{
		{"crew worker", "/town/gastown/crew/max", "gt-crew-max"},
		{"polecat worker", "/town/gastown/polecats/furiosa", "gt-furiosa"},
		{"retired witness dir", "/town/gastown/witness/main", ""},
		{"unknown type", "/town/gastown/unknown/thing", ""},
		{"too few path parts", "/town/gastown", ""},
		{"different rig", "/town/myrig/crew/alpha", "mr-crew-alpha"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guessSessionFromWorkerDir(cmdTestRegistry(), tt.workerDir, townRoot)
			if got != tt.want {
				t.Errorf("guessSessionFromWorkerDir(%q, %q) = %q, want %q",
					tt.workerDir, townRoot, got, tt.want)
			}
		})
	}
}
