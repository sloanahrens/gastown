package cmd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestPolecatListReuseVerdictRedrivesFromProbe is the claude-41j.1 D9 table
// for the list path over canned probe results: the recorded cleanup_status
// is a hint, the verdict re-derives from the live probe of the worktree, and
// both are tagged with where they came from.
// TestIntegrationPolecatListReuseVerdictRedrivesFromLiveGit runs the probe
// against real worktrees.
func TestPolecatListReuseVerdictRedrivesFromProbe(t *testing.T) {
	t.Parallel()
	clean := polecat.LiveGitState{Branch: "polecat/topaz", Source: polecat.GitStateSourceLive}
	dirty := polecat.LiveGitState{
		Branch:      "polecat/topaz",
		Dirty:       true,
		DirtyReason: "git_state=has_uncommitted uncommitted_files=1",
		Source:      polecat.GitStateSourceLive,
	}
	notRoot := polecat.LiveGitState{
		Source:       polecat.GitStateSourceUnknown,
		FailedReason: "git_state=unknown path=/rig/polecats/peridot: resolving worktree root failed: path is not a git worktree root",
	}

	tests := []struct {
		name             string
		live             *polecat.LiveGitState // nil: no probe target
		cleanupStatus    string
		wantReusable     bool
		wantVerdict      string
		wantReason       string
		wantGitStateSrc  string
		wantGitReasonHas string
		wantBlockerHas   string
	}{
		{
			// The 2026-09-10 01:14 incident: the self-report said has_stash ten
			// minutes after the stash was dropped, and it was the only thing
			// consulted. Live git now decides, and the stale hint loses.
			name:            "stale recorded has_stash with a clean live worktree is reusable",
			live:            &clean,
			cleanupStatus:   string(polecat.CleanupStash),
			wantReusable:    true,
			wantVerdict:     polecat.WorkstateVerdictSafeToNuke,
			wantReason:      "reusable",
			wantGitStateSrc: polecat.GitStateSourceLive,
		},
		{
			name:            "recorded clean cannot rescue a dirty live worktree",
			live:            &dirty,
			cleanupStatus:   string(polecat.CleanupClean),
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantReason:      "git-dirty",
			wantGitStateSrc: polecat.GitStateSourceLive,
		},
		{
			name:             "a failed live probe fails closed with git_state=unknown",
			live:             &notRoot,
			cleanupStatus:    string(polecat.CleanupClean),
			wantVerdict:      polecat.WorkstateVerdictNeedsRecovery,
			wantReason:       "git-check-failed",
			wantGitStateSrc:  polecat.GitStateSourceUnknown,
			wantGitReasonHas: "not a git worktree root",
		},
		{
			name:            "no probe target leaves the recorded hint authoritative",
			cleanupStatus:   string(polecat.CleanupStash),
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantReason:      "cleanup-has_stash",
			wantGitStateSrc: polecat.GitStateSourceRecorded,
		},
		{
			// gt-ui2x acceptance case: a seat whose cleanup_status was never
			// self-reported (gt done crashed before writing it, or the seat
			// predates the field) must become reusable once the agent bead was
			// actually read (buildPolecatInventoryItem sets AgentBeadRead from
			// fields != nil) and a live probe confirms the worktree clean —
			// not stay blocked forever with no path out.
			name:            "missing cleanup_status with a clean live worktree is reusable",
			live:            &clean,
			wantReusable:    true,
			wantVerdict:     polecat.WorkstateVerdictSafeToNuke,
			wantReason:      "reusable",
			wantGitStateSrc: polecat.GitStateSourceLive,
		},
		{
			// The companion negative case: missing cleanup_status must not
			// become a blanket clearance — a live probe that finds real dirt
			// still blocks, exactly like every other status. The
			// missing-status blocker (checked first in decideWorkstate) sets
			// the reported reason; the dirty git fact is still a second,
			// independent blocker.
			name:            "missing cleanup_status with a dirty live worktree still blocks",
			live:            &dirty,
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantReason:      "cleanup-unknown",
			wantGitStateSrc: polecat.GitStateSourceLive,
			wantBlockerHas:  "git_state=has_uncommitted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := polecatInventoryEnv{GitProbeLocalOnly: true}
			env.probe = func(path string, localOnly bool) polecat.LiveGitState {
				if tt.live == nil {
					t.Fatalf("probe(%q) called with no probe target", path)
				}
				if path != env.WorktreePath || !localOnly {
					t.Errorf("probe(%q, %v), want (%q, true)", path, localOnly, env.WorktreePath)
				}
				return *tt.live
			}
			if tt.live != nil {
				env.WorktreePath = filepath.Join("/rig", "polecats", "topaz", "gastown")
			}
			item := buildPolecatInventoryItem(
				"gastown",
				"topaz",
				&beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: tt.cleanupStatus, Branch: "polecat/recorded"},
				nil,
				polecatSessionSet{},
				env,
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
			// A live branch supersedes the recorded one; a failed or absent
			// probe leaves the recorded branch.
			wantBranch := "polecat/recorded"
			if item.GitStateSource == polecat.GitStateSourceLive {
				wantBranch = "polecat/topaz"
			}
			if item.Branch != wantBranch {
				t.Fatalf("Branch = %q, want %q", item.Branch, wantBranch)
			}
			if tt.wantBlockerHas != "" {
				found := false
				for _, b := range item.Disposition.Blockers {
					if strings.Contains(b, tt.wantBlockerHas) {
						found = true
					}
				}
				if !found {
					t.Fatalf("Blockers = %v, want one containing %q", item.Disposition.Blockers, tt.wantBlockerHas)
				}
			}
		})
	}
}

// TestPolecatListItemJSONCarriesFactSources pins the wire shape `gt polecat
// list --json` emits: a consumer must be able to tell the recorded cleanup hint
// from the live git measurement, and a failed measurement must say why.
func TestPolecatListItemJSONCarriesFactSources(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
