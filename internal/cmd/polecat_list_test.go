package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// fakePolecatMRLister is the one bulk merge-request source, faked so the join
// can be exercised without a Dolt server.
type fakePolecatMRLister struct {
	mrs []*beads.Issue
	err error
	// calls records the ListOptions each call was made with, so a test can
	// assert the read is one rig-wide query rather than a per-polecat fan-out.
	calls []beads.ListOptions
}

func (f *fakePolecatMRLister) ListMergeRequests(opts beads.ListOptions) ([]*beads.Issue, error) {
	f.calls = append(f.calls, opts)
	if f.err != nil {
		return nil, f.err
	}
	return f.mrs, nil
}

// mrBead builds a merge-request wisp description the way gt done writes it.
func mrBead(id, worker, rig, status, closeReason string, opts ...func(*beads.Issue)) *beads.Issue {
	description := "branch: polecat/" + worker + "/gt-abc\n" +
		"target: main\n" +
		"source_issue: gt-abc\n" +
		"rig: " + rig + "\n" +
		"worker: " + worker + "\n"
	if closeReason != "" {
		description += "close_reason: " + closeReason + "\n"
	}
	issue := &beads.Issue{ID: id, Title: "Merge: gt-abc", Status: status, Description: description, Labels: []string{"gt:merge-request"}}
	for _, opt := range opts {
		opt(issue)
	}
	return issue
}

func claimedMR(id, worker, rig string) *beads.Issue {
	mr := mrBead(id, worker, rig, "open", "")
	mr.Assignee = "gastown/refinery"
	return mr
}

func blockedMR(id, worker, rig string) *beads.Issue {
	mr := mrBead(id, worker, rig, "open", "")
	mr.BlockedBy = []string{"gt-blocker"}
	return mr
}

