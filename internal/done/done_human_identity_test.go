package done

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	crewTestHumanName  = "Test Operator"
	crewTestHumanEmail = "operator@example.test"
)

// writeTownHuman writes the town's overseer section: the one record gt done
// reads the human's identity from.
func writeTownHuman(t *testing.T, townRoot, name, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	town, err := json.Marshal(map[string]any{
		"overseer": map[string]any{
			"type": "overseer", "version": 1, "name": name, "email": email, "source": "test",
		},
	})
	if err != nil {
		t.Fatalf("marshal town.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), town, 0o644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
}

// assertCrewRefused checks a refused submission wrote nothing and gated
// nothing.
func assertCrewRefused(t *testing.T, h *submitHarness, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("crew submit succeeded, want a refusal")
	}
	if len(h.gate.heads) != 0 {
		t.Errorf("gate ran on %v for a refused submission, want none", h.gate.heads)
	}
	if issue := h.source(t); beads.HasLabel(issue, land.LabelReadyToLand) || strings.Contains(issue.Notes, land.ReadyNoteMarker) {
		t.Errorf("refused submission still marked the bead: labels %v notes %q", issue.Labels, issue.Notes)
	}
}

// TestCrewSubmitRefusesBranchNamingTheHuman: a crew/<agent>/<bead> branch
// whose agent segment is the human's name, or the local part of their email,
// is refused in any case and the refusal says which field and how to fix it
// (gt-dvotr).
func TestCrewSubmitRefusesBranchNamingTheHuman(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, humanName, humanEmail, branch string
	}{
		{"the configured name, in any case", "sloan", "ops@example.com", "crew/Sloan/gt-1"},
		{"the email local part, in any case", "Test Operator", "Sloan.Ahrens@example.com", "crew/sloan.ahrens/gt-1"},
		{"a crew branch missing its bead still names its agent", "Test Operator", "sloan.ahrens@example.com", "crew/sloan.ahrens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCrewSubmitHarness(t)
			writeTownHuman(t, h.townRoot, tc.humanName, tc.humanEmail)
			h.r.branch = tc.branch
			// The branch is on origin at HEAD, so only the identity check
			// stands between it and the gate.
			h.repo.origin[tc.branch] = crewTestHead
			err := submitCrewForLanding(h.r)
			assertCrewRefused(t, h, err)
			for _, want := range []string{"branch name", tc.branch, tc.humanName, "crew/<agent>/<bead>", "Rename the branch"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
		})
	}
}

// TestCrewSubmitRefusesHeadCommitNamingTheHuman: each of the head commit's
// four identity fields is refused on its own, in any case, and the refusal
// names that field and the amend that fixes it (gt-dvotr).
func TestCrewSubmitRefusesHeadCommitNamingTheHuman(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		commit            git.CommitAuthorship
		field, shownHuman string
	}{
		{
			name: "author name",
			commit: git.CommitAuthorship{
				AuthorName: "TEST OPERATOR", AuthorEmail: "dunlin@example.com",
				CommitterName: "dunlin", CommitterEmail: "dunlin@example.com",
			},
			field: "author name", shownHuman: "TEST OPERATOR",
		},
		{
			name: "author email",
			commit: git.CommitAuthorship{
				AuthorName: "dunlin", AuthorEmail: "operator@example.test",
				CommitterName: "dunlin", CommitterEmail: "dunlin@example.com",
			},
			field: "author email", shownHuman: "operator@example.test",
		},
		{
			name: "committer name",
			commit: git.CommitAuthorship{
				AuthorName: "dunlin", AuthorEmail: "dunlin@example.com",
				CommitterName: "Test Operator", CommitterEmail: "dunlin@example.com",
			},
			field: "committer name", shownHuman: "Test Operator",
		},
		{
			name: "committer email",
			commit: git.CommitAuthorship{
				AuthorName: "dunlin", AuthorEmail: "dunlin@example.com",
				CommitterName: "dunlin", CommitterEmail: "operator@example.test",
			},
			field: "committer email", shownHuman: "operator@example.test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCrewSubmitHarness(t)
			h.repo.authorship = tc.commit
			err := submitCrewForLanding(h.r)
			assertCrewRefused(t, h, err)
			for _, want := range []string{tc.field, tc.shownHuman, "git commit --amend --reset-author"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
		})
	}
}

// TestCrewSubmitPassesOverseerBranchAndAuthor: the overseer is an agent, so
// crew/overseer/<bead> with the overseer's author identity still submits
// (gt-dvotr).
func TestCrewSubmitPassesOverseerBranchAndAuthor(t *testing.T) {
	t.Parallel()
	const branch = "crew/overseer/gt-1"
	h := newCrewSubmitHarness(t)
	h.r.branch = branch
	h.r.sender = "town/crew/overseer"
	h.repo.origin[branch] = crewTestHead
	h.repo.authorship = git.CommitAuthorship{
		AuthorName: "overseer", AuthorEmail: "overseer@gastown.local",
		CommitterName: "overseer", CommitterEmail: "overseer@gastown.local",
	}
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit of the overseer's own branch: %v", err)
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("the overseer's branch was not marked ready to land")
	}
}

// TestCrewSubmitPassesBranchWithoutASeat: a branch that is not
// crew/<agent>/<bead> names no agent to judge, so only the head commit is
// checked.
func TestCrewSubmitPassesBranchWithoutASeat(t *testing.T) {
	t.Parallel()
	const branch = "feature/sloan.ahrens-work"
	h := newCrewSubmitHarness(t)
	h.r.branch = branch
	h.repo.origin[branch] = crewTestHead
	h.repo.authorship = git.CommitAuthorship{
		AuthorName: "dunlin", AuthorEmail: "dunlin@gastown.local",
		CommitterName: "dunlin", CommitterEmail: "dunlin@gastown.local",
	}
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit of a branch naming no agent: %v", err)
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("a branch naming no agent was not marked ready to land")
	}
}

// TestCrewSubmitRefusesUnreadableHumanIdentity: without the town's identity
// no branch or commit can be judged, so the submission is refused rather than
// passing by default (gt-2ge3z).
func TestCrewSubmitRefusesUnreadableHumanIdentity(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	if err := os.Remove(filepath.Join(h.townRoot, "mayor", "town.json")); err != nil {
		t.Fatalf("remove town.json: %v", err)
	}
	err := submitCrewForLanding(h.r)
	assertCrewRefused(t, h, err)
	if want := "human identity cannot be read"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q lacks %q", err, want)
	}
	if want := "mayor/overseer.json"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not name the file it read: want %q", err, want)
	}
}

// TestCrewSubmitRefusesUnreadableHeadIdentity: a head commit whose identity
// cannot be read is refused, never read as naming no one.
func TestCrewSubmitRefusesUnreadableHeadIdentity(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	h.repo.authorshipErr = errors.New("fatal: bad object")
	err := submitCrewForLanding(h.r)
	assertCrewRefused(t, h, err)
	if want := "author and committer of head commit"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q lacks %q", err, want)
	}
}
