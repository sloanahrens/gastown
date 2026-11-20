package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRoleMismatchTownRootIsNeutral pins the #1496 regression that
// role_e2e_test.go used to cover through the deleted `gt role show`: the town
// root and the kept mayor/ marker directory detect as RoleUnknown, so no
// GT_ROLE set in either is a mismatch. A stale GT_ROLE=mayor is tolerated
// there too — it parses to the retired role value rather than erroring
// (gt-rwp7z). The mismatch flag still ships; gt prime reads it.
func TestRoleMismatchTownRootIsNeutral(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "hq")
	mayorDir := filepath.Join(townRoot, "mayor")
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}

	tests := []struct {
		name         string
		cwd          string
		gtRole       string
		wantRole     Role
		wantSource   string
		wantMismatch bool
	}{
		{"town root with GT_ROLE=mayor", townRoot, "mayor", Role("mayor"), "env", false},
		{"mayor dir with GT_ROLE=mayor", mayorDir, "mayor", Role("mayor"), "env", false},
		{"mayor dir without GT_ROLE", mayorDir, "", RoleUnknown, "cwd", false},
		// Control: a cwd that does detect a role still flags a disagreeing env.
		{"polecat dir with GT_ROLE=crew", filepath.Join(townRoot, "gastown", "polecats", "jade"), "gastown/crew/max", RoleCrew, "env", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := getRoleWithContextEnv(tt.cwd, townRoot, env(map[string]string{EnvGTRole: tt.gtRole}))
			if err != nil {
				t.Fatalf("getRoleWithContextEnv: %v", err)
			}
			if info.Role != tt.wantRole || info.Source != tt.wantSource || info.Mismatch != tt.wantMismatch {
				t.Errorf("got role=%q source=%q mismatch=%v, want role=%q source=%q mismatch=%v",
					info.Role, info.Source, info.Mismatch, tt.wantRole, tt.wantSource, tt.wantMismatch)
			}
		})
	}
}

// TestRoleFromTheCommonWorkPath pins the sandbox common work path (gt-f80lq2):
// a seat starts in the link /sandbox/gt/work instead of its real worktree path,
// and gt must resolve the same identity from both without a mismatch. The town
// layout is built under t.TempDir() with the production names, so the test
// never reads the live town tree.
func TestRoleFromTheCommonWorkPath(t *testing.T) {
	t.Parallel()
	const rig, name = "gastown", "slit"
	townRoot := filepath.Join(t.TempDir(), "gt")
	worktree := filepath.Join(townRoot, rig, "polecats", name, "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	commonPath := filepath.Join(townRoot, "work")
	if err := os.Symlink(worktree, commonPath); err != nil {
		t.Fatalf("link the common work path: %v", err)
	}

	vars := map[string]string{EnvGTRole: rig + "/polecats/" + name, "GT_RIG": rig, "GT_POLECAT": name}
	env := func(k string) string { return vars[k] }

	fromWorktree, err := getRoleWithContextEnv(worktree, townRoot, env)
	if err != nil {
		t.Fatalf("getRoleWithContextEnv from the worktree: %v", err)
	}
	fromCommon, err := getRoleWithContextEnv(commonPath, townRoot, env)
	if err != nil {
		t.Fatalf("getRoleWithContextEnv from the common work path: %v", err)
	}

	// Assert the seat's identity explicitly, so two empty answers cannot pass.
	for what, info := range map[string]RoleInfo{"worktree": fromWorktree, "common work path": fromCommon} {
		if info.Role != RolePolecat || info.Rig != rig || info.Polecat != name || info.Source != "env" || info.Mismatch {
			t.Errorf("%s: got role=%q rig=%q polecat=%q source=%q mismatch=%v, want %q/%q/%q/env/false",
				what, info.Role, info.Rig, info.Polecat, info.Source, info.Mismatch, RolePolecat, rig, name)
		}
	}
}
