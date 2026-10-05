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
