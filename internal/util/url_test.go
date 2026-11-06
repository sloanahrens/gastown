package util

import "testing"

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "https://example.com/org/repo", "https://example.com/org/repo"},
		{"user info", "https://user:pass@example.com/org/repo", "https://example.com/org/repo"},
		{"token auth", "https://x-access-token:ghp_abc123@example.com/org/repo.git", "https://example.com/org/repo.git"},
		{"user only", "https://user@example.com/org/repo", "https://example.com/org/repo"},
		{"ssh url", "git@example.com:org/repo.git", "git@example.com:org/repo.git"},
		{"empty string", "", ""},
		{"invalid url no creds", "://bad", "://bad"},
		{"invalid url with creds", "://user:pass@host", "<invalid URL>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.in)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
