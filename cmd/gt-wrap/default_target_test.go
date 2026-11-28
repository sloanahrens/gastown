package main

import (
	"strings"
	"testing"
)

func TestUsesDefaultTarget(t *testing.T) {
	t.Parallel()
	const cluster = `{"target":"kubernetes","context":"ctx","namespace":"ns","deployment":"gt","container":"gt"}`
	tests := []struct {
		name, config, wantErr string
		noHome, want          bool
	}{
		{"no config file", "", "", false, true},
		{"default config", `{"target":"docker"}`, "", false, true},
		{"cluster config", cluster, "", false, false},
		{"unparseable config", `{`, "parse the target config file", false, false},
		{"no HOME", "", "HOME is not set", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tt.config != "" {
				h.writeConfig(t, tt.config)
			}
			if tt.noHome {
				h.surface.getenv = func(string) string { return "" }
			}
			got, err := usesDefaultTarget(h.surface, "gt")
			if tt.wantErr != "" {
				if err == nil || got || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("usesDefaultTarget = %v, %v; want false and an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("usesDefaultTarget = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}
