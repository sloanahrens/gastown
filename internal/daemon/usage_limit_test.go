package daemon

import (
	"testing"
)

func TestIsClaudeUsageLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		input  string
		expect bool
	}{
		{
			name:   "empty input is not a usage limit",
			input:  "",
			expect: false,
		},
		{
			name:   "primary Claude usage limit message",
			input:  "Claude usage limit reached. Resets at 7pm.",
			expect: true,
		},
		{
			name:   "Claude AI usage limit variant",
			input:  "Claude AI usage limit reached",
			expect: true,
		},
		{
			name:   "TUI hit-your-limit message",
			input:  "You've hit your weekly limit · resets 7pm (America/Los_Angeles)",
			expect: true,
		},
		{
			name:   "TUI option to wait for reset",
			input:  "  > Stop and wait for limit to reset\n",
			expect: true,
		},
		{
			name:   "API error rate limit",
			input:  "API Error: Rate limit reached for organization",
			expect: true,
		},
		{
			name:   "HTTP 429 too many requests",
			input:  "request failed: 429 Too Many Requests",
			expect: true,
		},
		{
			name:   "JSON rate_limit_error type",
			input:  `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
			expect: true,
		},
		{
			name:   "regular crash with stack trace is not a usage limit",
			input:  "panic: runtime error: invalid memory address\ngoroutine 1 [running]",
			expect: false,
		},
		{
			name:   "tmux pane content with normal output is not a usage limit",
			input:  "✓ Patrol cycle complete\n> awaiting next signal",
			expect: false,
		},
		{
			name:   "EOF / pipe closed is not a usage limit",
			input:  "read /dev/stdin: i/o timeout",
			expect: false,
		},
		{
			name:   "agent comment mentioning rate limit is NOT classified (conservative)",
			input:  "// TODO: handle rate limit responses better",
			expect: false,
		},
		{
			name:   "case-insensitive primary message",
			input:  "CLAUDE USAGE LIMIT REACHED",
			expect: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsClaudeUsageLimit(tc.input)
			if got != tc.expect {
				t.Fatalf("IsClaudeUsageLimit(%q) = %v, want %v", tc.input, got, tc.expect)
			}
		})
	}
}
