package beads_test

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These tests run the handoff, mail-sweep, molecule-attachment and
// merge-request helpers over beadsfake, the way any Client runs them now that
// they are free functions over beads.Client (gt-7iwy0.4.8). The helpers are
// exported from package beads, so the tests live in the external test package
// to import the fake without an import cycle.

// pinBead creates a bead and moves it to the pinned status the handoff and
// attachment helpers require.
func pinBead(t *testing.T, c beads.Client, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	issue, err := c.Create(opts)
	if err != nil {
		t.Fatalf("creating %q: %v", opts.Title, err)
	}
	pinned := beads.StatusPinned
	if err := c.Update(issue.ID, beads.UpdateOptions{Status: &pinned}); err != nil {
		t.Fatalf("pinning %s: %v", issue.ID, err)
	}
	return issue
}

// TestHandoffBeadsOverClient covers the handoff read/create/update/clear path
// the pinned-bead list and the single-role lookup both answer from.
func TestHandoffBeadsOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	created, err := beads.GetOrCreateHandoffBead(c, "mayor")
	if err != nil {
		t.Fatalf("GetOrCreateHandoffBead: %v", err)
	}
	if created.Title != "mayor Handoff" {
		t.Errorf("created title = %q, want %q", created.Title, "mayor Handoff")
	}
	if created.Status != beads.StatusPinned {
		t.Errorf("created status = %q, want %q", created.Status, beads.StatusPinned)
	}

	// A second call finds the bead it made rather than minting another.
	again, err := beads.GetOrCreateHandoffBead(c, "mayor")
	if err != nil {
		t.Fatalf("GetOrCreateHandoffBead (second): %v", err)
	}
	if again.ID != created.ID {
		t.Errorf("second call created %s, want the existing %s", again.ID, created.ID)
	}

	if err := beads.UpdateHandoffContent(c, "mayor", "left off here"); err != nil {
		t.Fatalf("UpdateHandoffContent: %v", err)
	}
	all, err := beads.FindAllHandoffBeads(c)
	if err != nil {
		t.Fatalf("FindAllHandoffBeads: %v", err)
	}
	if handoff := all["mayor"]; handoff == nil || handoff.Description != "left off here" {
		t.Errorf("FindAllHandoffBeads[mayor] = %+v, want the updated description", handoff)
	}

	if err := beads.ClearHandoffContent(c, "mayor"); err != nil {
		t.Fatalf("ClearHandoffContent: %v", err)
	}
	found, err := beads.FindHandoffBead(c, "mayor")
	if err != nil {
		t.Fatalf("FindHandoffBead: %v", err)
	}
	if found == nil || found.Description != "" {
		t.Errorf("FindHandoffBead after clear = %+v, want an empty description", found)
	}

	// A role with no handoff bead has nothing to clear, and that is not an
	// error.
	if err := beads.ClearHandoffContent(c, "nobody"); err != nil {
		t.Errorf("ClearHandoffContent(nobody) = %v, want nil", err)
	}
}

// TestClearMailOverClient covers the sweep's two arms: the open messages it
// closes, and the beads that are not messages at all.
func TestClearMailOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	message, err := c.Create(beads.CreateOptions{ID: "gt-msg-1", Title: "mail", Labels: []string{"gt:message"}, Priority: -1})
	if err != nil {
		t.Fatalf("creating message: %v", err)
	}
	if _, err := c.Create(beads.CreateOptions{ID: "gt-work-1", Title: "work", Labels: []string{"gt:task"}, Priority: -1}); err != nil {
		t.Fatalf("creating work bead: %v", err)
	}

	result, err := beads.ClearMail(c, "cleared during reset")
	if err != nil {
		t.Fatalf("ClearMail: %v", err)
	}
	if result.Closed != 1 {
		t.Errorf("ClearMail closed %d messages, want 1", result.Closed)
	}
	closed, err := c.Show(message.ID)
	if err != nil {
		t.Fatalf("Show(%s): %v", message.ID, err)
	}
	if closed.Status != string(beads.StatusClosed) {
		t.Errorf("%s status = %q after ClearMail, want closed", message.ID, closed.Status)
	}
	work, err := c.Show("gt-work-1")
	if err != nil {
		t.Fatalf("Show(gt-work-1): %v", err)
	}
	if work.Status == string(beads.StatusClosed) {
		t.Error("ClearMail closed a bead that is not a gt:message")
	}
}

