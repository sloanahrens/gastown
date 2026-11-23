package specdispatch

import (
	"strconv"
	"strings"
	"testing"
)

// The optional parts of the grammar and the case of its words change neither
// the numbers nor whether the line is found: the full line, the line without
// the files clause and the line without the full stop all parse. A bead's prose
// is markdown, so a bulleted or bolded budget line is the same line.
func TestRunawayBeadParsesTheChangeBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size string
		want ChangeBudget
	}{
		{"the full line", "Change: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80, Files: 2}},
		{"without the files clause", "Change: at most 80 added lines.", ChangeBudget{Lines: 80}},
		{"without the full stop", "Change: at most 80 added lines in 2 files", ChangeBudget{Lines: 80, Files: 2}},
		{"without either", "Change: at most 80 added lines", ChangeBudget{Lines: 80}},
		{"the words in capitals", "CHANGE: AT MOST 80 ADDED LINES IN 2 FILES.", ChangeBudget{Lines: 80, Files: 2}},
		{"the words in mixed case", "change: At Most 80 Added Lines In 2 Files", ChangeBudget{Lines: 80, Files: 2}},
		{"one line and one file", "Change: at most 1 added lines in 1 files.", ChangeBudget{Lines: 1, Files: 1}},
		{"padded with spaces", "   Change:   at most 80 added lines   in 2 files.   ", ChangeBudget{Lines: 80, Files: 2}},
		{
			"among the Size prose",
			"One worker, one landing.\n\nChange: at most 80 added lines in 2 files.\n\nSplits into one MR.",
			ChangeBudget{Lines: 80, Files: 2},
		},
		{"a dashed bullet", "- Change: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80, Files: 2}},
		{"a starred bullet", "* Change: at most 80 added lines.", ChangeBudget{Lines: 80}},
		{"a plus bullet", "+ Change: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80, Files: 2}},
		{"a bolded label", "**Change:** at most 80 added lines in 2 files.", ChangeBudget{Lines: 80, Files: 2}},
		{"a bolded word, plain colon", "**Change**: at most 80 added lines.", ChangeBudget{Lines: 80}},
		{"bold around the whole line", "**Change: at most 80 added lines in 2 files.**", ChangeBudget{Lines: 80, Files: 2}},
		{"an underscored label", "__Change:__ at most 80 added lines.", ChangeBudget{Lines: 80}},
		{
			"a bulleted bold line among the Size prose",
			"One worker, one landing.\n\n- **Change:** at most 80 added lines in 2 files.\n\nSplits into one MR.",
			ChangeBudget{Lines: 80, Files: 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := ParseChangeBudget(tc.size)
			if err != nil {
				t.Fatalf("ParseChangeBudget(%q): %v", tc.size, err)
			}
			if !found {
				t.Fatalf("ParseChangeBudget(%q): found = false, want true", tc.size)
			}
			if got != tc.want {
				t.Errorf("ParseChangeBudget(%q) = %+v, want %+v", tc.size, got, tc.want)
			}
		})
	}
}

// A Size section that states no budget is not an error: the bead simply has no
// change budget line.
func TestRunawayBeadSizeWithoutChangeLine(t *testing.T) {
	t.Parallel()

	for _, size := range []string{
		"",
		"One worker, one landing.",
		"Two workers, two landings.\nEach takes half the files.",
		"- One worker, one landing.",
		"* Two workers, two landings.",
	} {
		got, found, err := ParseChangeBudget(size)
		if err != nil {
			t.Errorf("ParseChangeBudget(%q): %v", size, err)
			continue
		}
		if found {
			t.Errorf("ParseChangeBudget(%q): found = true, want false", size)
		}
		if got != (ChangeBudget{}) {
			t.Errorf("ParseChangeBudget(%q) = %+v, want the zero budget", size, got)
		}
	}
}

// A line that starts "Change:" and does not match the grammar is refused with
// the grammar quoted, so the author sees what is expected. A count of zero is
// not a positive integer, and a count too large for an int is not one either.
func TestRunawayBeadMalformedChangeLineRefuses(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"Change:",
		"Change: at most eighty added lines in 2 files.",
		"Change: at most 80 lines in 2 files.",
		"Change: at most 0 added lines in 2 files.",
		"Change: at most 80 added lines in 0 files.",
		"Change: at most 80 added lines in 2 file.",
		"Change: at most -5 added lines in 2 files.",
		"Change: at most 80 added lines in 2 files, and no more.",
		"Change: at most 99999999999999999999 added lines.",
		"- Change: at most eighty added lines in 2 files.",
		"**Change:** at most 80 lines in 2 files.",
	} {
		size := "One worker, one landing.\n" + line
		got, found, err := ParseChangeBudget(size)
		if err == nil {
			t.Errorf("ParseChangeBudget(%q): err = nil, want the grammar refusal", size)
			continue
		}
		if found {
			t.Errorf("ParseChangeBudget(%q): found = true with an error", size)
		}
		if got != (ChangeBudget{}) {
			t.Errorf("ParseChangeBudget(%q) = %+v with an error, want the zero budget", size, got)
		}
		if !strings.Contains(err.Error(), changeBudgetGrammar) {
			t.Errorf("error %q does not quote the grammar %q", err, changeBudgetGrammar)
		}
		if !strings.Contains(err.Error(), strconv.Quote(line)) {
			t.Errorf("error %q does not quote the line %q", err, line)
		}
	}
}

// The first line that matches answers, and a later line that does not is still
// refused: two statements of a budget never merge into one.
func TestRunawayBeadReadsEveryChangeLine(t *testing.T) {
	t.Parallel()

	got, found, err := ParseChangeBudget("Change: at most 90 added lines in 3 files.\nChange: at most 10 added lines in 1 files.")
	if err != nil {
		t.Fatalf("ParseChangeBudget: %v", err)
	}
	if !found {
		t.Fatal("ParseChangeBudget: found = false, want true")
	}
	if want := (ChangeBudget{Lines: 90, Files: 3}); got != want {
		t.Errorf("ParseChangeBudget = %+v, want the first line's %+v", got, want)
	}

	if _, _, err := ParseChangeBudget("Change: at most 90 added lines in 3 files.\nChange: as soon as possible."); err == nil {
		t.Error("ParseChangeBudget with a malformed second line: err = nil, want the grammar refusal")
	}
}

// A Size section with no line starting "Change:" is read through the declared-
// line grammar the done gate refuses on (gt-hc43nx): a same-line budget, a
// lead-in and a sentence inside the prose all declare, the smallest number is
// the budget, and Files stays 0. A proper Change line still wins, and a
// malformed one is still refused.
func TestParseChangeBudgetReadsTheGateForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size string
		want ChangeBudget
	}{
		{"a same-line budget", "One worker, one landing. Change: at most 60 added lines in 2 files.", ChangeBudget{Lines: 60}},
		{"a lead-in", "About 40 added lines", ChangeBudget{Lines: 40}},
		{"a trailing clause", "At most 20 added lines (deleted lines do not count).", ChangeBudget{Lines: 20}},
		{"the cap", "the cap: 40 added lines", ChangeBudget{Lines: 40}},
		{"the budget sentence", "the budget is 300 added lines", ChangeBudget{Lines: 300}},
		{"the hyphenated form", "a 70-added-line constraint", ChangeBudget{Lines: 70}},
		{"a budget without the Change: word", "A budget stated without the word: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80}},
		{"a near-miss prefix", "Changes: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80}},
		{"the smallest of several", "About 90 added lines, and a 70-added-line constraint.", ChangeBudget{Lines: 70}},
		{"among the Size prose", "One worker, one landing.\nIt keeps to a 70-added-line constraint.\nSplits into one MR.", ChangeBudget{Lines: 70}},
		{"a Change line wins over the prose", "About 5 added lines.\nChange: at most 80 added lines in 2 files.", ChangeBudget{Lines: 80, Files: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := ParseChangeBudget(tc.size)
			if err != nil {
				t.Fatalf("ParseChangeBudget(%q): %v", tc.size, err)
			}
			if !found {
				t.Fatalf("ParseChangeBudget(%q): found = false, want true", tc.size)
			}
			if got != tc.want {
				t.Errorf("ParseChangeBudget(%q) = %+v, want %+v", tc.size, got, tc.want)
			}
		})
	}

	// A "Change:" line that does not match is refused even when the prose
	// states a number the gate would read: the fallback never runs.
	if _, _, err := ParseChangeBudget("About 40 added lines\nChange: as soon as possible."); err == nil {
		t.Error("a malformed Change line beside a prose number: err = nil, want the grammar refusal")
	}
}

// The lint's declared-lines advice reads the same forms the gate does
// (gt-hc43nx): a same-line budget at 9.8 minutes declaring 60 gets the band's
// advice, a declaration at the band's Advise or above gets none, and a Size
// section with no number is neither advised nor refused. The 9.8 band's p90 is
// 454, so its Advise is (454+1)/2 = 227.
func TestDeclaredLinesAdviceReadsTheGateForms(t *testing.T) {
	t.Parallel()

	const advice60 = "the Change line declares 60 added lines, but beads estimated at 9.8 minutes landed with a median of 109 (p75 216, p90 454) and the gate refuses past twice the declared number: declare at least 227, or split"

	for _, tc := range []struct {
		name   string
		size   string
		labels []string
		want   string
	}{
		{"a same-line budget", "One worker, one landing. Change: at most 60 added lines in 2 files.", []string{"sp-minutes:9.8"}, advice60},
		{"a lead-in", "One worker, one landing. About 60 added lines.", []string{"sp-minutes:9.8"}, advice60},
		{"at the band's Advise", "One worker, one landing. Change: at most 227 added lines.", []string{"sp-minutes:9.8"}, ""},
		{"above the band's Advise", "One worker, one landing. Change: at most 228 added lines.", []string{"sp-minutes:9.8"}, ""},
		{"no number", "One worker, one landing.", []string{"sp-minutes:9.8"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec(tc.size, tc.labels...)
			got := Advice(s)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("Advice() = %q, want none", got)
				}
			} else if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("Advice() = %q, want %q", got, tc.want)
			}
			if v := Lint(s, defaultTemplate()); v.Route == RouteRefuse {
				t.Fatalf("Lint() refused %q, want no refusal", v.Line(s.ID))
			}
		})
	}
}

