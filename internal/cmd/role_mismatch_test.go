package cmd

import (
	"path/filepath"
	"testing"
)

// TestRoleMismatchTownRootIsNeutral pins the #1496 regression that
// role_e2e_test.go used to cover through the deleted `gt role show`: the town
// root detects as RoleUnknown, so no GT_ROLE set there is a mismatch. The
// mismatch flag still ships; gt prime reads it.
func TestRoleMismatchTownRootIsNeutral(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "hq")
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
		{"town root with GT_ROLE=mayor", townRoot, "mayor", RoleMayor, "env", false},
		{"mayor dir with GT_ROLE=mayor", filepath.Join(townRoot, "mayor"), "mayor", RoleMayor, "env", false},
		// Control: a cwd that does detect a role still flags a disagreeing env.
		{"mayor dir with GT_ROLE=crew", filepath.Join(townRoot, "mayor"), "gastown/crew/max", RoleCrew, "env", true},
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
