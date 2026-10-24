package cmd

import (
	"runtime/debug"
	"testing"
)

func TestResolveBranch_IgnoresCallerCwd(t *testing.T) {
	t.Parallel()
	// The test binary runs inside this repo, on whatever branch it is checked
	// out to. An unstamped build must not report that branch (gt-t2fom).
	if got := resolveBranch(""); got != "" {
		t.Errorf("resolveBranch(\"\") = %q, want empty: an unstamped build must not read the caller's checkout", got)
	}
	if got := resolveBranch("main"); got != "main" {
		t.Errorf("resolveBranch(%q) = %q, want the stamped value", "main", got)
	}
}

func TestResolveBranch_ReadsBuildInfoVCSBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name: "vcs.branch recorded",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.branch", Value: "polecat/goose/gt-t2fom"},
			},
			want: "polecat/goose/gt-t2fom",
		},
		{
			name:     "vcs.branch present but empty",
			settings: []debug.BuildSetting{{Key: "vcs.branch", Value: ""}},
			want:     "",
		},
		{
			name:     "no vcs.branch",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := branchFromBuildSettings(tt.settings); got != tt.want {
				t.Errorf("branchFromBuildSettings() = %q, want %q", got, tt.want)
			}
		})
	}
}
