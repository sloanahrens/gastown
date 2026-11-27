package cmd

import (
	"path/filepath"
	"testing"
)

// TestDetectRoleCommonPath pins that cwd detection reads the common work path,
// <townRoot>/work, as neither a rig nor a role (gt-fc82u2). It is a link a seat
// may start in, and a path below it stays under it. The ordinary rig path is the
// control: the same detection still names its rig and polecat.
func TestDetectRoleCommonPath(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "gt")

	tests := []struct {
		name        string
		cwd         string
		wantRig     string
		wantRole    Role
		wantPolecat string
	}{
		{"common work path", filepath.Join(townRoot, "work"), "", RoleUnknown, ""},
		{"path below the common work path", filepath.Join(townRoot, "work", "gastown"), "", RoleUnknown, ""},
		{"ordinary rig polecat path", filepath.Join(townRoot, "gastown", "polecats", "slit"), "gastown", RolePolecat, "slit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectRole(tt.cwd, townRoot)
			if got.Rig != tt.wantRig || got.Role != tt.wantRole || got.Polecat != tt.wantPolecat {
				t.Errorf("detectRole(%q): got rig=%q role=%q polecat=%q, want rig=%q role=%q polecat=%q",
					tt.cwd, got.Rig, got.Role, got.Polecat, tt.wantRig, tt.wantRole, tt.wantPolecat)
			}
		})
	}
}

// TestDetectRoleCommonPathEnvFallback pins that GT_ROLE=polecat with no GT_RIG
// started in the common work path fills nothing from it, as from any cwd with no
// rig (gt-fc82u2). The town root is that reference cwd: the two must agree, and
// neither reports a rig or the incomplete-env flag.
func TestDetectRoleCommonPathEnvFallback(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "gt")
	commonPath := filepath.Join(townRoot, "work")
	env := func(k string) string {
		if k == EnvGTRole {
			return "polecat"
		}
		return ""
	}

	neutral, err := getRoleWithContextEnv(townRoot, townRoot, env)
	if err != nil {
		t.Fatalf("getRoleWithContextEnv from the town root: %v", err)
	}
	if neutral.Rig != "" || neutral.EnvIncomplete {
		t.Fatalf("control: town root with GT_ROLE=polecat got rig=%q incomplete=%v, want an empty rig and false",
			neutral.Rig, neutral.EnvIncomplete)
	}

	fromCommon, err := getRoleWithContextEnv(commonPath, townRoot, env)
	if err != nil {
		t.Fatalf("getRoleWithContextEnv from the common work path: %v", err)
	}
	if fromCommon.Role != RolePolecat || fromCommon.Source != "env" || fromCommon.Mismatch ||
		fromCommon.Rig != "" || fromCommon.Polecat != "" || fromCommon.EnvIncomplete {
		t.Errorf("from the common work path: got role=%q source=%q mismatch=%v rig=%q polecat=%q incomplete=%v, want %q/env/false/\"\"/\"\"/false",
			fromCommon.Role, fromCommon.Source, fromCommon.Mismatch, fromCommon.Rig, fromCommon.Polecat, fromCommon.EnvIncomplete,
			RolePolecat)
	}
}