// TestCloseStaleHookedMailBeadsOverClient pins the sweep to one agent's
// hooked mail: another agent's hooked mail, and a hooked bead that is not
// mail, both stay put.
func TestCloseStaleHookedMailBeadsOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	hooked := func(id, title string, labels []string, assignee string) {
		t.Helper()
		if _, err := c.Create(beads.CreateOptions{ID: id, Title: title, Labels: labels, Assignee: assignee, Priority: -1}); err != nil {
			t.Fatalf("creating %s: %v", id, err)
		}
		status := beads.StatusHooked
		if err := c.Update(id, beads.UpdateOptions{Status: &status}); err != nil {
			t.Fatalf("hooking %s: %v", id, err)
		}
	}
	hooked("gt-mine", "mine", []string{"gt:message"}, "gastown/mayor")
	hooked("gt-theirs", "theirs", []string{"gt:message"}, "gastown/witness")
	hooked("gt-task", "task", []string{"gt:task"}, "gastown/mayor")

	n, err := beads.CloseStaleHookedMailBeads(c, "gastown/mayor")
	if err != nil {
		t.Fatalf("CloseStaleHookedMailBeads: %v", err)
	}
	if n != 1 {
		t.Errorf("CloseStaleHookedMailBeads closed %d beads, want 1", n)
	}
	for id, want := range map[string]string{
		"gt-mine":   string(beads.StatusClosed),
		"gt-theirs": beads.StatusHooked,
		"gt-task":   beads.StatusHooked,
	} {
		issue, err := c.Show(id)
		if err != nil {
			t.Fatalf("Show(%s): %v", id, err)
		}
		if issue.Status != want {
			t.Errorf("%s status = %q, want %q", id, issue.Status, want)
		}
	}
}

// TestMoleculeAttachmentOverClient covers attach, read and both detach paths
// over a fake, and the pinned-only guard AttachMolecule keeps.
func TestMoleculeAttachmentOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	pinned := pinBead(t, c, beads.CreateOptions{ID: "gt-hook-1", Title: "handoff", Priority: -1})
	open, err := c.Create(beads.CreateOptions{ID: "gt-work-1", Title: "work", Priority: -1})
	if err != nil {
		t.Fatalf("creating work bead: %v", err)
	}

	if _, err := beads.AttachMolecule(c, open.ID, "gt-mol-1"); err == nil {
		t.Error("AttachMolecule accepted a bead that is not pinned")
	} else if !strings.Contains(err.Error(), "not pinned") {
		t.Errorf("AttachMolecule error = %v, want it to name the pinned requirement", err)
	}

	if _, err := beads.AttachMolecule(c, pinned.ID, "gt-mol-1"); err != nil {
		t.Fatalf("AttachMolecule: %v", err)
	}
	attachment, err := beads.GetAttachment(c, pinned.ID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if attachment == nil || attachment.AttachedMolecule != "gt-mol-1" {
		t.Fatalf("GetAttachment = %+v, want gt-mol-1 attached", attachment)
	}

	detached, err := beads.DetachMolecule(c, pinned.ID)
	if err != nil {
		t.Fatalf("DetachMolecule: %v", err)
	}
	if got := beads.ParseAttachmentFields(detached); got != nil {
		t.Errorf("DetachMolecule left %+v attached", got)
	}

	// Nothing left to detach: the bead comes back unchanged and the call is
	// still a success.
	if _, err := beads.DetachMolecule(c, pinned.ID); err != nil {
		t.Errorf("DetachMolecule with nothing attached = %v, want nil", err)
	}

	// The audited variant needs no audit log on a bare Client.
	if _, err := beads.AttachMolecule(c, pinned.ID, "gt-mol-2"); err != nil {
		t.Fatalf("AttachMolecule (second): %v", err)
	}
	if _, err := beads.DetachMoleculeWithAudit(c, pinned.ID, beads.DetachOptions{Operation: "burn", Agent: "marble"}); err != nil {
		t.Fatalf("DetachMoleculeWithAudit: %v", err)
	}
	after, err := beads.GetAttachment(c, pinned.ID)
	if err != nil {
		t.Fatalf("GetAttachment (after audit detach): %v", err)
	}
	if after != nil {
		t.Errorf("DetachMoleculeWithAudit left %+v attached", after)
	}
}

