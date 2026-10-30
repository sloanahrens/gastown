package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// fakeRequeueRemote answers ListRemoteRefsWithHashes with one branch tip, or
// none when tip is empty. It records what it was asked, so a test can pin that
// requeue reads the rig's configured remote and the branch's full ref.
type fakeRequeueRemote struct {
	tip       string
	err       error
	gotRemote string
	gotRef    string
}

func (f *fakeRequeueRemote) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	f.gotRemote, f.gotRef = remote, prefix
	if f.err != nil {
		return nil, f.err
	}
	if f.tip == "" {
		return nil, nil
	}
	return []git.RemoteRef{{Hash: f.tip, Name: prefix}}, nil
}

const (
	requeueBranch = "polecat/mica/gt-3e1z4+abc"
	requeueHead   = "1111111111111111111111111111111111111111"
)

func requeueRejectionNote(branch, head string) string {
	return land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: "gate", Reason: "the gate refused it",
		Branch: branch, Target: "main", MR: "gt-3e1z4", Head: head,
	})
}

func seedReworkBead(bd *beadsfake.Fake, labels []string, notes string) {
	bd.Seed(beads.Issue{
		ID: "gt-3e1z4", Title: "the work", Type: "task", Status: string(beads.StatusOpen),
		Labels: labels, Notes: notes,
	})
}

func TestRequeueRejectedLandingRestoresTheQueue(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelRework}, requeueRejectionNote(requeueBranch, requeueHead))
	remote := &fakeRequeueRemote{tip: requeueHead}
	var out bytes.Buffer

	if err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "runner fault, not the diff", "sloan", &out); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	issue, err := bd.Show("gt-3e1z4")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s after a requeue", issue.Labels, land.LabelReadyToLand)
	}
	if beads.HasLabel(issue, land.LabelRework) {
		t.Errorf("labels %v still carry %s after a requeue", issue.Labels, land.LabelRework)
	}

	comments, err := bd.Comments("gt-3e1z4")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("wrote %d comments, want 1: %+v", len(comments), comments)
	}
	for _, want := range []string{"sloan", "runner fault, not the diff", requeueHead, requeueBranch} {
		if !strings.Contains(comments[0].Text, want) {
			t.Errorf("comment %q does not record %q", comments[0].Text, want)
		}
	}

	if remote.gotRemote != "origin" || remote.gotRef != "refs/heads/"+requeueBranch {
		t.Errorf("read remote=%q ref=%q, want origin %q", remote.gotRemote, remote.gotRef, "refs/heads/"+requeueBranch)
	}
	if !strings.Contains(out.String(), "Re-queued gt-3e1z4") {
		t.Errorf("output %q does not confirm the requeue", out.String())
	}
}

// TestRequeueRejectedLandingRecordsTheAttempt: the requeue comment names the
// rejection attempt it undid, which is what lets the landing worker tell a
// re-queued block from one an interrupted pass left and record the next
// rejection as a new attempt (gt-en9gs).
func TestRequeueRejectedLandingRecordsTheAttempt(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelRework}, land.FormatRejectionNote(land.RejectionNote{
		Attempt: 3, Kind: "gate", Reason: "red again",
		Branch: requeueBranch, Target: "main", MR: "gt-3e1z4", Head: requeueHead,
	}))
	remote := &fakeRequeueRemote{tip: requeueHead}

	if err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{}); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	comments, err := bd.Comments("gt-3e1z4")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 || !strings.Contains(comments[0].Text, "Attempt: 3") {
		t.Fatalf("requeue comment = %+v, want it to name attempt 3", comments)
	}
	if q, ok := land.NewestRequeue(comments); !ok || q.Attempt != 3 || q.Blocks != 1 {
		t.Errorf("NewestRequeue = %+v, %v; want attempt 3 of the bead's one rejection block", q, ok)
	}
}

func TestRequeueRejectedLandingRefusesChangedHead(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelRework}, requeueRejectionNote(requeueBranch, requeueHead))
	remote := &fakeRequeueRemote{tip: "2222222222222222222222222222222222222222"}

	err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "the head changed since the rejection") {
		t.Fatalf("changed head: err = %v, want a refusal naming the change", err)
	}
	wantStillRejected(t, bd)
}

func TestRequeueRejectedLandingRefusesMissingBranch(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelRework}, requeueRejectionNote(requeueBranch, requeueHead))
	remote := &fakeRequeueRemote{} // no tip: the branch is gone from the remote

	err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "is not on origin") {
		t.Fatalf("missing branch: err = %v, want a refusal naming the remote", err)
	}
	wantStillRejected(t, bd)
}

