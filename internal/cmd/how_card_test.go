package cmd

import (
	"strings"
	"testing"
)

// TestHowCardRulesReadTheFile drives the production loader over the embedded
// card. A blank line, a line over the limit, or a card over the byte budget in
// overseer-card.md makes this test fail with the validator's error, which names
// the file and the line or size.
func TestHowCardRulesReadTheFile(t *testing.T) {
	t.Parallel()

	lines, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("overseerCardRules returned no rule lines")
	}
}

// TestHowCardRulesPinTheFile pins the card's wording. A rule line changed in
// overseer-card.md without rewriting testdata/how/card.golden fails here, so a
// card edit is reviewed as a diff instead of slipping in through the embed.
func TestHowCardRulesPinTheFile(t *testing.T) {
	t.Parallel()

	lines, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	checkHowGolden(t, "card.golden", []byte(strings.Join(lines, "\n")+"\n"))
}

// TestHowCardRulesRefuseBadLines checks the shapes the card must never have and
// the line or size the error names for each, and that a card at each limit is
// accepted.
func TestHowCardRulesRefuseBadLines(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("x", overseerCardMaxLine)
	overLimit := strings.Repeat("x", overseerCardMaxLine+1)

	// atByteBudget is thirteen valid lines and exactly overseerCardMaxBytes, so
	// only the whole-file check decides it; overByteBudget is one byte more.
	// Twelve lines sit at the line limit, which the per-line check accepts.
	atByteBudget := strings.Repeat(atLimit+"\n", 12) + strings.Repeat("y", 88)
	overByteBudget := strings.Repeat(atLimit+"\n", 12) + strings.Repeat("y", 89)
	atByteBudgetLines := make([]string, 0, 13)
	for i := 0; i < 12; i++ {
		atByteBudgetLines = append(atByteBudgetLines, atLimit)
	}
	atByteBudgetLines = append(atByteBudgetLines, strings.Repeat("y", 88))

	tests := []struct {
		name    string
		text    string
		want    []string
		wantErr []string
	}{
		{
			name: "valid lines",
			text: "first\nsecond\n",
			want: []string{"first", "second"},
		},
		{
			name: "the last line needs no newline",
			text: "first\nsecond",
			want: []string{"first", "second"},
		},
		{
			name: "a line at the limit is accepted",
			text: atLimit,
			want: []string{atLimit},
		},
		{
			name: "a card at the byte budget is accepted",
			text: atByteBudget,
			want: atByteBudgetLines,
		},
		{
			name:    "a card over the byte budget",
			text:    overByteBudget,
			wantErr: []string{"2501 bytes", "more than the 2500-byte card budget"},
		},
		{
			name:    "an empty line",
			text:    "first\n\nthird\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "a whitespace-only line",
			text:    "first\n  \nthird\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "a blank line after the last newline",
			text:    "first\n\n",
			wantErr: []string{"line 2", "empty line"},
		},
		{
			name:    "an empty file",
			text:    "",
			wantErr: []string{"line 1", "empty line"},
		},
		{
			name:    "a line over the limit",
			text:    "first\nsecond\n" + overLimit + "\n",
			wantErr: []string{"line 3", "201 characters, more than 200"},
		},
		{
			name:    "the first bad line is the one named",
			text:    "first\n\n" + overLimit + "\n",
			wantErr: []string{"line 2", "empty line"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lines, err := parseOverseerCard(tc.text)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parseOverseerCard accepted the card and returned %q", lines)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("parseOverseerCard error %q does not name %q", err, want)
					}
				}
				if lines != nil {
					t.Errorf("parseOverseerCard returned %q beside its error", lines)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOverseerCard: %v", err)
			}
			if len(lines) != len(tc.want) {
				t.Fatalf("parseOverseerCard returned %d lines, want %d: %q", len(lines), len(tc.want), lines)
			}
			for i := range tc.want {
				if lines[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i+1, lines[i], tc.want[i])
				}
			}
		})
	}
}