// TestMergeRequestLookupsOverClient covers the branch scan and the
// source_issue scan over a fake. Merge requests are wisps, as they are in the
// database, so the wisp arm of ListMergeRequests is what finds them.
func TestMergeRequestLookupsOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	createMR := func(branch, sourceIssue, status string) *beads.Issue {
		t.Helper()
		issue, err := c.Create(beads.CreateOptions{
			Title:       "MR for " + branch,
			Labels:      []string{"gt:merge-request"},
			Description: "branch: " + branch + "\nsource_issue: " + sourceIssue + "\n",
			Ephemeral:   true,
			Priority:    -1,
		})
		if err != nil {
			t.Fatalf("creating MR for %s: %v", branch, err)
		}
		if status != string(beads.StatusOpen) {
			if err := c.Update(issue.ID, beads.UpdateOptions{Status: &status}); err != nil {
				t.Fatalf("setting %s to %s: %v", issue.ID, status, err)
			}
		}
		return issue
	}
	openMR := createMR("polecat/toast/gt-work-1", "gt-work-1", string(beads.StatusOpen))
	createMR("polecat/nux/gt-work-2", "gt-work-2", string(beads.StatusClosed))

	mr, err := beads.FindMRForBranch(c, "polecat/toast/gt-work-1")
	if err != nil {
		t.Fatalf("FindMRForBranch: %v", err)
	}
	if mr == nil || mr.ID != openMR.ID {
		t.Errorf("FindMRForBranch = %+v, want %s", mr, openMR.ID)
	}

	// The open-only scan skips the closed bead; the branch scan matches the
	// full header line, not a prefix of another branch.
	if mr, err := beads.FindMRForBranch(c, "polecat/nux/gt-work-2"); err != nil || mr != nil {
		t.Errorf("FindMRForBranch(closed MR branch) = (%+v, %v), want (nil, nil)", mr, err)
	}

	open, err := beads.FindOpenMRsForIssue(c, "gt-work-1")
	if err != nil {
		t.Fatalf("FindOpenMRsForIssue: %v", err)
	}
	if len(open) != 1 || open[0].ID != openMR.ID {
		t.Errorf("FindOpenMRsForIssue(gt-work-1) = %v, want [%s]", mrIDs(open), openMR.ID)
	}
	// gt-work-2's only MR is closed, so it has no open attempt.
	if open, err := beads.FindOpenMRsForIssue(c, "gt-work-2"); err != nil || len(open) != 0 {
		t.Errorf("FindOpenMRsForIssue(gt-work-2) = (%v, %v), want none", mrIDs(open), err)
	}
	// A longer ID that shares a prefix is not a match.
	if open, err := beads.FindOpenMRsForIssue(c, "gt-work"); err != nil || len(open) != 0 {
		t.Errorf("FindOpenMRsForIssue(gt-work) = (%v, %v), want none", mrIDs(open), err)
	}
}

func mrIDs(issues []*beads.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.ID)
	}
	return out
}

// TestReadyDispatchableOverClient covers the Client path: the bookkeeping
// families the bd-backed store excludes server-side are dropped here.
func TestReadyDispatchableOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	if _, err := c.Create(beads.CreateOptions{ID: "gt-real", Title: "real work", Labels: []string{"gt:task"}, Priority: -1}); err != nil {
		t.Fatalf("creating work bead: %v", err)
	}
	if _, err := c.Create(beads.CreateOptions{ID: "gt-mail", Title: "mail", Labels: []string{"gt:message"}, Priority: -1}); err != nil {
		t.Fatalf("creating mail bead: %v", err)
	}

	issues, err := beads.ReadyDispatchable(c)
	if err != nil {
		t.Fatalf("ReadyDispatchable: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-real" {
		t.Errorf("ReadyDispatchable = %v, want [gt-real]", mrIDs(issues))
	}
}

// TestReadyForMolNeedsBdClient pins the documented boundary: a molecule's
// ready set comes from bd's ready --mol query, which no Client method reaches.
func TestReadyForMolNeedsBdClient(t *testing.T) {
	t.Parallel()
	if _, err := beads.ReadyForMol(beadsfake.New(), "gt-mol-1"); err == nil {
		t.Fatal("ReadyForMol over beadsfake = nil error, want the bd-backed Client requirement")
	}
}
