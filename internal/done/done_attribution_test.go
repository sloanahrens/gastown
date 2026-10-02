package done

import (
	"testing"
)

const claudeTrailer = "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"

func TestStripAttributionLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		message     string
		want        string
		wantRemoved int
	}{
		{
			name:        "co-authored-by trailer",
			message:     "fix: thing (gt-1)\n\nbody line\n\n" + claudeTrailer,
			want:        "fix: thing (gt-1)\n\nbody line",
			wantRemoved: 1,
		},
		{
			name:        "generated-with footer and trailer",
			message:     "fix: thing\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n" + claudeTrailer,
			want:        "fix: thing",
			wantRemoved: 2,
		},
		{
			name:        "trailer case and spacing vary",
			message:     "fix: thing\n\nco-authored-by : Someone <a@b.c>",
			want:        "fix: thing",
			wantRemoved: 1,
		},
		{
			name:        "trailer between other trailers keeps the others",
			message:     "fix: thing\n\nSigned-off-by: A <a@b.c>\n" + claudeTrailer + "\nReviewed-by: B <b@b.c>",
			want:        "fix: thing\n\nSigned-off-by: A <a@b.c>\nReviewed-by: B <b@b.c>",
			wantRemoved: 1,
		},
		{
			name:    "prose that mentions claude is not attribution",
			message: "feat: strip Claude attribution in gt done\n\nClaude Code adds a Co-Authored-By trailer; this removes it.",
		},
		{
			name:    "clean message is returned byte for byte",
			message: "fix: thing\n\n  indented body  \n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed := stripAttributionLines(tt.message)
			if len(removed) != tt.wantRemoved {
				t.Errorf("removed %d lines %q, want %d", len(removed), removed, tt.wantRemoved)
			}
			want := tt.want
			if tt.wantRemoved == 0 {
				want = tt.message
			}
			if got != want {
				t.Errorf("cleaned message = %q, want %q", got, want)
			}
		})
	}
}
