package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// noStepsPhrase is the part of the reader line the bead names, asserted on its
// own so a reworded constant cannot pass the test unnoticed.
const noStepsPhrase = "no steps poured (root-only)"

// hookedTo returns a pinned bead attached to the molecule rootID.
func hookedTo(rootID string) *beads.Issue {
	return &beads.Issue{
		ID:    "gt-hook",
		Title: "work",
		Description: beads.SetAttachmentFields(&beads.Issue{}, &beads.AttachmentFields{
			AttachedMolecule: rootID,
		}),
	}
}

// TestMoleculeNoStepsProgressInfo: a molecule root that poured no step beads
// reports NoSteps instead of reading as a childless plain issue; a plain task
// with no children still reads as not a molecule at all (gt-thyafn).
func TestMoleculeNoStepsProgressInfo(t *testing.T) {
	t.Parallel()

	t.Run("molecule root with no children", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		progress, err := getMoleculeProgressInfo(fake, root.ID)
		if err != nil {
			t.Fatalf("getMoleculeProgressInfo(%s) = %v, want no error", root.ID, err)
		}
		if progress == nil {
			t.Fatalf("progress = nil, want a root-only report for %s", root.ID)
		}
		if !progress.NoSteps {
			t.Errorf("NoSteps = false, want true")
		}
		if progress.TotalSteps != 0 || progress.Complete {
			t.Errorf("progress = %+v, want 0 steps and not complete", progress)
		}
		if progress.RootID != root.ID || progress.RootTitle != root.Title {
			t.Errorf("progress = %+v, want root %s %q", progress, root.ID, root.Title)
		}
	})

	t.Run("plain task with no children", func(t *testing.T) {
		t.Parallel()

		fake := beadsfake.New()
		fake.Seed(beads.Issue{ID: "gt-task-plain", Title: "a plain task"})

		progress, err := getMoleculeProgressInfo(fake, "gt-task-plain")
		if err != nil {
			t.Fatalf("getMoleculeProgressInfo = %v, want no error", err)
		}
		if progress != nil {
			t.Errorf("progress = %+v, want nil: a plain task is not a molecule", progress)
		}
	})
}

// TestMoleculeNoStepsPinnedCompletion: a pinned bead whose molecule poured no
// steps is not complete, so it does not pass as done; a molecule whose steps
// are all closed still does (gt-thyafn).
func TestMoleculeNoStepsPinnedCompletion(t *testing.T) {
	t.Parallel()

	t.Run("root-only molecule is not complete", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		complete, attached := checkPinnedBeadComplete(fake, hookedTo(root.ID))
		if complete {
			t.Error("checkPinnedBeadComplete = true, want false for a molecule with no poured steps")
		}
		if !attached {
			t.Error("hasAttachment = false, want true")
		}
	})

	t.Run("molecule with every step closed is complete", func(t *testing.T) {
		t.Parallel()

		fake := beadsfake.New()
		fake.Seed(beads.Issue{ID: "gt-wisp-done", Title: "mol-polecat-work", Type: moleculeIssueType})
		step, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: "gt-wisp-done"})
		if err != nil {
			t.Fatalf("create step: %v", err)
		}
		if err := fake.Close(step.ID); err != nil {
			t.Fatalf("close step: %v", err)
		}

		complete, attached := checkPinnedBeadComplete(fake, hookedTo("gt-wisp-done"))
		if !complete {
			t.Error("checkPinnedBeadComplete = false, want true when every step is closed")
		}
		if !attached {
			t.Error("hasAttachment = false, want true")
		}
	})
}

// TestMoleculeNoStepsProgressPrintsTheLine: gt mol progress on a root-only
// molecule prints the not-tracked line and exits 0, and --json reports
// no_steps rather than 0% or an error (gt-thyafn).
func TestMoleculeNoStepsProgressPrintsTheLine(t *testing.T) {
	t.Parallel()

	t.Run("human", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		var out bytes.Buffer

		err := moleculeProgressRun{out: &out, client: fake, rootID: root.ID}.run()
		if err != nil {
			t.Fatalf("gt mol progress %s = %v, want nil", root.ID, err)
		}
		if !strings.Contains(out.String(), noStepsPhrase) {
			t.Errorf("output = %q, want the phrase %q", out.String(), noStepsPhrase)
		}
		if strings.Contains(out.String(), "░") || strings.Contains(out.String(), "%") {
			t.Errorf("output = %q, want no progress bar for a molecule with no steps", out.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		var out bytes.Buffer

		err := moleculeProgressRun{out: &out, client: fake, rootID: root.ID, asJSON: true}.run()
		if err != nil {
			t.Fatalf("gt mol progress --json %s = %v, want nil", root.ID, err)
		}
		var progress MoleculeProgressInfo
		if err := json.Unmarshal(out.Bytes(), &progress); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, out.String())
		}
		if !progress.NoSteps {
			t.Errorf("no_steps = false in %s, want true", out.String())
		}
		if progress.TotalSteps != 0 || progress.Complete {
			t.Errorf("progress = %+v, want 0 steps and not complete", progress)
		}
	})
}