func TestRequeueRejectedLandingRefusesNotRejected(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		labels []string
		notes  string
		want   string
	}{
		"no rework label":   {nil, requeueRejectionNote(requeueBranch, requeueHead), "not in a rejected state"},
		"no rejection note": {[]string{land.LabelRework}, "Findings: look at the helper.\n", "not in a rejected state"},
		"rejection has no head": {[]string{land.LabelRework}, land.FormatRejectionNote(land.RejectionNote{
			Attempt: 1, Kind: "gate", Reason: "red", Branch: requeueBranch, Target: "main", MR: "gt-3e1z4",
		}), "names no head"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			seedReworkBead(bd, tc.labels, tc.notes)
			remote := &fakeRequeueRemote{tip: requeueHead}
			err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
			if remote.gotRef != "" {
				t.Errorf("a refusal read the remote (ref %q); the bead state is the first gate", remote.gotRef)
			}
		})
	}
}

func TestRequeueRejectedLandingRefusesAlreadyQueued(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelReadyToLand}, requeueRejectionNote(requeueBranch, requeueHead))
	remote := &fakeRequeueRemote{tip: requeueHead}

	err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "already carries gt:ready-to-land") {
		t.Fatalf("already queued: err = %v, want a refusal naming the label", err)
	}
}

func TestRequeueRejectedLandingSurfacesARemoteReadError(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedReworkBead(bd, []string{land.LabelRework}, requeueRejectionNote(requeueBranch, requeueHead))
	remote := &fakeRequeueRemote{err: errors.New("no such remote")}

	err := requeueRejectedLanding(bd, remote, "origin", "gt-3e1z4", "x", "sloan", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "reading refs/heads/"+requeueBranch+" from origin") {
		t.Fatalf("remote read error: err = %v, want it surfaced", err)
	}
}

// wantStillRejected: a refused requeue changed nothing — rework stays, the
// ready label is absent, and no requeue comment was written.
func wantStillRejected(t *testing.T, bd *beadsfake.Fake) {
	t.Helper()
	issue, err := bd.Show("gt-3e1z4")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !beads.HasLabel(issue, land.LabelRework) || beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("a refused requeue changed the labels: %v", issue.Labels)
	}
	comments, err := bd.Comments("gt-3e1z4")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("a refused requeue wrote a comment: %+v", comments)
	}
}

// requeueEnv is a getenv over a fixed map, so a case names only the variables
// it cares about.
func requeueEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestRequeueActor: the re-queue comment records the session's agent identity,
// resolved as gt done resolves one (gt-i817y) — BEADS_ACTOR, then BD_ACTOR,
// then a crew GT_ROLE — and never the host user, whose name is the human's, not
// the agent's. A session naming no agent writes the "unnamed-agent" placeholder
// rather than borrow a name it was not given.
func TestRequeueActor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"BEADS_ACTOR is the session's own identity",
			map[string]string{"BEADS_ACTOR": "town/crew/overseer"}, "town/crew/overseer"},
		{"BEADS_ACTOR leads BD_ACTOR and a crew GT_ROLE",
			map[string]string{"BEADS_ACTOR": "town/crew/overseer", "BD_ACTOR": "gastown/crew/sloan", "GT_ROLE": "gastown/crew/dunlin"},
			"town/crew/overseer"},
		{"BD_ACTOR leads a crew GT_ROLE",
			map[string]string{"BD_ACTOR": "town/crew/dunlin", "GT_ROLE": "gastown/crew/dunlin"},
			"town/crew/dunlin"},
		{"a crew GT_ROLE names its member",
			map[string]string{"GT_ROLE": "gastown/crew/dunlin"}, "gastown/crew/dunlin"},
		{"GT_POLECAT names the polecat when no crew identity is set",
			map[string]string{"GT_POLECAT": "imperator", "GT_ROLE": "gastown/polecats/imperator"}, "imperator"},
		{"a crew identity leads GT_POLECAT",
			map[string]string{"BEADS_ACTOR": "town/crew/overseer", "GT_POLECAT": "imperator"}, "town/crew/overseer"},
		{"no agent named is the placeholder",
			map[string]string{}, "unnamed-agent"},
		{"the host user is never the actor",
			map[string]string{"USER": "sloan"}, "unnamed-agent"},
		{"the host user does not override a crew identity",
			map[string]string{"USER": "sloan", "BEADS_ACTOR": "town/crew/overseer"}, "town/crew/overseer"},
		{"a blank GT_POLECAT names no one",
			map[string]string{"GT_POLECAT": "  ", "USER": "sloan"}, "unnamed-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := requeueActor(requeueEnv(tc.env)); got != tc.want {
				t.Errorf("requeueActor(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}
