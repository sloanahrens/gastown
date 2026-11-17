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
		"A budget stated without the word: at most 80 added lines in 2 files.",
		"Changes: at most 80 added lines in 2 files.",
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
