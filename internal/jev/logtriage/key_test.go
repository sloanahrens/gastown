package logtriage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The key collapses a line's times, bead ids, hashes, durations and numbers,
// and leaves the words around them alone.
func TestJevLogtriagePatternKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, line, want string
	}{
		{
			name: "daemon timestamp, count and duration",
			line: "2026/10/21 03:31:34 jsonl_git_backup: events: exported 2786 rows in 306ms",
			want: "<time> jsonl_git_backup: events: exported <n> rows in <dur>",
		},
		{
			name: "bead id, commit hash and seconds",
			line: "2026/10/21 03:32:01 landing_worker: gt-rpdhw: merge failed at 77b9028a after 3s",
			want: "<time> landing_worker: <id>: merge failed at <hash> after <dur>",
		},
		{
			name: "ISO time and a wisp id",
			line: "polecat gt-wisp-cguzc6 hook 2026-10-21T03:45:00Z stale",
			want: "polecat <id> hook <time> stale",
		},
		{
			name: "a patrol name keeps its words",
			line: "jev-drift: asked 1 beads, 0 failed",
			want: "jev-drift: asked <n> beads, <n> failed",
		},
		{
			name: "a word of hex letters keeps its letters",
			line: "effaced the record of 1h30m0s",
			want: "effaced the record of <dur>",
		},
		{
			name: "a long run of digits is a number, not a hash",
			line: "deploy ran 1729500000 times",
			want: "deploy ran <n> times",
		},
		{
			name: "a hash of digits and letters is a hash",
			line: "commit 4096e2f9 landed",
			want: "commit <hash> landed",
		},
		{
			name: "surrounding space is trimmed, inner spacing kept",
			line: "  padded  line  ",
			want: "padded  line",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PatternKey(tc.line); got != tc.want {
				t.Errorf("PatternKey(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// Lines that differ only in their values share one key, which is what lets a
// group count them together.
func TestJevLogtriagePatternKeyCollapsesValues(t *testing.T) {
	t.Parallel()
	a := PatternKey("2026/10/21 03:32:01 landing_worker: gt-rpdhw: merge failed at 77b9028a after 3s")
	b := PatternKey("2026/10/21 03:33:12 landing_worker: gt-4mtmy: merge failed at 34cd71ae after 14s")
	if a != b {
		t.Errorf("keys differ:\n%q\n%q", a, b)
	}
}

// A key is capped, so one long line cannot bloat every log line it heads.
func TestJevLogtriagePatternKeyIsCapped(t *testing.T) {
	t.Parallel()
	got := PatternKey("error: " + strings.Repeat("x", 500))
	if n := utf8.RuneCountInString(got); n != maxKeyRunes+1 {
		t.Errorf("key has %d runes, want %d (the cap and an ellipsis)", n, maxKeyRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a capped key does not end in an ellipsis: %q", got[len(got)-5:])
	}
}
