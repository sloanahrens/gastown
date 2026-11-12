package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

const fullCommitTest = "0123456789abcdef0123456789abcdef01234567"

func TestFullBuildCommitReturnsTheRecordedRevision(t *testing.T) {
	t.Parallel()
	settings := []debug.BuildSetting{{Key: "vcs.revision", Value: fullCommitTest}}
	for _, stamped := range []string{"", fullCommitTest[:9], fullCommitTest} {
		got, err := recordedFullCommit(stamped, settings)
		if err != nil {
			t.Fatalf("stamp %q: fullBuildCommit: %v", stamped, err)
		}
		if got != fullCommitTest {
			t.Fatalf("stamp %q: got %q, want the full recorded sha %q", stamped, got, fullCommitTest)
		}
	}
}

func TestFullBuildCommitRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		stamped  string
		settings []debug.BuildSetting
		reason   string
	}{
		{
			name:     "no vcs.revision in the build",
			stamped:  fullCommitTest[:9],
			settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "false"}},
			reason:   "no vcs.revision",
		},
		{
			name:     "a short recorded revision",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: fullCommitTest[:9]}},
			reason:   "not a full commit sha",
		},
		{
			name:     "an uppercase recorded revision",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: strings.ToUpper(fullCommitTest)}},
			reason:   "not a full commit sha",
		},
		{
			name:     "a stamp that is not the start of the revision",
			stamped:  "deadbeef0",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: fullCommitTest}},
			reason:   "is not the start of its vcs.revision",
		},
		{
			name:     "an unknown stamp from a build with no HEAD",
			stamped:  "unknown",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: fullCommitTest}},
			reason:   "is not the start of its vcs.revision",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := recordedFullCommit(c.stamped, c.settings)
			if err == nil {
				t.Fatalf("fullBuildCommit returned %q, want a refusal", got)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("error %q does not give the reason %q", err, c.reason)
			}
		})
	}
}

func TestFullBuildCommitRefusesABinaryWithNoBuildInfo(t *testing.T) {
	t.Parallel()
	got, err := fullBuildCommit("", func() (*debug.BuildInfo, bool) { return nil, false })
	if err == nil {
		t.Fatalf("fullBuildCommit returned %q, want a refusal", got)
	}
	if !strings.Contains(err.Error(), "no build info") {
		t.Fatalf("error %q does not give the reason", err)
	}
}
