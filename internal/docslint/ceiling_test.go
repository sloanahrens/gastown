package docslint

import (
	"reflect"
	"strings"
	"testing"
)

// words is text of exactly n words.
func words(n int) string {
	return strings.Repeat("word ", n)
}

func TestWordCeilingTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   memFS
		ceiling int
		want    []string
	}{
		{
			name:    "a file at the ceiling passes",
			files:   memFS{"AGENTS.md": words(10)},
			ceiling: 10,
		},
		{
			name:    "one word over the ceiling is a finding",
			files:   memFS{"AGENTS.md": words(11)},
			ceiling: 10,
			want:    []string{"AGENTS.md:1: word-ceiling: 11 words, ceiling 10"},
		},
		{
			name:    "an exempt formula over the ceiling is not a finding",
			files:   memFS{"internal/formula/formulas/mol-polecat-work.formula.toml": words(20)},
			ceiling: 10,
		},
		{
			name:    "a formula that is not exempt over the ceiling is a finding",
			files:   memFS{"internal/formula/formulas/mol-other.formula.toml": words(20)},
			ceiling: 10,
			want:    []string{"internal/formula/formulas/mol-other.formula.toml:1: word-ceiling: 20 words, ceiling 10"},
		},
		{
			name:    "a custom ceiling moves the line",
			files:   memFS{"AGENTS.md": words(15)},
			ceiling: 20,
		},
		{
			name:    "a file outside the ceiling tier is not counted",
			files:   memFS{"docs/guide.md": words(50)},
			ceiling: 10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckWordCeiling(tc.files, tc.ceiling)
			if err != nil {
				t.Fatalf("CheckWordCeiling(): %v", err)
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, tc.want) {
				t.Errorf("CheckWordCeiling() = %q, want %q", gotStrings, tc.want)
			}
		})
	}
}

func TestWordCeilingReadErrorIsAnError(t *testing.T) {
	t.Parallel()
	files := memFS{"AGENTS.md": words(5)}
	unreadable := oneUnreadableFS{memFS: files, unreadable: "AGENTS.md"}
	got, err := CheckWordCeiling(unreadable, 10)
	if err == nil {
		t.Fatalf("CheckWordCeiling() with an unreadable file = %q, want an error, not a pass", findingStrings(got))
	}
}