// The lint's p25 refusal reads the same forms the gate does (gt-hc43nx): a
// same-line budget and a lead-in that declare 30 added lines at 9.8 minutes are
// both refused, naming the band's p25 (58), while a 60-line declaration in
// either form is not refused. The rows are driven through Lint so deleting the
// Size-section fallback turns them red.
func TestDeclaredLinesRefuseReadsTheGateForms(t *testing.T) {
	t.Parallel()

	const wantReason = "the Change line declares 30 added lines, below what three in four beads estimated at 9.8 minutes landed with (p25 58); declare at least 227, or split"

	for _, tc := range []struct {
		name       string
		size       string
		wantReason string
	}{
		{"a same-line 30", "One worker, one landing. Change: at most 30 added lines in 2 files.", wantReason},
		{"a lead-in 30", "One worker, one landing. About 30 added lines.", wantReason},
		{"a same-line 60", "One worker, one landing. Change: at most 60 added lines in 2 files.", ""},
		{"a lead-in 60", "One worker, one landing. About 60 added lines.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec(tc.size, "sp-minutes:9.8")
			v := Lint(s, defaultTemplate())
			got := ""
			for _, r := range v.Refusals {
				if r.Field == "size" {
					got = r.Reason
				}
			}
			if got != tc.wantReason {
				t.Fatalf("size refusal = %q, want %q (verdict %s)", got, tc.wantReason, v.Line(s.ID))
			}
			if tc.wantReason == "" && v.Route == RouteRefuse {
				t.Fatalf("Lint() refused %q, want no refusal", v.Line(s.ID))
			}
		})
	}
}
