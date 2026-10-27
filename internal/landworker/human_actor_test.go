package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// seedCrewReady seeds a crew session's submission: the READY TO LAND block a
// crew gt done writes carries no worker line, and the submission is recorded
// as a comment under the identity the session ran as.
func (h *harness) seedCrewReady(t *testing.T, id, author string) {
	t.Helper()
	w := land.Work{Branch: branch, Head: noteHead, Target: "main"}
	h.bd.Seed(beads.Issue{ID: id, Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/crew/overseer",
		Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w)})
	if err := h.bd.AddCommentAs(id, author, "Submitted for landing: "+branch+" @ "+noteHead+" onto main"); err != nil {
		t.Fatal(err)
	}
}

// readyToLand reports whether the bead still waits for its landing.
func (h *harness) readyToLand(t *testing.T, id string) bool {
	t.Helper()
	is, err := h.bd.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return beads.HasLabel(is, land.LabelReadyToLand)
}

// TestPassRefusesASubmissionByTheHumanIdentity: a crew submission recorded
// under the human's own identity is not landed, and the bead says why in one
// comment that names the actor and asks for an agent (gt-2ge3z).
func TestPassRefusesASubmissionByTheHumanIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedCrewReady(t, "gt-crew", humanActor)
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 || rep.Skipped != 1 {
		t.Fatalf("report %v, calls %d; want a skip and no landing", rep, len(h.lander.calls))
	}
	if !h.readyToLand(t, "gt-crew") {
		t.Error("the refused bead lost its ready label; it must keep waiting for an agent")
	}
	cs := h.comments(t, "gt-crew")
	if len(cs) != 2 {
		t.Fatalf("comments %q; want the submission and one refusal", cs)
	}
	if refusal := cs[len(cs)-1]; !strings.Contains(refusal, humanActor) || !strings.Contains(refusal, "An agent must submit it") {
		t.Errorf("refusal %q; want it to name %q and say an agent must submit it", refusal, humanActor)
	}
}

// TestPassRefusesAHumanSubmissionBesideAWorkerLine: the actor is the
// submission comment's author even when the block carries a worker line, so a
// human-signed crew submission cannot land on the strength of a worker line
// from another session (gt-2ge3z).
func TestPassRefusesAHumanSubmissionBesideAWorkerLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	if err := h.bd.AddCommentAs("gt-abc", humanActor, "Submitted for landing: "+branch+" @ "+noteHead+" onto main"); err != nil {
		t.Fatal(err)
	}
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 || rep.Skipped != 1 {
		t.Fatalf("report %v, calls %d; want a skip and no landing", rep, len(h.lander.calls))
	}
	cs := h.comments(t, "gt-abc")
	if len(cs) != 2 {
		t.Fatalf("comments %q; want the submission and one refusal", cs)
	}
	if refusal := cs[len(cs)-1]; !strings.Contains(refusal, humanActor) || !strings.Contains(refusal, "An agent must submit it") {
		t.Fatalf("refusal %q; want it to name %q and ask for an agent", refusal, humanActor)
	}
}

// TestPassRefusesASubmissionWithNoRecordedActor: an actor the worker cannot
// name never lands, because landing it would pass the human check by default.
func TestPassRefusesASubmissionWithNoRecordedActor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	w := land.Work{Branch: branch, Head: noteHead, Target: "main"}
	h.bd.Seed(beads.Issue{ID: "gt-anon", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/crew/overseer",
		Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w)})
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 || rep.Skipped != 1 {
		t.Fatalf("report %v, calls %d; want a skip and no landing", rep, len(h.lander.calls))
	}
	cs := h.comments(t, "gt-anon")
	if len(cs) != 1 || !strings.Contains(cs[0], "records no submitter") || !strings.Contains(cs[0], "An agent must submit it") {
		t.Fatalf("comments %q; want one refusal saying the bead records no submitter", cs)
	}
}

// TestPassAcceptsAnAgentWhoseNameContainsTheHumanName: only the whole
// identity is the human's; an agent whose name contains it is an agent, and
// its submission lands (gt-2ge3z).
func TestPassAcceptsAnAgentWhoseNameContainsTheHumanName(t *testing.T) {
	t.Parallel()
	for _, author := range []string{"sloan-runner", "Sloan Ahrens (agent)", "sloan"} {
		t.Run(author, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedCrewReady(t, "gt-crew", author)
			if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
				t.Fatalf("report %v; want the agent's submission landed", rep)
			}
			if got := h.lander.calls[0].Worker; got != "" {
				t.Errorf("landed worker %q; a crew landing has none", got)
			}
		})
	}
}

// TestPassRefusesWhenTheWorkerHasNoHumanIdentity: a worker built without the
// town's identity cannot tell a human's submission from an agent's, so it
// lands nothing rather than checking by default.
func TestPassRefusesWhenTheWorkerHasNoHumanIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.w.HumanActor = ""
	h.seedReady(t, "gt-abc")
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 || rep.Skipped != 1 {
		t.Fatalf("report %v, calls %d; want a skip and no landing", rep, len(h.lander.calls))
	}
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 || !strings.Contains(cs[0], "no human identity configured") {
		t.Fatalf("comments %q; want one refusal naming the missing identity", cs)
	}
}

// TestPassRefusalIsAnnotatedOnce: the refusal is one comment however often
// the worker re-checks a bead only that comment can unblock.
func TestPassRefusalIsAnnotatedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedCrewReady(t, "gt-crew", humanActor)
	h.w.Pass(context.Background())
	h.now = h.now.Add(humanWaitBackoff + time.Minute)
	h.w.Pass(context.Background())
	if cs := h.comments(t, "gt-crew"); len(cs) != 2 {
		t.Fatalf("comments %q; want the submission and the one refusal", cs)
	}
}

// errComments is a bead store whose comment read fails.
var errComments = errors.New("dolt: connection refused")

type unreadableComments struct {
	*beadsfake.Fake
}

func (unreadableComments) Comments(string) ([]beads.Comment, error) { return nil, errComments }

// TestPassLandsNothingWhenTheSubmissionRecordCannotBeRead: the actor is part
// of the submission record, so a comment read that fails is an infrastructure
// failure, never a submission that lands unchecked.
func TestPassLandsNothingWhenTheSubmissionRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.Beads = unreadableComments{h.bd}
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 {
		t.Fatalf("landed %+v; the submission's actor could not be read", h.lander.calls)
	}
	if rep.Failed != 1 {
		t.Fatalf("report %v; want one infrastructure failure", rep)
	}
	if !h.readyToLand(t, "gt-abc") {
		t.Error("the bead lost its ready label on a failure that landed nothing")
	}
}
