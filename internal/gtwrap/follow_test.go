package gtwrap

import "testing"

// TestIsFollowCall: only `gt attention` with -f or --follow streams, so only it gets
// the stdin watchdog; a -f that is another flag's value is not a follow.
func TestIsFollowCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{"follow", []string{"gt", "attention", "-f"}, true},
		{"long follow", []string{"gt", "attention", "--follow"}, true},
		{"follow with other flags", []string{"gt", "attention", "-f", "--wake-only"}, true},
		{"follow then unfollow", []string{"gt", "attention", "-f", "--follow=false"}, false},
		{"attention without follow", []string{"gt", "attention", "--wake-only"}, false},
		{"-f as value of since", []string{"gt", "attention", "--since", "-f"}, false},
		{"-f after double dash", []string{"gt", "attention", "--", "-f"}, false},
		{"unparseable follow value", []string{"gt", "attention", "--follow=yes"}, false},
		{"unparseable value then -f", []string{"gt", "attention", "--follow=yes", "-f"}, false},
		{"non-attention verb with -f", []string{"gt", "tail", "-f", "x.log"}, false},
		{"empty argv", []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isFollowCall(tc.argv); got != tc.want {
				t.Errorf("isFollowCall(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
