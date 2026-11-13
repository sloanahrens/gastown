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
		name     string
		readings []time.Duration // clock reading for each call, after base
		want     []int64         // stamp for each call, in ms after base
	}{
		{
			name:     "same millisecond bumps by one",
			readings: []time.Duration{0, 0},
			want:     []int64{0, 1},
		},
		{
			name:     "three calls in one millisecond bump by one each",
			readings: []time.Duration{0, 0, 0},
			want:     []int64{0, 1, 2},
		},
		{
			name:     "small backward skew bumps by one",
			readings: []time.Duration{0, -5 * time.Millisecond},
			want:     []int64{0, 1},
		},
		{
			name:     "backward step beyond the lead follows the clock",
			readings: []time.Duration{0, -10 * time.Minute},
			want:     []int64{0, -600_000},
		},
		{
			name:     "clock advanced keeps its own reading",
			readings: []time.Duration{0, 10 * time.Millisecond},
			want:     []int64{0, 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A local clock, so no other test moves its last stamp.
			c := &branchStampClock{}
			for i, reading := range tt.readings {
				at := base.Add(reading)
				got := stampMillis(t, c.next(func() time.Time { return at }))
				if want := base.UnixMilli() + tt.want[i]; got != want {
					t.Fatalf("call %d stamp = %d, want %d", i, got, want)
				}
			}
		})
	}
}

func TestBranchStampClock_BumpIsBoundedByLead(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_800_000_000_000)
	c := &branchStampClock{}
	c.next(func() time.Time { return base })
	// A step back that keeps the bump within the lead is still bumped.
	edge := base.Add(-branchStampMaxLead + time.Millisecond)
	got := stampMillis(t, c.next(func() time.Time { return edge }))
	if want := base.UnixMilli() + 1; got != want {
		t.Fatalf("stamp inside the lead = %d, want the bumped %d", got, want)
	}
	// The last stamp is now base+1, so reaching past would need a bump beyond
	// the lead: the stamp follows the clock instead of running ahead.
	past := base.Add(-branchStampMaxLead)
	got = stampMillis(t, c.next(func() time.Time { return past }))
	if want := past.UnixMilli(); got != want {
		t.Fatalf("stamp past the lead = %d, want the clock reading %d", got, want)
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
