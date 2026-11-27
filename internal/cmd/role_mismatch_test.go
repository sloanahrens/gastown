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
// and gt must resolve the same identity from both without a mismatch. gt never
// resolves the link on disk: the cwd is a plain string, cwd detection sees one
// segment and yields no role, so the identity comes from GT_ROLE alone. The
// test passes the link path as that string and creates no symlink, because a
// symlink here would not be read. The town layout is built under t.TempDir()
// with the production names, so the test never reads the live town tree.
func TestRoleFromTheCommonWorkPath(t *testing.T) {
	t.Parallel()
	const rig, name = "gastown", "slit"
	townRoot := filepath.Join(t.TempDir(), "gt")
	worktree := filepath.Join(townRoot, rig, "polecats", name, "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	commonPath := filepath.Join(townRoot, "work")

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

// TestCommonWorkPathIncompleteEnvFallback pins the fallback that fills a missing
// rig from cwd detection, and that the common work path fills nothing (gt-37qegv,
// gt-fc82u2). The common path is one segment that names no rig, so a fill from it
// would invent the rig "work"; a rig root is the cwd that does fill. The test
// pins the role the env names, the polecat and the incomplete flag only.
func TestCommonWorkPathIncompleteEnvFallback(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "gt")
	commonPath := filepath.Join(townRoot, "work")

	tests := []struct {
		name           string
		cwd            string
		vars           map[string]string
		wantPolecat    string
		wantIncomplete bool
	}{
		{"GT_ROLE alone fills nothing from the common path", commonPath, map[string]string{EnvGTRole: "polecat"}, "", false},
		{"GT_ROLE alone fills the rig from a rig root", filepath.Join(townRoot, "gastown"), map[string]string{EnvGTRole: "polecat"}, "", true},
		{"GT_ROLE with GT_RIG and GT_POLECAT is complete", commonPath, map[string]string{EnvGTRole: "polecat", "GT_RIG": "gastown", "GT_POLECAT": "slit"}, "slit", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := func(k string) string { return tt.vars[k] }
			info, err := getRoleWithContextEnv(tt.cwd, townRoot, env)
			if err != nil {
				t.Fatalf("getRoleWithContextEnv: %v", err)
			}
			if info.Role != RolePolecat || info.Source != "env" || info.Mismatch || info.Polecat != tt.wantPolecat || info.EnvIncomplete != tt.wantIncomplete {
				t.Errorf("got role=%q source=%q mismatch=%v polecat=%q incomplete=%v, want %q/env/false/%q/%v",
					info.Role, info.Source, info.Mismatch, info.Polecat, info.EnvIncomplete, RolePolecat, tt.wantPolecat, tt.wantIncomplete)
			}
		})
	}
}
