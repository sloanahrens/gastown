package polecat

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFormatGeneratedBranchName_ActionCompatible(t *testing.T) {
	t.Parallel()
	branch := FormatGeneratedBranchName("alpha", "gt-pin-bd-metadata", "mk123456")
	if strings.Contains(branch, "@") {
		t.Fatalf("FormatGeneratedBranchName() = %q, must not contain @", branch)
	}

	claudeCodeActionBranch := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.#+,-]*$`)
	if !claudeCodeActionBranch.MatchString(branch) {
		t.Fatalf("FormatGeneratedBranchName() = %q, rejected by claude-code-action branch pattern", branch)
	}
}

func TestParseBranchName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		branch        string
		wantOk        bool
		wantGenerated bool
		wantPolecat   string
		wantIssue     string
	}{
		{
			name:          "generated issue with plus suffix",
			branch:        "polecat/alpha/gt-pin-bd-metadata+mk123456",
			wantOk:        true,
			wantGenerated: true,
			wantPolecat:   "alpha",
			wantIssue:     "gt-pin-bd-metadata",
		},
		{
			name:          "generated dotted subtask with plus suffix",
			branch:        "polecat/alpha/gt-4kp9.5.5.1+mk123456",
			wantOk:        true,
			wantGenerated: true,
			wantPolecat:   "alpha",
			wantIssue:     "gt-4kp9.5.5.1",
		},
		{
			name:          "legacy generated issue with at suffix",
			branch:        "polecat/alpha/gt-jns7.1@mk123456",
			wantOk:        true,
			wantGenerated: true,
			wantPolecat:   "alpha",
			wantIssue:     "gt-jns7.1",
		},
		{
			name:        "raw dashed issue slug is not truncated",
			branch:      "polecat/alpha/gt-pin-bd-metadata",
			wantOk:      true,
			wantPolecat: "alpha",
			wantIssue:   "gt-pin-bd-metadata",
		},
		{
			name:          "no issue generated branch",
			branch:        "polecat/alpha-mk123456",
			wantOk:        true,
			wantGenerated: true,
			wantPolecat:   "alpha",
		},
		{
			name:   "empty generated suffix is invalid",
			branch: "polecat/alpha/gt-abc+",
		},
		{
			name:   "empty generated issue is invalid",
			branch: "polecat/alpha/+mk123456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseBranchName(tt.branch)
			if ok != tt.wantOk {
				t.Fatalf("ParseBranchName(%q) ok = %v, want %v", tt.branch, ok, tt.wantOk)
			}
			if !ok {
				return
			}
			if got.Generated != tt.wantGenerated {
				t.Errorf("Generated = %v, want %v", got.Generated, tt.wantGenerated)
			}
			if got.Polecat != tt.wantPolecat {
				t.Errorf("Polecat = %q, want %q", got.Polecat, tt.wantPolecat)
			}
			if got.Issue != tt.wantIssue {
				t.Errorf("Issue = %q, want %q", got.Issue, tt.wantIssue)
			}
		})
	}
}

// stampMillis decodes a stamp branchStampClock.next returned.
func stampMillis(t *testing.T, stamp string) int64 {
	t.Helper()
	ms, err := strconv.ParseInt(stamp, 36, 64)
	if err != nil {
		t.Fatalf("stamp %q is not a base-36 integer: %v", stamp, err)
	}
	return ms
}

func TestBranchStampClock_NextIsStrictlyIncreasing(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_800_000_000_000)
	tests := []struct {
		name   string
		second time.Duration // clock reading for the second stamp, after base
		want   int64         // second stamp in ms, after base
	}{
		{name: "same millisecond bumps by one", second: 0, want: 1},
		{name: "clock went backwards bumps by one", second: -5 * time.Millisecond, want: 1},
		{name: "clock advanced keeps its own reading", second: 10 * time.Millisecond, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A local clock, so no other test moves its last stamp.
			c := &branchStampClock{}
			first := stampMillis(t, c.next(func() time.Time { return base }))
			if first != base.UnixMilli() {
				t.Fatalf("first stamp = %d, want the clock reading %d", first, base.UnixMilli())
			}
			at := base.Add(tt.second)
			second := stampMillis(t, c.next(func() time.Time { return at }))
			if want := base.UnixMilli() + tt.want; second != want {
				t.Fatalf("second stamp = %d, want %d", second, want)
			}
		})
	}
}

