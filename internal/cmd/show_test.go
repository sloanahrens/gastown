package cmd

import "testing"

func TestExtractBeadIDFromArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"simple", []string{"myproject-abc"}, "myproject-abc"},
		{"with flags after", []string{"gt-abc123", "--json"}, "gt-abc123"},
		{"with flags before", []string{"--json", "hq-xyz"}, "hq-xyz"},
		{"with id flag equals", []string{"--json", "--id=gt-abc123"}, "gt-abc123"},
		{"with id flag value", []string{"--id", "hq-xyz", "--json"}, "hq-xyz"},
		{"positional before id flag value", []string{"gt-abc123", "--id", "hq-xyz"}, "gt-abc123"},
		{"positional before id flag equals", []string{"gt-abc123", "--id=hq-xyz"}, "gt-abc123"},
		{"positional after id flag value", []string{"--id", "hq-xyz", "gt-abc123"}, "gt-abc123"},
		{"positional after id flag equals", []string{"--id=hq-xyz", "gt-abc123"}, "gt-abc123"},
		{"flag-like id fallback", []string{"--id=--gt-abc123"}, "--gt-abc123"},
		{"with as-of before id", []string{"--as-of", "main", "gt-abc123"}, "gt-abc123"},
		{"with directory before id", []string{"-C", "/tmp/work", "gt-abc123"}, "gt-abc123"},
		{"with format before id", []string{"--format", "json", "gt-abc123"}, "gt-abc123"},
		{"with end of flags marker", []string{"--", "gt-abc123"}, "gt-abc123"},
		{"flags only", []string{"--json", "-v"}, ""},
		{"value flag only", []string{"--as-of", "main"}, ""},
		{"empty", []string{}, ""},
		{"mixed", []string{"-v", "bd-def456", "--json"}, "bd-def456"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBeadIDFromArgs(tc.args)
			if got != tc.want {
				t.Errorf("extractBeadIDFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
