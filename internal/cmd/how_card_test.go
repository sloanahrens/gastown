package cmd

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/taskmap"
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

// TestHowCardContent drives the card over the real map and the embedded rule
// file: the title line and the lookup rule come first, every group appears with
// its one-line description, every rule line appears, and the whole card is
// under the size limit.
func TestHowCardContent(t *testing.T) {
	t.Parallel()

	card, err := howCardText(howDefault{taskmap.Default()})
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(card, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("howCardText returned %d lines, want at least the title and the lookup rule", len(lines))
	}
	if lines[0] != overseerCardTitle {
		t.Errorf("the card's first line = %q, want the title %q", lines[0], overseerCardTitle)
	}
	if lines[1] != overseerCardLookupRule {
		t.Errorf("the card's second line = %q, want the lookup rule %q", lines[1], overseerCardLookupRule)
	}

	have := make(map[string]bool, len(lines))
	for _, line := range lines {
		have[line] = true
	}

	groups := taskmap.Groups()
	width := 0
	for _, g := range groups {
		if n := len(g); n > width {
			width = n
		}
	}
	for _, g := range groups {
		want := fmt.Sprintf("%-*s  %s", width, g, taskmap.Describe(g))
		if !have[want] {
			t.Errorf("the card does not carry group line %q", want)
		}
	}

	rules, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	for i, rule := range rules {
		if !have[rule] {
			t.Errorf("the card does not carry rule line %d %q", i+1, rule)
		}
	}

	if n := len(card); n >= overseerCardPrintMaxBytes {
		t.Errorf("the card is %d bytes, not under %d", n, overseerCardPrintMaxBytes)
	}
}

// TestHowCardCommandRunsTheCard drives runHowCmd with --card set, so deleting
// the flag branch in runHowCmd fails here even though runHowCard still builds a
// card. Not parallel: --card is a package-level flag, and a listing test
// running beside it would print a card instead of its listing.
//
//testpolicy:allow parallel — sets the package-level --card flag, which a listing test running mid-run would read
func TestHowCardCommandRunsTheCard(t *testing.T) {
	if err := howCmd.Flags().Set("card", "true"); err != nil {
		t.Fatalf("set --card: %v", err)
	}
	t.Cleanup(func() {
		if err := howCmd.Flags().Set("card", "false"); err != nil {
			t.Errorf("reset --card: %v", err)
		}
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runHowCmd(cmd, nil); err != nil {
		t.Fatalf("runHowCmd with --card: %v", err)
	}

	want, err := howCardText(howDefault{taskmap.Default()})
	if err != nil {
		t.Fatalf("howCardText: %v", err)
	}
	if out.String() != want {
		t.Errorf("gt how --card wrote %q, want the card %q", out.String(), want)
	}
	if !strings.HasPrefix(out.String(), overseerCardTitle+"\n") {
		t.Errorf("gt how --card wrote %q, want it to start with the title line", out.String())
	}
}

// TestHowCardRefusesAnArgument checks that --card refuses a group or word
// rather than printing the card as if the argument had chosen part of it.
func TestHowCardRefusesAnArgument(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runHowCard(&out, howFixture(), []string{"roll"})
	if err == nil {
		t.Fatal("runHowCard accepted an argument, want an error")
	}
	if !strings.Contains(err.Error(), "roll") {
		t.Errorf("runHowCard error %q does not name the argument", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardRefusesACardOverTheLimit checks the size bound: a card over
// overseerCardPrintMaxBytes is an error naming the size, and no partial card
// reaches the writer.
func TestHowCardRefusesACardOverTheLimit(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.groups = taskmap.Groups()
	long := strings.Repeat("x", 250)
	m.describe = func(taskmap.Group) string { return long }

	var out bytes.Buffer
	err := runHowCard(&out, m, nil)
	if err == nil {
		t.Fatal("runHowCard printed a card over the limit, want an error")
	}
	for _, want := range []string{fmt.Sprint(overseerCardPrintMaxBytes), "not under"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runHowCard error %q does not name %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardRefusesACardAtTheLimit pins the bound's edge: a card of exactly
// overseerCardPrintMaxBytes is refused, and one byte under it is printed and
// measured, so "under" keeps its strict meaning.
func TestHowCardRefusesACardAtTheLimit(t *testing.T) {
	t.Parallel()

	rules, err := overseerCardRules()
	if err != nil {
		t.Fatalf("overseerCardRules: %v", err)
	}
	// base is the card's size with a one-character group description: the title
	// and lookup rule lines, a blank line, the one group's line, a blank line,
	// then every rule line.
	const group, placeholder = "g", "x"
	base := len(overseerCardTitle) + 1 + len(overseerCardLookupRule) + 1 + 1 +
		len(group) + 2 + len(placeholder) + 1 + 1
	for _, rule := range rules {
		base += len(rule) + 1
	}

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "one byte under the limit is printed", size: overseerCardPrintMaxBytes - 1},
		{name: "exactly at the limit is refused", size: overseerCardPrintMaxBytes, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := howFixture()
			m.groups = []taskmap.Group{group}
			desc := strings.Repeat("y", tc.size-base+1)
			m.describe = func(taskmap.Group) string { return desc }

			var out bytes.Buffer
			err := runHowCard(&out, m, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("runHowCard printed a %d-byte card, want an error", tc.size)
				}
				if out.Len() != 0 {
					t.Errorf("runHowCard wrote %q before refusing", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("runHowCard refused a %d-byte card: %v", tc.size, err)
			}
			if got := out.Len(); got != tc.size {
				t.Errorf("the card is %d bytes, want %d", got, tc.size)
			}
		})
	}
}

// TestHowCardRefusesAGroupWithNoDescription checks the card refuses a group
// description of "" (writeHowGroups' error) rather than print a group line with
// nothing after the name, and that no partial card reaches the writer.
func TestHowCardRefusesAGroupWithNoDescription(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.describe = func(taskmap.Group) string { return "" }

	var out bytes.Buffer
	err := runHowCard(&out, m, nil)
	if err == nil {
		t.Fatal("runHowCard printed a card for a group with no description, want an error")
	}
	if !strings.Contains(err.Error(), "no description") {
		t.Errorf("runHowCard error %q does not name the missing description", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHowCard wrote %q before refusing", out.String())
	}
}

// TestHowCardIsNotARoleDirective checks the card reaches an agent through gt
// how --card only: the repo's directives/ directory, which gt directive list
// scans, carries no card file, and every file it does carry reaches a role. The
// repo root is scanned because its directives/ directory is the town layer.
func TestHowCardIsNotARoleDirective(t *testing.T) {
	t.Parallel()

	files, err := listableDirectiveFiles(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("listableDirectiveFiles: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "overseer-card") {
			t.Errorf("gt directive list reports the card as a directive: %s", f.Path)
		}
		if f.Unused() {
			t.Errorf("gt directive list reports %s as reaching no role", f.Path)
		}
	}
}
