package beads

import (
	"errors"
	"testing"
)

// TestAcceptanceTickBoxFlipsOnlyTheNamedLine: the box is found by its text, so
// the line is flipped wherever it sits, and every other byte of the block
// comes back as it went in (CRLF line ends and nested or numbered items too).
func TestAcceptanceTickBoxFlipsOnlyTheNamedLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		block string
		box   string
		want  string
	}{
		{
			name:  "first line",
			block: "- [ ] one\n- [ ] two",
			box:   "one",
			want:  "- [x] one\n- [ ] two",
		},
		{
			name:  "whole item line names the same box",
			block: "- [ ] one\n- [ ] two",
			box:   "- [ ] two",
			want:  "- [ ] one\n- [x] two",
		},
		{
			name:  "nested and numbered markers",
			block: "- [ ] one\n  * [ ] nested\n3) [ ] numbered",
			box:   "numbered",
			want:  "- [ ] one\n  * [ ] nested\n3) [x] numbered",
		},
		{
			name:  "carriage returns are kept",
			block: "- [ ] one\r\n- [ ] two\r\n",
			box:   "two",
			want:  "- [ ] one\r\n- [x] two\r\n",
		},
		{
			name:  "a checked twin does not stop the open box",
			block: "- [x] run the gate\n- [ ] run the gate",
			box:   "run the gate",
			want:  "- [x] run the gate\n- [x] run the gate",
		},
		{
			name:  "a later bracket in the text does not move the box",
			block: "- [ ] see [docs]",
			box:   "see [docs]",
			want:  "- [x] see [docs]",
		},
		{
			name:  "a box inside a fence is not a box",
			block: "```\n- [ ] example\n```\n- [ ] example",
			box:   "example",
			want:  "```\n- [ ] example\n```\n- [x] example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TickAcceptanceBox(tc.block, tc.box)
			if err != nil {
				t.Fatalf("TickAcceptanceBox: %v", err)
			}
			if got != tc.want {
				t.Errorf("TickAcceptanceBox = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAcceptanceTickBoxRefusesWhatItCannotName: an absent or ambiguous box is
// an error and the block is not guessed at; an already ticked box returns the
// block as it was.
func TestAcceptanceTickBoxRefusesWhatItCannotName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		block string
		box   string
		err   error
	}{
		{"absent", "- [ ] one", "two", ErrAcceptanceBoxNotFound},
		{"empty block", "", "one", ErrAcceptanceBoxNotFound},
		{"prose is not a box", "run the gate", "run the gate", ErrAcceptanceBoxNotFound},
		{"ambiguous", "- [ ] one\n- [ ] one", "one", ErrAcceptanceBoxAmbiguous},
		{"blank text", "- [ ] one", "  ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TickAcceptanceBox(tc.block, tc.box)
			if err == nil {
				t.Fatalf("TickAcceptanceBox returned %q, want an error", got)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error %v, want %v", err, tc.err)
			}
			if got != "" {
				t.Errorf("TickAcceptanceBox returned %q with the error", got)
			}
		})
	}
}

func TestAcceptanceTickBoxLeavesATickedBoxAlone(t *testing.T) {
	t.Parallel()
	block := "- [x] one\n- [ ] two"
	got, err := TickAcceptanceBox(block, "one")
	if err != nil {
		t.Fatalf("TickAcceptanceBox: %v", err)
	}
	if got != block {
		t.Errorf("TickAcceptanceBox = %q, want the block unchanged", got)
	}
}