// TestPolecatMRStatus covers every value PolecatListItem.MRStatus can take, so
// the dashboard (gt-kqi2) never has to guess what a status means: the queue
// states come from the same derivation `gt mq list` uses, and the terminal ones
// say how the MR left the queue.
func TestPolecatMRStatus(t *testing.T) {
	tests := []struct {
		name string
		mr   *beads.Issue
		want string
	}{
		{name: "no bead is missing", mr: nil, want: polecatMRStatusMissing},
		{name: "unblocked and unclaimed is ready", mr: mrBead("gt-mr1", "topaz", "gastown", "open", ""), want: polecatMRStatusReady},
		{name: "unresolved blocker is blocked", mr: blockedMR("gt-mr1", "topaz", "gastown"), want: polecatMRStatusBlocked},
		{name: "refinery claim is open (in flight)", mr: claimedMR("gt-mr1", "topaz", "gastown"), want: polecatMRStatusOpen},
		{name: "closed with close_reason merged is merged", mr: mrBead("gt-mr1", "topaz", "gastown", "closed", "merged"), want: polecatMRStatusMerged},
		{name: "closed with close_reason rejected is rejected", mr: mrBead("gt-mr1", "topaz", "gastown", "closed", "rejected"), want: polecatMRStatusRejected},
		{name: "closed with a conflict close reason is rejected", mr: mrBead("gt-mr1", "topaz", "gastown", "closed", "conflict"), want: polecatMRStatusRejected},
		{name: "closed without a close reason is rejected", mr: mrBead("gt-mr1", "topaz", "gastown", "closed", ""), want: polecatMRStatusRejected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := polecatMRStatusFor(tt.mr); got != tt.want {
				t.Fatalf("polecatMRStatusFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPolecatMRIndexStatusFor guards the join itself: the agent bead's
// active_mr wins, the worker key is the fallback for a polecat whose pointer
// was already cleared, and a pointer with no bead behind it reports missing
// rather than silently nothing.
func TestPolecatMRIndexStatusFor(t *testing.T) {
	open := mrBead("gt-mr-open", "topaz", "gastown", "open", "")
	merged := mrBead("gt-mr-merged", "flint", "gastown", "closed", "merged")
	otherRig := mrBead("gt-mr-other", "topaz", "greenplace", "open", "")
	unkeyed := &beads.Issue{ID: "gt-mr-unkeyed", Status: "open"}

	index := buildPolecatMRIndex("gastown", []*beads.Issue{open, merged, otherRig, unkeyed, nil})

	tests := []struct {
		name       string
		activeMR   string
		worker     string
		wantID     string
		wantStatus string
	}{
		{name: "active_mr wins", activeMR: "gt-mr-open", worker: "topaz", wantID: "gt-mr-open", wantStatus: polecatMRStatusReady},
		{name: "merged active_mr reports merged", activeMR: "gt-mr-merged", worker: "flint", wantID: "gt-mr-merged", wantStatus: polecatMRStatusMerged},
		{name: "worker key fills a cleared pointer", worker: "topaz", wantID: "gt-mr-open", wantStatus: polecatMRStatusReady},
		{name: "stale pointer falls back to the worker's queued MR", activeMR: "gt-mr-reaped", worker: "topaz", wantID: "gt-mr-open", wantStatus: polecatMRStatusReady},
		{name: "pointer with no bead and no queued MR is missing", activeMR: "gt-mr-reaped", worker: "flint", wantID: "gt-mr-reaped", wantStatus: polecatMRStatusMissing},
		{name: "polecat with no MR reports nothing", wantID: "", wantStatus: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, status := index.statusFor(tt.activeMR, tt.worker)
			if id != tt.wantID || status != tt.wantStatus {
				t.Fatalf("statusFor(%q, %q) = (%q, %q), want (%q, %q)", tt.activeMR, tt.worker, id, status, tt.wantID, tt.wantStatus)
			}
		})
	}

	t.Run("an index that never loaded claims nothing", func(t *testing.T) {
		// A failed rig-wide query must not turn every active_mr into "missing":
		// the queue was never read, so the inventory has no state to report and
		// the blocker text says unknown instead.
		id, status := polecatMRIndex{}.statusFor("gt-mr-open", "topaz")
		if id != "" || status != "" {
			t.Fatalf("unloaded index statusFor = (%q, %q), want empty", id, status)
		}
	})

	t.Run("another rig's MR is indexed out entirely", func(t *testing.T) {
		if _, ok := index.byID["gt-mr-other"]; ok {
			t.Error("buildPolecatMRIndex kept an MR belonging to another rig")
		}
	})

	t.Run("closed MRs never answer the worker key", func(t *testing.T) {
		if _, ok := index.byWorker["flint"]; ok {
			t.Error("byWorker holds a terminal MR — a stale merged MR would be reported as current work")
		}
	})

	t.Run("newest open MR wins for a re-submitted worker", func(t *testing.T) {
		older := mrBead("gt-mr-old", "topaz", "gastown", "open", "")
		older.CreatedAt = "2026-09-19T10:00:00Z"
		newer := mrBead("gt-mr-new", "topaz", "gastown", "open", "")
		newer.CreatedAt = "2026-09-19T12:00:00Z"
		// Newest first and oldest first must agree: the pick may not depend on
		// the order the lister happened to return.
		for _, mrs := range [][]*beads.Issue{{newer, older}, {older, newer}} {
			index := buildPolecatMRIndex("gastown", mrs)
			if id, _ := index.statusFor("", "topaz"); id != "gt-mr-new" {
				t.Fatalf("statusFor(worker) = %q, want gt-mr-new", id)
			}
		}
	})
}

// TestPolecatInventoryMRJoin is the plan's acceptance path end to end for the
// join: an agent bead with active_mr plus the bulk lister naming that MR open
// and unblocked reports ready; an active_mr whose bead the lister does not
// return reports missing. It also pins the blocker text — the inventory used to
// hardcode status=unknown here, so the list could never say what the MR's real
// state was.
func TestPolecatInventoryMRJoin(t *testing.T) {
	setupPolecatTestRegistry(t)

	tests := []struct {
		name             string
		activeMR         string
		mrs              []*beads.Issue
		listErr          error
		wantID           string
		wantStatus       string
		wantBlocker      string
		wantVerdict      string
		wantReuseStatus  string
		wantReusableFlag bool
	}{
		{
			name:            "queued MR reports ready",
			activeMR:        "gt-mr-open",
			mrs:             []*beads.Issue{mrBead("gt-mr-open", "topaz", "gastown", "open", "")},
			wantID:          "gt-mr-open",
			wantStatus:      polecatMRStatusReady,
			wantBlocker:     "active_mr=gt-mr-open status=ready",
			wantVerdict:     polecat.WorkstateVerdictPendingMR,
			wantReuseStatus: "idle-pr-open",
		},
		{
			// The rig's queue was read and has nothing for this polecat: no
			// active_mr bead, and no open MR under its worker name either.
			name:       "pointer with no bead behind it reports missing",
			activeMR:   "gt-mr-reaped",
			mrs:        []*beads.Issue{mrBead("gt-mr-open", "flint", "gastown", "open", "")},
			wantID:     "gt-mr-reaped",
			wantStatus: polecatMRStatusMissing,
			// Missing still blocks: the inventory cannot prove the MR was
			// merged (only that its wisp is gone), so the disposition stays
			// fail-closed — but it now says so truthfully.
			wantBlocker:     "active_mr=gt-mr-reaped status=missing",
			wantVerdict:     polecat.WorkstateVerdictPendingMR,
			wantReuseStatus: "idle-pr-open",
		},
		{
			name:            "unreadable MR source falls back to unknown",
			activeMR:        "gt-mr-open",
			listErr:         errors.New("dolt unreachable"),
			wantID:          "",
			wantStatus:      "",
			wantBlocker:     "active_mr=gt-mr-open status=unknown",
			wantVerdict:     polecat.WorkstateVerdictPendingMR,
			wantReuseStatus: "idle-pr-open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakePolecatMRLister{mrs: tt.mrs, err: tt.listErr}
			index, err := loadPolecatMRIndex(lister, "gastown")
			if err != nil {
				// The list path warns and keeps an empty index — same shape.
				index = polecatMRIndex{}
			}
			if len(lister.calls) != 1 {
				t.Fatalf("ListMergeRequests called %d times, want exactly 1 (one rig-wide query, never per polecat)", len(lister.calls))
			}
			if call := lister.calls[0]; call.Label != "gt:merge-request" || call.Rig != "gastown" || call.Priority != -1 {
				t.Fatalf("ListMergeRequests opts = %+v, want the rig-wide merge-request query", call)
			}

			item := buildPolecatInventoryItem(
				"gastown",
				"topaz",
				&beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), ActiveMR: tt.activeMR},
				nil,
				polecatSessionSet{},
				polecatInventoryEnv{MRs: index},
			)
			if item.MRID != tt.wantID || item.MRStatus != tt.wantStatus {
				t.Fatalf("MRID/MRStatus = %q/%q, want %q/%q", item.MRID, item.MRStatus, tt.wantID, tt.wantStatus)
			}
			if len(item.Disposition.Blockers) != 1 || item.Disposition.Blockers[0] != tt.wantBlocker {
				t.Fatalf("blockers = %v, want [%s]", item.Disposition.Blockers, tt.wantBlocker)
			}
			if item.Disposition.Verdict != tt.wantVerdict || item.Disposition.ReuseStatus != tt.wantReuseStatus {
				t.Fatalf("disposition = %+v, want verdict %s reuse %s", item.Disposition, tt.wantVerdict, tt.wantReuseStatus)
			}
		})
	}
}

// TestPolecatAgentMRDetails pins the text output: only what the join actually
// found is claimed, and a polecat with neither agent nor MR adds no line.
func TestPolecatAgentMRDetails(t *testing.T) {
	tests := []struct {
		name string
		item PolecatListItem
		want string
	}{
		{name: "neither", item: PolecatListItem{Rig: "gastown", Name: "topaz"}, want: ""},
		{name: "agent only", item: PolecatListItem{Agent: "flash"}, want: "agent=flash"},
		{name: "agent and MR", item: PolecatListItem{Agent: "flash", MRID: "gt-mr1", MRStatus: polecatMRStatusReady}, want: "agent=flash mr=gt-mr1 ready"},
		{name: "MR without a status", item: PolecatListItem{MRID: "gt-mr1"}, want: "mr=gt-mr1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := polecatAgentMRDetails(tt.item); got != tt.want {
				t.Fatalf("polecatAgentMRDetails() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPolecatSpawnGraceWindowFallsBack guards that an unreadable town root uses
// the compiled-in witness default instead of switching the grace off — grace
// only delays a stalled verdict, so losing the config must not lose the grace.
func TestPolecatSpawnGraceWindowFallsBack(t *testing.T) {
	got := polecatSpawnGraceWindow("")
	if got <= 0 {
		t.Fatalf("polecatSpawnGraceWindow(\"\") = %s, want the compiled-in default", got)
	}
	if got != config.DefaultWitnessHeartbeatStartupGrace {
		t.Fatalf("polecatSpawnGraceWindow(\"\") = %s, want %s", got, config.DefaultWitnessHeartbeatStartupGrace)
	}
}

// TestPolecatSpawnGraceWindowReadsTownSettings drives the configured branch.
func TestPolecatSpawnGraceWindowReadsTownSettings(t *testing.T) {
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	settings := `{"type":"town-settings","version":1,"operational":{"witness":{"heartbeat_startup_grace":"90s"}}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(settings), 0644); err != nil {
		t.Fatalf("write town settings: %v", err)
	}

	if got := polecatSpawnGraceWindow(townRoot); got != 90*time.Second {
		t.Fatalf("polecatSpawnGraceWindow(configured) = %s, want 90s", got)
	}
}

// TestPolecatListJSONAddsAgentAndMRFields pins the additive contract for the
// dashboard consumer (gt-kqi2): the new fields are present when known and
// omitted when not, so existing JSON readers keep working.
func TestPolecatListJSONAddsAgentAndMRFields(t *testing.T) {
	encoded, err := json.Marshal(PolecatListItem{
		Rig: "gastown", Name: "topaz", State: polecat.StateWorking,
		Agent: "flash", MRID: "gt-mr1", MRStatus: polecatMRStatusReady,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"agent":"flash"`, `"mr_id":"gt-mr1"`, `"mr_status":"ready"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("json %s missing %s", encoded, want)
		}
	}

	encoded, err = json.Marshal(PolecatListItem{Rig: "gastown", Name: "topaz", State: polecat.StateIdle})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, unwanted := range []string{"agent", "mr_id", "mr_status"} {
		if strings.Contains(string(encoded), unwanted) {
			t.Errorf("json %s should omit %s for a polecat with none", encoded, unwanted)
		}
	}
}
