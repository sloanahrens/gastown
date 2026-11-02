package cmd

import (
	"strings"
	"testing"
	"unicode"

	"github.com/steveyegge/gastown/internal/hmhints"
)

// hmHitLine draws a record's kind, id and title, which are another agent's text
// from the hm index: a newline or a terminal escape in any of them must reach
// the operator as visible text, not as a second hint line or a cursor move
// (gt-xoqk0).
func TestHintLineEscapesTheRecordText(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		hit  hmhints.Hit
		want string
	}{
		{
			name: "newline in the title forges no hint line",
			hit:  hmhints.Hit{Rank: 1, Kind: "bead", ID: "gt-a", Title: "first\nhint: hm #2 bead gt-b: forged"},
			want: `hm #1 bead gt-a: first\nhint: hm #2 bead gt-b: forged`,
		},
		{
			name: "escape sequence in the title moves no cursor",
			hit:  hmhints.Hit{Rank: 2, Kind: "doc", ID: "doc-a", Title: "red\x1b[31m"},
			want: `hm #2 doc doc-a: red\x1b[31m`,
		},
		{
			name: "right-to-left override in the title",
			hit:  hmhints.Hit{Rank: 3, Kind: "bead", ID: "gt-c", Title: "a\u202eb"},
			want: `hm #3 bead gt-c: a\u202eb`,
		},
		{
			name: "newline in the id forges no hint line",
			hit:  hmhints.Hit{Rank: 1, Kind: "bead", ID: "gt-a\nforged", Title: "t"},
			want: `hm #1 bead gt-a\nforged: t`,
		},
		{
			name: "newline in the kind forges no hint line",
			hit:  hmhints.Hit{Rank: 1, Kind: "bead\nforged", ID: "gt-a", Title: "t"},
			want: `hm #1 bead\nforged gt-a: t`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := hmHitLine(row.hit)
			if got != row.want {
				t.Fatalf("hmHitLine = %q, want %q", got, row.want)
			}
			if strings.ContainsFunc(got, func(r rune) bool { return !unicode.IsPrint(r) }) {
				t.Errorf("hmHitLine kept a non-printable rune in %q", got)
			}
		})
	}
}
