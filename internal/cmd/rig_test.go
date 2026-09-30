package cmd

import (
	"testing"
)

func TestIsGitRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		// Remote URLs — should return true
		{"https://github.com/org/repo.git", true},
		{"http://github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"git://github.com/org/repo.git", true},
		{"deploy@private-host.internal:repos/app.git", true},

		// Custom git remote helper schemes — should return true
		{"s3://my-bucket/rigs/my-project", true},
		{"codecommit://my-repo", true},
		{"gs://my-bucket/repos/foo", true},

		// Local paths — should return false
		{"/Users/scott/projects/foo", false},
		{"/tmp/repo", false},
		{"./foo", false},
		{"../foo", false},
		{"~/projects/foo", false},
		{"C:\\Users\\scott\\projects\\foo", false},
		{"C:/Users/scott/projects/foo", false},

		// Bare directory name — should return false
		{"foo", false},

		// file:// URIs — explicit local git remotes are allowed
		{"file:///tmp/local-repo.git", true},
		{"file:///Users/scott/projects/foo", true},
		{"file://user@localhost:/tmp/local-repo.git", true},

		// Argument injection — should return false
		{"-oProxyCommand=evil", false},
		{"--upload-pack=touch /tmp/pwned", false},
		{"-c", false},

		// Malformed SCP-style — should return false
		{"@host:path", false},     // empty user
		{"user@:/path", false},    // empty host
		{"localhost:path", false}, // no user (not SCP-style)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isGitRemoteURL(tt.input)
			if got != tt.want {
				t.Errorf("isGitRemoteURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
