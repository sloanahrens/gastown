package land

import (
	"maps"
	"strings"
	"testing"
)

func TestParseExemptions(t *testing.T) {
	t.Parallel()
	good := map[string]map[string]string{
		"internal/land TestScan # reads every file":  {"internal/land TestScan": "reads every file"},
		"internal/land\tTestScan # reads every file": {"internal/land TestScan": "reads every file"},
		"# header\n\ninternal/land TestScan # reads every file\n \n# note\n": {
			"internal/land TestScan": "reads every file",
		},
	}
	for text, want := range good {
		got, err := parseExemptions(text)
		if err != nil {
			t.Errorf("parseExemptions(%q) err = %v, want nil", text, err)
		} else if !maps.Equal(got, want) {
			t.Errorf("parseExemptions(%q) = %v, want %v", text, got, want)
		}
	}
	bad := map[string]string{
		"internal/land TestScan":                                 "internal/land TestScan",
		"internal/land TestScan #  ":                             "internal/land TestScan",
		"internal/land TestScan # a\ninternal/land TestScan # b": "internal/land TestScan",
	}
	for text, want := range bad {
		if _, err := parseExemptions(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseExemptions(%q) err = %v, want an error naming %q", text, err, want)
		}
	}
}