// TestMoleculeNoStepsDagPrintsTheLine: gt mol dag on a root-only molecule
// prints the not-tracked line and exits 0 instead of failing with "no steps
// found"; --json reports no_steps (gt-thyafn).
func TestMoleculeNoStepsDagPrintsTheLine(t *testing.T) {
	t.Parallel()

	t.Run("human", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		var out bytes.Buffer

		err := moleculeDagRun{out: &out, client: fake, rootID: root.ID}.run()
		if err != nil {
			t.Fatalf("gt mol dag %s = %v, want nil", root.ID, err)
		}
		if !strings.Contains(out.String(), noStepsPhrase) {
			t.Errorf("output = %q, want the phrase %q", out.String(), noStepsPhrase)
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		var out bytes.Buffer

		err := moleculeDagRun{out: &out, client: fake, rootID: root.ID, asJSON: true}.run()
		if err != nil {
			t.Fatalf("gt mol dag --json %s = %v, want nil", root.ID, err)
		}
		var dag DAGInfo
		if err := json.Unmarshal(out.Bytes(), &dag); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, out.String())
		}
		if !dag.NoSteps {
			t.Errorf("no_steps = false in %s, want true", out.String())
		}
	})
}

// TestMoleculeNoStepsReadersRejectAChildlessNonMolecule: a bead that is not a
// molecule root keeps the "no steps found" refusal, so a mistyped id never
// reads as a root-only molecule (gt-thyafn).
func TestMoleculeNoStepsReadersRejectAChildlessNonMolecule(t *testing.T) {
	t.Parallel()

	fake := beadsfake.New()
	fake.Seed(beads.Issue{ID: "gt-task-plain", Title: "a plain task"})

	t.Run("progress", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		err := moleculeProgressRun{out: &out, client: fake, rootID: "gt-task-plain"}.run()
		if err == nil || !strings.Contains(err.Error(), "no steps found") {
			t.Errorf("gt mol progress on a childless plain task = %v, want the no-steps refusal", err)
		}
	})

	t.Run("dag", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		err := moleculeDagRun{out: &out, client: fake, rootID: "gt-task-plain"}.run()
		if err == nil || !strings.Contains(err.Error(), "no steps found") {
			t.Errorf("gt mol dag on a childless plain task = %v, want the no-steps refusal", err)
		}
	})
}

// TestMoleculeNoStepsProgressReadFailure: gt mol progress hands back the failed
// read of the open steps instead of plotting them from an empty map (gt-thyafn).
func TestMoleculeNoStepsProgressReadFailure(t *testing.T) {
	t.Parallel()

	readErr := errors.New("dial tcp 127.0.0.1:3307: connection refused")

	fake := beadsfake.New()
	fake.Seed(beads.Issue{ID: "gt-wisp-abc", Title: "mol-polecat-work", Type: moleculeIssueType})
	if _, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: "gt-wisp-abc"}); err != nil {
		t.Fatalf("create step: %v", err)
	}

	var out bytes.Buffer
	err := moleculeProgressRun{
		out:    &out,
		client: brokenReadClient{Client: fake, showMultiple: true, err: readErr},
		rootID: "gt-wisp-abc",
	}.run()
	if err == nil || !strings.Contains(err.Error(), readErr.Error()) {
		t.Errorf("gt mol progress with a failed step read = %v, want it to carry %q", err, readErr)
	}
}

// TestMoleculeNoStepsStatusPrintsTheLine: gt mol status over a hooked bead
// whose molecule poured no steps names the formula as the next action and
// draws no progress bar (gt-thyafn). applyHookAttachment and
// outputMoleculeStatus are the read and the print runMoleculeStatus reaches.
func TestMoleculeNoStepsStatusPrintsTheLine(t *testing.T) {
	t.Parallel()

	fake, root := rootOnlyMolecule(t)
	hook := hookedTo(root.ID)

	status := MoleculeStatusInfo{Target: "gastown/polecats/dementus", HasWork: true, PinnedBead: hook}
	var warn bytes.Buffer
	applyHookAttachment(&warn, fake, &status, hook)

	if status.Progress == nil || !status.Progress.NoSteps {
		t.Fatalf("Progress = %+v, want a root-only report", status.Progress)
	}
	if status.NextAction != noStepsPouredLine {
		t.Errorf("NextAction = %q, want %q", status.NextAction, noStepsPouredLine)
	}
	if warn.Len() != 0 {
		t.Errorf("warning = %q, want nothing", warn.String())
	}

	var out bytes.Buffer
	outputMoleculeStatus(&out, status, func() {})
	if !strings.Contains(out.String(), noStepsPhrase) {
		t.Errorf("output = %q, want the phrase %q", out.String(), noStepsPhrase)
	}
	if strings.Contains(out.String(), "Progress:") {
		t.Errorf("output = %q, want no progress bar for a molecule with no steps", out.String())
	}
}

