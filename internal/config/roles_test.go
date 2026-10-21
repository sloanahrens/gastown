package config

import (
	"testing"
	"time"
)

func TestLoadBuiltinRoleDefinition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		role        string
		wantScope   string
		wantPattern string
		wantPreSync bool
	}{
		{
			name:        "polecat",
			role:        "polecat",
			wantScope:   "rig",
			wantPattern: "{prefix}-{name}",
			wantPreSync: false,
		},
		{
			name:        "crew",
			role:        "crew",
			wantScope:   "rig",
			wantPattern: "{prefix}-crew-{name}",
			wantPreSync: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, err := loadBuiltinRoleDefinition(tt.role)
			if err != nil {
				t.Fatalf("loadBuiltinRoleDefinition(%s) error: %v", tt.role, err)
			}

			if def.Role != tt.role {
				t.Errorf("Role = %q, want %q", def.Role, tt.role)
			}
			if def.Scope != tt.wantScope {
				t.Errorf("Scope = %q, want %q", def.Scope, tt.wantScope)
			}
			if def.Session.Pattern != tt.wantPattern {
				t.Errorf("Session.Pattern = %q, want %q", def.Session.Pattern, tt.wantPattern)
			}
			if def.Session.NeedsPreSync != tt.wantPreSync {
				t.Errorf("Session.NeedsPreSync = %v, want %v", def.Session.NeedsPreSync, tt.wantPreSync)
			}

			// Verify health config has reasonable defaults
			if def.Health.PingTimeout.Duration == 0 {
				t.Error("Health.PingTimeout should not be zero")
			}
			if def.Health.ConsecutiveFailures == 0 {
				t.Error("Health.ConsecutiveFailures should not be zero")
			}
		})
	}
}

func TestLoadBuiltinRoleDefinition_UnknownRole(t *testing.T) {
	t.Parallel()
	_, err := loadBuiltinRoleDefinition("nonexistent")
	if err == nil {
		t.Error("expected error for unknown role, got nil")
	}
}

func TestAllRoles(t *testing.T) {
	t.Parallel()
	roles := AllRoles()
	if len(roles) != 2 {
		t.Errorf("AllRoles() returned %d roles, want 2", len(roles))
	}

	expected := map[string]bool{
		"polecat": true,
		"crew":    true,
	}

	for _, r := range roles {
		if !expected[r] {
			t.Errorf("unexpected role %q in AllRoles()", r)
		}
	}
}

func TestTownRoles(t *testing.T) {
	t.Parallel()
	roles := TownRoles()
	if len(roles) != 0 {
		t.Errorf("TownRoles() returned %d roles, want 0 (no town-scoped roles remain)", len(roles))
	}

	for _, r := range roles {
		def, err := loadBuiltinRoleDefinition(r)
		if err != nil {
			t.Fatalf("loadBuiltinRoleDefinition(%s) error: %v", r, err)
		}
		if def.Scope != "town" {
			t.Errorf("role %s has scope %q, expected 'town'", r, def.Scope)
		}
	}
}

func TestRigRoles(t *testing.T) {
	t.Parallel()
	roles := RigRoles()
	if len(roles) != 2 {
		t.Errorf("RigRoles() returned %d roles, want 2", len(roles))
	}

	for _, r := range roles {
		def, err := loadBuiltinRoleDefinition(r)
		if err != nil {
			t.Fatalf("loadBuiltinRoleDefinition(%s) error: %v", r, err)
		}
		if def.Scope != "rig" {
			t.Errorf("role %s has scope %q, expected 'rig'", r, def.Scope)
		}
	}
}

func TestExpandPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern  string
		town     string
		rig      string
		name     string
		role     string
		prefix   string
		expected string
	}{
		{
			pattern:  "{town}",
			town:     "/home/user/gt",
			expected: "/home/user/gt",
		},
		{
			pattern:  "{prefix}-witness",
			rig:      "gastown",
			prefix:   "gt",
			expected: "gt-witness",
		},
		{
			pattern:  "{town}/{rig}/crew/{name}",
			town:     "/home/user/gt",
			rig:      "gastown",
			name:     "max",
			expected: "/home/user/gt/gastown/crew/max",
		},
		{
			pattern:  "{prefix}-{name}",
			rig:      "gastown",
			name:     "toast",
			prefix:   "gt",
			expected: "gt-toast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := ExpandPattern(tt.pattern, tt.town, tt.rig, tt.name, tt.role, tt.prefix)
			if got != tt.expected {
				t.Errorf("ExpandPattern() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestDuration_UnmarshalText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"1h30m", time.Hour + 30*time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var d Duration
			if err := d.UnmarshalText([]byte(tt.input)); err != nil {
				t.Fatalf("UnmarshalText() error: %v", err)
			}
			if d.Duration != tt.expected {
				t.Errorf("Duration = %v, want %v", d.Duration, tt.expected)
			}
		})
	}
}
