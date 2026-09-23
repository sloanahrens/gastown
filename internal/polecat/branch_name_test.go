package polecat

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFormatGeneratedBranchName_ActionCompatible(t *testing.T) {
	branch := FormatGeneratedBranchName("alpha", "gt-pin-bd-metadata", "mk123456")
	if strings.Contains(branch, "@") {
		t.Fatalf("FormatGeneratedBranchName() = %q, must not contain @", branch)
	}

	claudeCodeActionBranch := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.#+,-]*$`)
	if !claudeCodeActionBranch.MatchString(branch) {
		t.Fatalf("FormatGeneratedBranchName() = %q, rejected by claude-code-action branch pattern", branch)
	}
	if err := exec.Command("git", "check-ref-format", "--branch", branch).Run(); err != nil {
		t.Fatalf("FormatGeneratedBranchName() = %q, rejected by git check-ref-format: %v", branch, err)
	}
}

func TestParseBranchName(t *testing.T) {
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

func TestBranchNameMetaGeneratedAt(t *testing.T) {
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