// TestMoleculeNoStepsSquashDigest: the squash digest of a molecule that
// poured no steps carries no execution summary, since it would read 0/0
// steps, partial; a molecule with steps still carries one (gt-thyafn).
func TestMoleculeNoStepsSquashDigest(t *testing.T) {
	t.Parallel()

	t.Run("root-only molecule", func(t *testing.T) {
		t.Parallel()

		db := squashDB(t, "gt-wisp-rootonly")
		squashForDigest(t, db)
		desc := digestDescription(t, db, "gt-wisp-rootonly")
		if strings.Contains(desc, "Execution Summary") {
			t.Errorf("digest = %q, want no Execution Summary for a molecule with no steps", desc)
		}
		if strings.Contains(desc, "0/0") {
			t.Errorf("digest = %q, want no 0/0 step count", desc)
		}
	})

	t.Run("molecule with steps", func(t *testing.T) {
		t.Parallel()

		db := squashDB(t, "gt-wisp-stepped",
			beads.Issue{ID: "gt-step-1", Title: "Step 1", Status: "closed"},
			beads.Issue{ID: "gt-step-2", Title: "Step 2", Status: "open"})
		squashForDigest(t, db)
		desc := digestDescription(t, db, "gt-wisp-stepped")
		if !strings.Contains(desc, "Execution Summary") || !strings.Contains(desc, "Steps: 1/2 completed") {
			t.Errorf("digest = %q, want an Execution Summary reading Steps: 1/2 completed", desc)
		}
	})
}

// squashDB is the store a squash over a witness handoff bead pinned to a
// molecule-typed root sees, with steps as the root's poured children.
func squashDB(t *testing.T, rootID string, steps ...beads.Issue) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-handoff-1", Title: "witness Handoff", Status: string(beads.StatusPinned), Description: "attached_molecule: " + rootID},
		beads.Issue{ID: rootID, Title: "mol-polecat-work", Status: string(beads.StatusHooked), Type: moleculeIssueType, Ephemeral: true},
	)
	seedChildren(t, db, rootID, steps...)
	return db
}

// squashForDigest runs gt mol squash for witness over db with its digest on.
func squashForDigest(t *testing.T, db *beadsfake.Fake) {
	t.Helper()
	e := testMoleculeEnv(t.TempDir(), &doneRecorder{Client: db, mu: &sync.Mutex{}, closes: &[]string{}})
	e.noDigest = false
	if err := moleculeSquash(squashCmd(context.Background()), e, []string{"witness"}); err != nil {
		t.Fatalf("squash: %v", err)
	}
}

// digestDescription returns the description of the digest the squash wrote
// for rootID.
func digestDescription(t *testing.T, db *beadsfake.Fake, rootID string) string {
	t.Helper()
	digests, err := db.List(beads.ListOptions{Ephemeral: true, Status: "all", Priority: -1})
	if err != nil {
		t.Fatalf("listing digests: %v", err)
	}
	for _, is := range digests {
		if is.Title == "Digest: "+rootID {
			return is.Description
		}
	}
	t.Fatalf("no digest written for %s", rootID)
	return ""
}

// TestMoleculeNoStepsHookRefusesWithoutForce: a pinned bead attached to a
// molecule that poured no steps is not complete, so gt hook refuses it without
// --force (the switch's refusal branch) and replaces it with it; a completed
// molecule is replaced without --force (gt-thyafn).
func TestMoleculeNoStepsHookRefusesWithoutForce(t *testing.T) {
	t.Parallel()

	t.Run("root-only molecule without --force is refused", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		action, _ := hookReplacementActionFor(fake, hookedTo(root.ID), false)
		if action != hookRefuseIncomplete {
			t.Errorf("action = %v, want the incomplete refusal without --force", action)
		}
	})

	t.Run("root-only molecule under --force is replaced", func(t *testing.T) {
		t.Parallel()

		fake, root := rootOnlyMolecule(t)
		action, attached := hookReplacementActionFor(fake, hookedTo(root.ID), true)
		if action != hookForceReplace {
			t.Errorf("action = %v, want the forced replacement under --force", action)
		}
		if !attached {
			t.Error("hasAttachment = false, want true: the displaced bead is a molecule")
		}
	})

	t.Run("completed molecule is closed", func(t *testing.T) {
		t.Parallel()

		fake := beadsfake.New()
		fake.Seed(beads.Issue{ID: "gt-wisp-done", Title: "mol-polecat-work", Type: moleculeIssueType})
		step, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: "gt-wisp-done"})
		if err != nil {
			t.Fatalf("create step: %v", err)
		}
		if err := fake.Close(step.ID); err != nil {
			t.Fatalf("close step: %v", err)
		}

		action, attached := hookReplacementActionFor(fake, hookedTo("gt-wisp-done"), false)
		if action != hookReplaceCompleted || !attached {
			t.Errorf("action = %v, attached = %v, want a completed molecule closed", action, attached)
		}
	})

	t.Run("bead with no molecule is released", func(t *testing.T) {
		t.Parallel()

		action, attached := hookReplacementActionFor(beadsfake.New(), &beads.Issue{ID: "gt-task", Title: "a plain task"}, false)
		if action != hookReplaceCompleted || attached {
			t.Errorf("action = %v, attached = %v, want the naked bead released, not closed", action, attached)
		}
	})
}
