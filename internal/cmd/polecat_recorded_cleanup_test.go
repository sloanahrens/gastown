package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// initInventoryGitWorktree builds a real one-commit repo so the list path's
// live probe has something to measure, the same way a polecat worktree does.
func initInventoryGitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "polecat/topaz"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// TestPolecatListReuseVerdictRedrivesFromLiveGit is the claude-41j.1 D9
// end-to-end table for the list path: the recorded cleanup_status is a hint,
// the verdict re-derives from a live probe of the worktree, and both are
// tagged with where they came from.
func TestPolecatListReuseVerdictRedrivesFromLiveGit(t *testing.T) {
	cleanWorktree := initInventoryGitWorktree(t)

	dirtyWorktree := initInventoryGitWorktree(t)
	dirtyPath := filepath.Join(dirtyWorktree, "uncommitted.txt")
	if err := os.WriteFile(dirtyPath, []byte("work in progress\n"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	// A leftover polecat directory: it exists, but it is not a worktree of its
	// own — it sits inside the rig's repository. Probing it must report the
	// probe as failed, not the rig root's branch and dirt as the polecat's.
	rigRoot := initInventoryGitWorktree(t)
	if err := os.WriteFile(filepath.Join(rigRoot, "rig-dirt.txt"), []byte("churn\n"), 0644); err != nil {
		t.Fatalf("write rig dirt: %v", err)
	}
	leftoverDir := filepath.Join(rigRoot, "polecats", "peridot", "gastown")
	if err := os.MkdirAll(leftoverDir, 0755); err != nil {
		t.Fatalf("mkdir leftover: %v", err)
	}

	tests := []struct {
		name             string
		worktreePath     string
		cleanupStatus    string
		wantReusable     bool
		wantVerdict      string
		wantReason       string
		wantGitStateSrc  string
		wantGitReasonHas string
	}{
		{
			// The 2026-09-10 01:14 incident: the self-report said has_stash ten
			// minutes after the stash was dropped, and it was the only thing
			// consulted. Live git now decides, and the stale hint loses.
			name:            "stale recorded has_stash with a clean live worktree is reusable",
			worktreePath:    cleanWorktree,
			cleanupStatus:   string(polecat.CleanupStash),
			wantReusable:    true,
			wantVerdict:     polecat.WorkstateVerdictSafeToNuke,
			wantReason:      "reusable",
			wantGitStateSrc: polecat.GitStateSourceLive,
		},
		{
			name:            "recorded clean cannot rescue a dirty live worktree",
			worktreePath:    dirtyWorktree,
			cleanupStatus:   string(polecat.CleanupClean),
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantReason:      "git-dirty",
			wantGitStateSrc: polecat.GitStateSourceLive,
		},
		{
			name:             "a failed live probe fails closed with git_state=unknown",
			worktreePath:     filepath.Join(t.TempDir(), "gone"),
			cleanupStatus:    string(polecat.CleanupClean),
			wantVerdict:      polecat.WorkstateVerdictNeedsRecovery,
			wantReason:       "git-check-failed",
			wantGitStateSrc:  polecat.GitStateSourceUnknown,
			wantGitReasonHas: "git_state=unknown",
		},
		{
			name:             "a directory that is not its own worktree is unmeasurable, not clean",
			worktreePath:     leftoverDir,
			cleanupStatus:    string(polecat.CleanupClean),
			wantVerdict:      polecat.WorkstateVerdictNeedsRecovery,
			wantReason:       "git-check-failed",
			wantGitStateSrc:  polecat.GitStateSourceUnknown,
			wantGitReasonHas: "not a git worktree root",
		},
		{
			name:            "no probe target leaves the recorded hint authoritative",
			worktreePath:    "",
			cleanupStatus:   string(polecat.CleanupStash),
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantReason:      "cleanup-has_stash",
			wantGitStateSrc: polecat.GitStateSourceRecorded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := buildPolecatInventoryItem(
				"gastown",
				"topaz",
				&beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: tt.cleanupStatus, Branch: "polecat/topaz"},
				nil,
				polecatSessionSet{},
				polecatInventoryEnv{WorktreePath: tt.worktreePath},
			)

			if item.Disposition.Reusable != tt.wantReusable {
				t.Fatalf("Reusable = %v, want %v (disposition %+v)", item.Disposition.Reusable, tt.wantReusable, item.Disposition)
			}
			if item.Disposition.Verdict != tt.wantVerdict || item.Disposition.Reason != tt.wantReason {
				t.Fatalf("verdict/reason = %s/%s, want %s/%s", item.Disposition.Verdict, item.Disposition.Reason, tt.wantVerdict, tt.wantReason)
			}
			if item.GitStateSource != tt.wantGitStateSrc {
				t.Fatalf("GitStateSource = %q, want %q", item.GitStateSource, tt.wantGitStateSrc)
			}
			if item.CleanupStatusSource != polecat.CleanupStatusSourceRecorded {
				t.Fatalf("CleanupStatusSource = %q, want %q", item.CleanupStatusSource, polecat.CleanupStatusSourceRecorded)
			}
			if tt.wantGitReasonHas != "" && !strings.Contains(item.GitStateReason, tt.wantGitReasonHas) {
				t.Fatalf("GitStateReason = %q, want it to mention %q", item.GitStateReason, tt.wantGitReasonHas)
			}
			if item.GitStateSource == polecat.GitStateSourceLive && item.Branch != "polecat/topaz" {
				t.Fatalf("Branch = %q, want the live branch polecat/topaz", item.Branch)
			}
		})
	}
}