func TestBranchStampClock_StampParsesAsGeneratedAt(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_800_000_000_000)
	c := &branchStampClock{}
	now := func() time.Time { return base }
	c.next(now)
	// The second call at the same millisecond is bumped one millisecond past the clock.
	stamp := c.next(now)

	meta, ok := ParseGeneratedBranchName(FormatGeneratedBranchName("alpha", "gt-abc", stamp))
	if !ok {
		t.Fatalf("ParseGeneratedBranchName did not parse a branch built from stamp %q", stamp)
	}
	got, ok := meta.GeneratedAt()
	if !ok {
		t.Fatalf("GeneratedAt() ok = false for stamp %q, want true", stamp)
	}
	if want := base.Add(time.Millisecond); !got.Equal(want) {
		t.Fatalf("GeneratedAt() = %v, want %v", got, want)
	}
}

func TestBranchNameMetaGeneratedAt(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Millisecond)
	suffix := strconv.FormatInt(now.UnixMilli(), 36)

	t.Run("decodes a generated timestamp suffix", func(t *testing.T) {
		meta, ok := ParseGeneratedBranchName(FormatGeneratedBranchName("alpha", "gt-abc", suffix))
		if !ok {
			t.Fatalf("ParseGeneratedBranchName did not parse a branch it generated")
		}
		got, ok := meta.GeneratedAt()
		if !ok {
			t.Fatalf("GeneratedAt() ok = false, want true")
		}
		if !got.Equal(now) {
			t.Fatalf("GeneratedAt() = %v, want %v", got, now)
		}
	})

	t.Run("decodes the no-issue dash form", func(t *testing.T) {
		meta, ok := ParseGeneratedBranchName(FormatGeneratedBranchName("alpha", "", suffix))
		if !ok {
			t.Fatalf("ParseGeneratedBranchName did not parse a branch it generated")
		}
		got, ok := meta.GeneratedAt()
		if !ok {
			t.Fatalf("GeneratedAt() ok = false, want true")
		}
		if !got.Equal(now) {
			t.Fatalf("GeneratedAt() = %v, want %v", got, now)
		}
	})

	t.Run("rejects the preserve-then-nuke backup-<sha> suffix", func(t *testing.T) {
		meta, ok := ParseGeneratedBranchName(FormatGeneratedBranchName("alpha", "gt-abc", "backup-deadbeef"))
		if !ok {
			t.Fatalf("ParseGeneratedBranchName did not parse a branch it generated")
		}
		if _, ok := meta.GeneratedAt(); ok {
			t.Fatalf("GeneratedAt() ok = true for a backup-<sha> suffix, want false")
		}
	})

	t.Run("rejects a non-generated branch", func(t *testing.T) {
		meta, ok := ParseBranchName("polecat/alpha/gt-pin-bd-metadata")
		if !ok {
			t.Fatalf("ParseBranchName did not parse")
		}
		if _, ok := meta.GeneratedAt(); ok {
			t.Fatalf("GeneratedAt() ok = true for a non-generated branch, want false")
		}
	})
}

func TestParseGeneratedBranchNameRejectsRawDashedIssues(t *testing.T) {
	t.Parallel()
	rejects := []string{
		"polecat/alpha/gt-pin-bd-metadata",
		"polecat/alpha/gt-jns7.1-mk123456",
	}
	for _, branch := range rejects {
		if meta, ok := ParseGeneratedBranchName(branch); ok {
			t.Errorf("ParseGeneratedBranchName(%q) = %+v, want ok=false", branch, meta)
		}
	}
}
