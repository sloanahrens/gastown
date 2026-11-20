package cmd

import (
	"strings"
	"testing"
	"time"
)

// TestDoltLogStaleNotice pins the one line the log commands print when the
// host dolt.log has not been written since the Dolt server moved into its
// container (gt-3ktm5u): a file older than doltLogStaleAfter names its age and
// the command that reads the container's own log; a fresh file names nothing.
func TestDoltLogStaleNotice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		modTime   time.Time
		container string
		want      []string // substrings the notice must hold; empty wants no notice
		refuse    string   // substring the notice must not hold
	}{
		{
			name:      "fifteen days stale names the age and the container log command",
			modTime:   now.Add(-15 * 24 * time.Hour),
			container: "gt-dolt",
			want:      []string{"15 days ago", "docker logs gt-dolt | grep 'serialization failure'"},
		},
		{
			name:      "three hours stale names hours",
			modTime:   now.Add(-3 * time.Hour),
			container: "gt-dolt",
			want:      []string{"3 hours ago", "docker logs gt-dolt"},
		},
		{
			name:      "five minutes fresh prints no notice",
			modTime:   now.Add(-5 * time.Minute),
			container: "gt-dolt",
		},
		{
			name:      "a future timestamp prints no notice",
			modTime:   now.Add(time.Minute),
			container: "gt-dolt",
		},
		{
			name:      "no container configured names the setting, not a broken command",
			modTime:   now.Add(-15 * 24 * time.Hour),
			container: "",
			want:      []string{"15 days ago", "dolt.external_container"},
			refuse:    "docker logs ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := doltLogStaleNotice(tc.modTime, now, tc.container)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("notice = %q, want none", got)
				}
				return
			}
			if strings.Contains(got, "\n") {
				t.Errorf("notice is not one line: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q does not name %q", got, want)
				}
			}
			if tc.refuse != "" && strings.Contains(got, tc.refuse) {
				t.Errorf("notice %q names %q", got, tc.refuse)
			}
		})
	}
}
