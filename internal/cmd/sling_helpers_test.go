package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
)

func TestIsDeferredBead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		info *beadInfo
		want bool
	}{
		{"open bead is not deferred", &beadInfo{Status: "open", Description: "some task"}, false},
		{"in_progress bead is not deferred", &beadInfo{Status: "in_progress", Description: "working on it"}, false},
		{"deferred status", &beadInfo{Status: "deferred", Description: "some task"}, true},
		{"description says deferred to post-launch", &beadInfo{Status: "open", Description: "deferred to post-launch"}, true},
		{"description says deferred to post launch", &beadInfo{Status: "open", Description: "deferred to post launch"}, true},
		{"description says status: deferred", &beadInfo{Status: "open", Description: "status: deferred\nsome other notes"}, true},
		{"case insensitive description", &beadInfo{Status: "open", Description: "Deferred to Post-Launch"}, true},
		{"deferred keyword not in deferral phrase", &beadInfo{Status: "open", Description: "the user deferred this action"}, false},
		{"empty description", &beadInfo{Status: "open", Description: ""}, false},
		{"hooked bead not deferred", &beadInfo{Status: "hooked", Description: "some work"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDeferredBead(tt.info); got != tt.want {
				t.Errorf("isDeferredBead(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

func TestCollectExistingMoleculesFiltersClosedMolecules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		info *beadInfo
		want []string
	}{
		{
			name: "open molecule is collected",
			info: &beadInfo{
				Dependencies: []beads.IssueDep{
					{ID: "bd-wisp-abc", Status: "open"},
				},
			},
			want: []string{"bd-wisp-abc"},
		},
		{
			name: "closed molecule is skipped",
			info: &beadInfo{
				Dependencies: []beads.IssueDep{
					{ID: "bd-wisp-abc", Status: "closed"},
				},
			},
			want: nil,
		},
		{
			name: "tombstone molecule is skipped",
			info: &beadInfo{
				Dependencies: []beads.IssueDep{
					{ID: "bd-wisp-abc", Status: "tombstone"},
				},
			},
			want: nil,
		},
		{
			name: "mixed: open kept, closed skipped",
			info: &beadInfo{
				Dependencies: []beads.IssueDep{
					{ID: "bd-wisp-dead", Status: "closed"},
					{ID: "bd-wisp-live", Status: "in_progress"},
				},
			},
			want: []string{"bd-wisp-live"},
		},
		{
			name: "non-wisp dependency ignored regardless of status",
			info: &beadInfo{
				Dependencies: []beads.IssueDep{
					{ID: "bd-regular-dep", Status: "open"},
				},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectExistingMolecules(tt.info)
			if len(got) != len(tt.want) {
				t.Fatalf("collectExistingMolecules() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("collectExistingMolecules()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestCollectExistingMoleculeDepsReadsCanonicalWispEdges: the molecules
// bonded to a bead come from one sql query over the wisp dependency edges,
// whose rows are deduplicated in order.
func TestCollectExistingMoleculeDepsReadsCanonicalWispEdges(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.OnSQL(func(query string) ([][]string, error) {
		if !strings.Contains(query, "wisp_dependencies") || !strings.Contains(query, "depends_on_issue_id") || !strings.Contains(query, "depends_on_wisp_id") {
			return nil, fmt.Errorf("unexpected query %q", query)
		}
		return [][]string{{"issue_id"}, {"gt-wisp-live"}, {"gt-wisp-live"}, {"gt-wisp-other"}}, nil
	})

	got, err := fakeSlingStores(db).moleculeDeps("gt-work", t.TempDir())
	if err != nil {
		t.Fatalf("collectExistingMoleculeDeps: %v", err)
	}
	want := []string{"gt-wisp-live", "gt-wisp-other"}
	if len(got) != len(want) {
		t.Fatalf("collectExistingMoleculeDeps() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collectExistingMoleculeDeps()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsSlingConfigError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"not initialized", fmt.Errorf("database not initialized"), true},
		{"no such table", fmt.Errorf("no such table: issues"), true},
		{"table not found", fmt.Errorf("table not found: issues"), true},
		{"issue_prefix missing", fmt.Errorf("issue_prefix not configured"), true},
		{"no database", fmt.Errorf("no database found"), true},
		{"database not found", fmt.Errorf("database not found"), true},
		{"connection refused", fmt.Errorf("connection refused"), true},
		{"circuit breaker", fmt.Errorf("Dolt circuit breaker is open: server appears down"), true},
		{"server appears down", fmt.Errorf("server appears down"), true},
		{"server down", fmt.Errorf("server down"), true},
		{"server not running", fmt.Errorf("Dolt server is not running"), true},
		{"server may not be running", fmt.Errorf("Dolt server may not be running"), true},
		{"transient error", fmt.Errorf("optimistic lock failed"), false},
		{"generic error", fmt.Errorf("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSlingConfigError(tt.err); got != tt.want {
				t.Errorf("isSlingConfigError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// updateFailsStore is a sling store whose every Update fails with err.
type updateFailsStore struct {
	slingFake
	err   error
	calls *int
}

func (s updateFailsStore) Update(string, beads.UpdateOptions) error {
	*s.calls++
	return s.err
}

// TestHookBeadWithRetryFailsFastOnBdStderr: a Dolt/beads configuration
// failure bd reports on stderr is not retried, and the error carries bd's
// words plus the reconciliation guidance (gt-2ra).
func TestHookBeadWithRetryFailsFastOnBdStderr(t *testing.T) {
	t.Parallel()
	calls := 0
	store := updateFailsStore{slingFake{beadsfake.New()}, fmt.Errorf("bd update gt-work: Dolt circuit breaker is open: server appears down"), &calls}
	stores := slingStores{pinned: func(string) slingStore { return store }}

	err := stores.hookWithRetry(nil, "gt-work", "gastown/polecats/rust", t.TempDir())
	if err == nil {
		t.Fatal("hookBeadWithRetry error = nil, want fail-fast error")
	}
	if !strings.Contains(err.Error(), "Dolt circuit breaker is open") {
		t.Fatalf("error missing bd stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "Safe next action") {
		t.Fatalf("error missing reconciliation guidance: %v", err)
	}
	if calls != 1 {
		t.Fatalf("bd update invoked %d times, want 1", calls)
	}
}

// TestLoadRigCommandVarsReadsRigRootMergeQueue reproduces gt-me9t: operators
// following docs/onboard-repo write merge_queue.{build,test,lint}_command
// into <rig>/config.json (the rig root config, alongside default_branch and
// beads.prefix). loadRigCommandVars must surface those commands as formula
// vars, not just the ones from settings/config.json or the repo-committed
// .gastown/settings.json.
func TestLoadRigCommandVarsReadsRigRootMergeQueue(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://github.com/sloanahrens/gastown.git",
  "default_branch": "main",
  "beads": {"prefix": "gt"},
  "merge_queue": {
    "build_command": "make build",
    "test_command": "make test",
    "lint_command": "make lint"
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	vars := loadRigCommandVars(townRoot, "gastown")

	want := map[string]string{
		"build_command": "make build",
		"test_command":  "make test",
		"lint_command":  "make lint",
	}
	got := make(map[string]string, len(vars))
	for _, v := range vars {
		if eq := strings.Index(v, "="); eq > 0 {
			got[v[:eq]] = v[eq+1:]
		}
	}
	for key, wantVal := range want {
		if gotVal, ok := got[key]; !ok || gotVal != wantVal {
			t.Errorf("loadRigCommandVars() var %q = %q, want %q (vars: %v)", key, gotVal, wantVal, vars)
		}
	}
}

// TestLoadRigCommandVarsPrecedence verifies the three-tier merge order
// mandated by gt-e50d's narrowed fix: rig root config.json is the floor,
// the repo-committed .gastown/settings.json overrides it, and rig-local
// settings/config.json has the final say. Each tier sets a distinct command
// so the test fails if rootMQ and repoMQ are ever collapsed into a single
// "floor" or their precedence is flipped.
func TestLoadRigCommandVarsPrecedence(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	settingsDir := filepath.Join(rigDir, "settings")
	for _, dir := range []string{gastownDir, settingsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Floor: rig root config.json sets all three commands.
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://github.com/sloanahrens/gastown.git",
  "default_branch": "main",
  "beads": {"prefix": "gt"},
  "merge_queue": {
    "build_command": "make build-root",
    "test_command": "make test-root",
    "lint_command": "make lint-root"
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	// Repo-committed settings override the floor for build and test, but
	// leave lint unset so it must fall through to the rig-root floor.
	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "build_command": "make build-repo",
    "test_command": "make test-repo"
  }
}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}

	// Rig-local operator override has the final say, but only touches build.
	localSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "build_command": "make build-local"
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write settings/config.json: %v", err)
	}

	vars := loadRigCommandVars(townRoot, "gastown")

	got := make(map[string]string, len(vars))
	for _, v := range vars {
		if eq := strings.Index(v, "="); eq > 0 {
			got[v[:eq]] = v[eq+1:]
		}
	}

	want := map[string]string{
		"build_command": "make build-local", // rig-local wins over both repo and root
		"test_command":  "make test-repo",   // repo wins over root floor
		"lint_command":  "make lint-root",   // neither repo nor local set it — falls to root floor
	}
	for key, wantVal := range want {
		if gotVal, ok := got[key]; !ok || gotVal != wantVal {
			t.Errorf("loadRigCommandVars() var %q = %q, want %q (vars: %v)", key, gotVal, wantVal, vars)
		}
	}
}

func TestShouldAcceptPermissionWarning_ResolvedPreset(t *testing.T) {
	t.Parallel()
	claude := config.GetAgentPresetByName("claude")
	if !shouldAcceptPermissionWarning("deepseek-flash", claude, true) {
		t.Error("custom agent resolved to claude must accept the bypass-permissions warning")
	}
	if shouldAcceptPermissionWarning("mystery", nil, false) {
		t.Error("unresolved agent must not accept")
	}
	if !shouldAcceptPermissionWarning("", nil, false) {
		t.Error("session without GT_AGENT is Claude by default")
	}
}

func TestFormulaShowHasBody(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"":                    false,
		"\n":                  false,
		"null\n":              false,
		`{"formula":"mol-x"}`: true,
		"formula: mol-x\n":    true,
	} {
		if got := formulaShowHasBody([]byte(out)); got != want {
			t.Errorf("formulaShowHasBody(%q) = %v, want %v", out, got, want)
		}
	}
}

// TestVerifyFormulaTriesMolPrefixAndChecksBody: a formula is found under its
// own name or with the mol- prefix, in the town's formula engine at workDir;
// a show that prints no formula does not count.
func TestVerifyFormulaTriesMolPrefixAndChecksBody(t *testing.T) {
	t.Parallel()
	fake := &fakeCook{show: func(name string) []byte {
		switch name {
		case "mol-widget":
			return []byte(`{"formula":"mol-widget"}`)
		case "ghost", "mol-ghost":
			return []byte("null\n")
		}
		return nil
	}}
	f := formulaBD{open: fake.open}
	if err := f.verifyFormula("widget", "/town/gastown", "/town"); err != nil {
		t.Fatalf("widget via mol-widget: %v", err)
	}
	if got, want := fake.log(), "formula show widget\nformula show mol-widget"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if site := fake.calls[0].site; site.dir != "/town/gastown" || site.townRoot != "/town" {
		t.Errorf("site = %+v, want dir /town/gastown in town /town", site)
	}
	if err := f.verifyFormula("ghost", "", "/town"); err == nil || !strings.Contains(err.Error(), "formula 'ghost' not found") {
		t.Errorf("ghost: err = %v, want not found", err)
	}
}

// TestClearReassignedPolecatStateRestoresOnDemand: the clear that runs before a
// force steal's spawn must be undoable — a dispatch whose spawn then fails hands
// the outgoing holder its hook_bead and agent_state back (gt-u0zq0).
func TestClearReassignedPolecatStateRestoresOnDemand(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead())

	restore := clearReassignedPolecatStateIn(db, "gt-gastown-polecat-nux")
	_, cleared, err := beads.GetAgentBead(db, "gt-gastown-polecat-nux")
	if err != nil || cleared == nil {
		t.Fatalf("agent bead after the clear: %+v, %v", cleared, err)
	}
	if cleared.HookBead != "" || cleared.AgentState != string(beads.AgentStateIdle) {
		t.Fatalf("after the clear: hook %q state %q, want empty hook and idle", cleared.HookBead, cleared.AgentState)
	}

	restore()
	_, back, err := beads.GetAgentBead(db, "gt-gastown-polecat-nux")
	if err != nil || back == nil {
		t.Fatalf("agent bead after the restore: %+v, %v", back, err)
	}
	if back.HookBead != "gt-base-123" || back.AgentState != "working" {
		t.Errorf("after the restore: hook %q state %q, want gt-base-123 and working", back.HookBead, back.AgentState)
	}

	// A restore that runs twice (an error path and a rollback) is the same
	// restore, not a second write of stale state.
	restore()
	_, again, err := beads.GetAgentBead(db, "gt-gastown-polecat-nux")
	if err != nil || again == nil {
		t.Fatalf("agent bead after the second restore: %+v, %v", again, err)
	}
	if again.HookBead != "gt-base-123" || again.AgentState != "working" {
		t.Errorf("second restore: hook %q state %q, want gt-base-123 and working", again.HookBead, again.AgentState)
	}
}

// TestClearReassignedPolecatStateWithoutABeadStillClears: a holder whose agent
// bead cannot be read has nothing to restore; the clear still runs and the
// returned restore is a no-op.
func TestClearReassignedPolecatStateWithoutABeadStillClears(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-other", Title: "not an agent bead", Status: "open"})

	restore := clearReassignedPolecatStateIn(db, "gt-gastown-polecat-nux")
	if restore == nil {
		t.Fatal("clearReassignedPolecatStateIn returned no restore")
	}
	restore()
}
