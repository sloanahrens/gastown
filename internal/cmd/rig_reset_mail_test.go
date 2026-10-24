package cmd

import "testing"

// TestResetMailIdentity: a reset clears the mail addressed to the role it
// resets, under the identity mail is assigned to. The --role flag and the
// cwd-detected role both resolve through resetMailRole and resetMailIdentity.
func TestResetMailIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		roleFlag string
		detected RoleInfo
		want     string
		wantErr  bool
	}{
		{
			name:     "bare --role takes the rig the reset runs in",
			roleFlag: "witness",
			detected: RoleInfo{Rig: "gastown"},
			want:     "gastown/witness",
		},
		{
			name:     "full --role address is kept as given",
			roleFlag: "gastown/witness",
			detected: RoleInfo{Rig: "other"},
			want:     "gastown/witness",
		},
		{
			name:     "--role polecat address normalizes to the polecat's identity",
			roleFlag: "gastown/polecats/goose",
			want:     "gastown/goose",
		},
		{
			name:     "--role crew address normalizes to the crew member's identity",
			roleFlag: "gastown/crew/max",
			want:     "gastown/max",
		},
		{
			name:     "bare polecat --role has no name to address",
			roleFlag: "polecat",
			detected: RoleInfo{Rig: "gastown"},
			wantErr:  true,
		},
		{
			name:     "auto-detected witness takes its rig",
			detected: RoleInfo{Role: "witness", Rig: "gastown"},
			want:     "gastown/witness",
		},
		{
			name:     "auto-detected polecat takes its rig and name",
			detected: RoleInfo{Role: RolePolecat, Rig: "gastown", Polecat: "goose"},
			want:     "gastown/goose",
		},
		{
			name:     "auto-detected town-level role is addressed bare",
			detected: RoleInfo{Role: "mayor"},
			want:     "mayor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resetMailIdentity(resetMailRole(tt.roleFlag, tt.detected))
			if (err != nil) != tt.wantErr {
				t.Fatalf("resetMailIdentity error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("resetMailIdentity = %q, want %q", got, tt.want)
			}
		})
	}
}
