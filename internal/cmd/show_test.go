package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestRunShowWithPrintsBdTextAndReturnsItsError drives gt show's command body:
// bd's text reaches the writer and bd's failure is the command's error, so a
// failed read cannot become a silent exit 0 (gt-p85pw.10, gt-5bv77).
func TestRunShowWithPrintsBdTextAndReturnsItsError(t *testing.T) {
	t.Parallel()
	bdErr := errors.New("bd show gt-x: exit status 1: no issue found")

	var out strings.Builder
	var gotID string
	var gotArgs []string
	show := func(id string, args []string) (string, error) {
		gotID, gotArgs = id, args
		return "◇ gt-x · title\n", bdErr
	}

	err := runShowWith(&out, []string{"--json", "gt-x"}, show)
	if !errors.Is(err, bdErr) {
		t.Errorf("runShowWith err = %v, want bd's error", err)
	}
	if want := "◇ gt-x · title\n"; out.String() != want {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
	if gotID != "gt-x" {
		t.Errorf("show id = %q, want the positional ID", gotID)
	}
	if !slices.Equal(gotArgs, []string{"--json", "gt-x"}) {
		t.Errorf("show args = %v, want the caller's argv", gotArgs)
	}
}

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