// TestPolecatListItemJSONCarriesFactSources pins the wire shape `gt polecat
// list --json` emits: a consumer must be able to tell the recorded cleanup hint
// from the live git measurement, and a failed measurement must say why.
func TestPolecatListItemJSONCarriesFactSources(t *testing.T) {
	item := PolecatListItem{
		Rig:                 "gastown",
		Name:                "topaz",
		CleanupStatus:       string(polecat.CleanupStash),
		CleanupStatusSource: polecat.CleanupStatusSourceRecorded,
		GitStateSource:      polecat.GitStateSourceLive,
		ReuseStatus:         "idle-clean",
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["cleanup_status_source"] != polecat.CleanupStatusSourceRecorded {
		t.Fatalf("cleanup_status_source = %v, want %q", decoded["cleanup_status_source"], polecat.CleanupStatusSourceRecorded)
	}
	if decoded["git_state_source"] != polecat.GitStateSourceLive {
		t.Fatalf("git_state_source = %v, want %q", decoded["git_state_source"], polecat.GitStateSourceLive)
	}

	unknown := PolecatListItem{GitStateSource: polecat.GitStateSourceUnknown, GitStateReason: "git_state=unknown path=/gone"}
	encoded, err = json.Marshal(unknown)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded = map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["git_state_source"] != polecat.GitStateSourceUnknown {
		t.Fatalf("git_state_source = %v, want %q", decoded["git_state_source"], polecat.GitStateSourceUnknown)
	}
	if decoded["git_state_reason"] == "" || decoded["git_state_reason"] == nil {
		t.Fatalf("a failed live probe must carry a reason, got %v", decoded["git_state_reason"])
	}
}

// TestPolecatReuseDetailLineMarksRecordedFields pins the human table's
// provenance marks: the recorded cleanup hint is labeled as recorded, and the
// live git answer says so (with the reason when the probe failed).
func TestPolecatReuseDetailLineMarksRecordedFields(t *testing.T) {
	tests := []struct {
		name string
		item PolecatListItem
		want string
	}{
		{
			name: "no reuse status renders no line",
			item: PolecatListItem{Rig: "gastown", Name: "topaz"},
			want: "",
		},
		{
			name: "recorded cleanup and live git",
			item: PolecatListItem{
				ReuseStatus:         "idle-clean",
				CleanupStatus:       string(polecat.CleanupStash),
				CleanupStatusSource: polecat.CleanupStatusSourceRecorded,
				GitStateSource:      polecat.GitStateSourceLive,
			},
			want: "reuse: idle-clean cleanup=has_stash (recorded) git=live",
		},
		{
			name: "failed probe carries its reason",
			item: PolecatListItem{
				ReuseStatus:    "idle-recovery-needed",
				GitStateSource: polecat.GitStateSourceUnknown,
				GitStateReason: "git_state=unknown path=/gone",
			},
			want: "reuse: idle-recovery-needed git=unknown (git_state=unknown path=/gone)",
		},
		{
			name: "recorded fallback is labeled too",
			item: PolecatListItem{
				ReuseStatus:    "idle-recovery-needed",
				CleanupStatus:  string(polecat.CleanupStash),
				GitStateSource: polecat.GitStateSourceRecorded,
			},
			want: "reuse: idle-recovery-needed cleanup=has_stash git=recorded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := polecatReuseDetailLine(tt.item); got != tt.want {
				t.Fatalf("polecatReuseDetailLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
