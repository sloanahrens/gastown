package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestAgentBeadMatchesDescriptionAndIDFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		issue *beads.Issue
		role  string
		rig   string
		want  bool
	}{
		{
			name: "description matches legacy random wisp ID",
			issue: &beads.Issue{
				ID:          "au-wisp-0ti",
				Description: "Agent\n\nrole_type: refinery\nrig: alleago_ui",
			},
			role: "refinery",
			rig:  "alleago_ui",
			want: true,
		},
		{
			name: "canonical ID fallback matches sparse wisp metadata",
			issue: &beads.Issue{
				ID: "gt-gastown-witness",
			},
			role: "witness",
			rig:  "gastown",
			want: true,
		},
		{
			name: "collapsed prefix-rig ID fallback matches sparse metadata",
			issue: &beads.Issue{
				ID: "cp-refinery",
			},
			role: "refinery",
			rig:  "cp",
			want: true,
		},
		{
			name: "role mismatch",
			issue: &beads.Issue{
				ID:          "gt-gastown-witness",
				Description: "Agent\n\nrole_type: witness\nrig: gastown",
			},
			role: "refinery",
			rig:  "gastown",
			want: false,
		},
		{
			name: "rig mismatch",
			issue: &beads.Issue{
				ID:          "gt-gastown-refinery",
				Description: "Agent\n\nrole_type: refinery\nrig: gastown",
			},
			role: "refinery",
			rig:  "other",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentBeadMatches(tt.issue, tt.role, tt.rig)
			if got != tt.want {
				t.Fatalf("agentBeadMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPickBestAgentBead(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{
		candidate("town-issue", agentSourceTownIssues, "open"),
		candidate("rig-issue", agentSourceRigIssues, "open"),
		candidate("town-wisp", agentSourceTownWisps, "open"),
		candidate("rig-wisp", agentSourceRigWisps, "open"),
	}

	got, err := pickBestAgentBead(candidates)
	if err != nil {
		t.Fatalf("pickBestAgentBead returned error: %v", err)
	}
	if got == nil || got.ID != "rig-wisp" {
		t.Fatalf("pickBestAgentBead picked %v, want rig-wisp", got)
	}
}

func TestPickBestAgentBeadSkipsClosed(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{
		candidate("closed-rig-wisp", agentSourceRigWisps, "closed"),
		candidate("open-rig-issue", agentSourceRigIssues, "open"),
	}

	got, err := pickBestAgentBead(candidates)
	if err != nil {
		t.Fatalf("pickBestAgentBead returned error: %v", err)
	}
	if got == nil || got.ID != "open-rig-issue" {
		t.Fatalf("pickBestAgentBead picked %v, want open-rig-issue", got)
	}
}

func TestPickBestAgentBeadRejectsSameRankDuplicates(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{
		candidate("rig-wisp-a", agentSourceRigWisps, "open"),
		candidate("rig-wisp-b", agentSourceRigWisps, "open"),
		candidate("rig-issue", agentSourceRigIssues, "open"),
	}

	got, err := pickBestAgentBead(candidates)
	if err == nil {
		t.Fatalf("pickBestAgentBead picked %v, want duplicate error", got)
	}
	if !strings.Contains(err.Error(), "multiple matching agent beads") {
		t.Fatalf("error = %q, want duplicate diagnostic", err)
	}
}

func TestAgentBeadNotFoundMessageNamesClosedMatches(t *testing.T) {
	t.Parallel()
	// Role-matched candidates, as runAgentsResolve passes them.
	candidates := []agentBeadCandidate{
		candidate("gt-gastown-refinery", agentSourceRigIssues, "closed"),
		candidate("gt-town-refinery", agentSourceTownIssues, "closed"),
	}

	got := agentBeadNotFoundMessage("refinery", "gastown", closedAgentBeads(candidates))

	if !strings.Contains(got, `no agent bead found for role "refinery" in rig "gastown"`) {
		t.Fatalf("message = %q, want the base no-match diagnostic", got)
	}
	if !strings.Contains(got, "2 closed beads also match") {
		t.Fatalf("message = %q, want a closed-match count", got)
	}
	if !strings.Contains(got, "gt-gastown-refinery (rig-issues), gt-town-refinery (town-issues)") {
		t.Fatalf("message = %q, want both closed matches with provenance", got)
	}
}

func TestAgentBeadNotFoundMessageNamesASingleClosedMatch(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{candidate("gt-gastown-refinery", agentSourceRigIssues, "closed")}

	got := agentBeadNotFoundMessage("refinery", "gastown", closedAgentBeads(candidates))

	if !strings.Contains(got, "; closed bead gt-gastown-refinery (rig-issues) also matches") {
		t.Fatalf("message = %q, want the singular closed-match form", got)
	}
}

func TestClosedAgentBeadsSelectsClosedSortedByID(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{
		candidate("rig-issue", agentSourceRigIssues, "open"),
		candidate("zeta", agentSourceRigWisps, "closed"),
		candidate("alpha", agentSourceTownIssues, "CLOSED"),
	}

	got := closedAgentBeads(candidates)

	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Fatalf("closedAgentBeads() = %+v, want the two closed candidates ordered alpha, zeta", got)
	}
}

func TestAgentBeadNotFoundMessageWithoutClosedMatches(t *testing.T) {
	t.Parallel()
	got := agentBeadNotFoundMessage("refinery", "gastown", nil)

	if got != `no agent bead found for role "refinery" in rig "gastown"` {
		t.Fatalf("message = %q, want the bare diagnostic", got)
	}
}

func TestAgentBeadNotFoundMessageWithoutRig(t *testing.T) {
	t.Parallel()
	got := agentBeadNotFoundMessage("mayor", "", nil)

	if got != `no agent bead found for role "mayor"` {
		t.Fatalf("message = %q, want no rig clause", got)
	}
}

func TestPickBestAgentBeadDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	candidates := []agentBeadCandidate{candidate("rig-wisp", agentSourceRigWisps, "closed")}

	if _, err := pickBestAgentBead(candidates); err != nil {
		t.Fatalf("pickBestAgentBead returned error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != "rig-wisp" || candidates[0].Status != "closed" {
		t.Fatalf("candidates = %+v, want the closed candidate left intact for reporting", candidates)
	}
}

func candidate(id string, source agentBeadSource, status string) agentBeadCandidate {
	return agentBeadCandidate{
		ID:     id,
		Source: source,
		Status: status,
		Issue:  &beads.Issue{ID: id, Status: status},
	}
}
