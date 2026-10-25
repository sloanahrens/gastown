package cmd

import (
	"strings"
	"testing"
)

func TestIsValidGroupName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"ops-team", true},
		{"all_witnesses", true},
		{"team123", true},
		{"a", true},
		{"abc", true},
		{"my-cool-group", true},

		// Invalid
		{"", false},
		{"with spaces", false},
		{"with.dots", false},
		{"@team", false},
		{"group/name", false},
		{"team!", false},
		{"A", false},
		{"Ops-Team", false},
		{"-x", false},
		{"_x", false},
		{strings.Repeat("a", 65), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidGroupName(tt.name); got != tt.want {
				t.Errorf("isValidGroupName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsValidMemberPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		want    bool
	}{
		// Direct addresses
		{"gastown/crew/max", true},
		{"mayor/", true},
		{"deacon/", true},
		{"gastown/witness", true},

		// Wildcard patterns
		{"*/witness", true},
		{"gastown/*", true},
		{"gastown/crew/*", true},

		// Special patterns
		{"@town", true},
		{"@crew", true},
		{"@witnesses", true},
		{"@rig/gastown", true},

		// Group names
		{"ops-team", true},
		{"all_witnesses", true},

		// Invalid
		{"", false},
		{"@", false},
		{"Ops-Team", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := isValidMemberPattern(tt.pattern); got != tt.want {
				t.Errorf("isValidMemberPattern(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

// TestMemberPatternError: a rejected bare name says it is a group, queue, or
// channel name and quotes the rule it broke, so the uppercase rejection is
// explained rather than repeated as the token (gt-0w11c).
func TestMemberPatternError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		want    []string // nil means the pattern is valid
	}{
		{"ops-team", nil},
		{"gastown/crew/max", nil},
		{"@town", nil},
		{"", []string{"must not be empty"}},
		{"@", []string{"name after @"}},
		{"gastown//max", []string{"empty segment"}},
		{"Ops-Team", []string{"group, queue, or channel", "lowercase alphanumeric"}},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			err := memberPatternError(tt.pattern)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("memberPatternError(%q) = %v, want nil", tt.pattern, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("memberPatternError(%q) = nil, want an error", tt.pattern)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("memberPatternError(%q) = %q, want it to contain %q", tt.pattern, err, want)
				}
			}
		})
	}
}
