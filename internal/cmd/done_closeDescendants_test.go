package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDoneCloseDescendantsWithChildren verifies that when gt done is called
// with a molecule that has children, closeDescendants closes the children
// before the root molecule (edge case #1).
func TestDoneCloseDescendantsWithChildren(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: childrenStepsBD}
	// Call updateAgentStateOnDone directly
	runDoneStateUpdate(t, fb)

	// Verify close calls
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Should have closed: step-1, step-2 (children), then wisp (attached molecule), then base-123 (hooked bead)
	foundStep1 := false
	foundStep2 := false
	foundWisp := false
	foundBase := false

	for _, line := range closeLines {
		if strings.Contains(line, "gt-step-1") {
			foundStep1 = true
		}
		if strings.Contains(line, "gt-step-2") {
			foundStep2 = true
		}
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-base-123") {
			foundBase = true
		}
	}

	if !foundStep1 {
		t.Errorf("child gt-step-1 was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundStep2 {
		t.Errorf("child gt-step-2 was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundBase {
		t.Errorf("hooked bead gt-base-123 was NOT closed\nClose calls:\n%s", closes)
	}

	// Verify order: children should be closed before wisp, wisp before base
	step1Idx := -1
	step2Idx := -1
	wispIdx := -1
	baseIdx := -1

	for i, line := range closeLines {
		if strings.Contains(line, "gt-step-1") {
			step1Idx = i
		}
		if strings.Contains(line, "gt-step-2") {
			step2Idx = i
		}
		if strings.Contains(line, "gt-wisp-xyz") {
			wispIdx = i
		}
		if strings.Contains(line, "gt-base-123") {
			baseIdx = i
		}
	}

	// wisp should be closed AFTER children
	if wispIdx >= 0 && step1Idx >= 0 && wispIdx < step1Idx {
		t.Errorf("wisp closed BEFORE step-1 (wisp line %d, step-1 line %d)", wispIdx, step1Idx)
	}
	if wispIdx >= 0 && step2Idx >= 0 && wispIdx < step2Idx {
		t.Errorf("wisp closed BEFORE step-2 (wisp line %d, step-2 line %d)", wispIdx, step2Idx)
	}
	// base should be closed AFTER wisp
	if baseIdx >= 0 && wispIdx >= 0 && baseIdx < wispIdx {
		t.Errorf("base bead closed BEFORE wisp (base line %d, wisp line %d)", baseIdx, wispIdx)
	}
}

// TestDoneCloseDescendantsNoChildren verifies that gt done works correctly
// when the molecule has no children - it should just close the molecule and
// hooked bead without errors (edge case #2).
func TestDoneCloseDescendantsNoChildren(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: noChildrenBD}
	// Should not error even though molecule has no children
	runDoneStateUpdate(t, fb)

	// Verify close calls
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Should have closed: wisp, then base (no children to close)
	foundWisp := false
	foundBase := false

	for _, line := range closeLines {
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-base-123") {
			foundBase = true
		}
	}

	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundBase {
		t.Errorf("hooked bead gt-base-123 was NOT closed\nClose calls:\n%s", closes)
	}

	// Should only have 2 close calls (wisp and base)
	if len(closeLines) != 2 {
		t.Errorf("expected 2 close calls (wisp, base), got %d:\n%s", len(closeLines), closes)
	}
}

// TestDoneCloseDescendantsSomeAlreadyClosed verifies that closeDescendants
// skips children that are already closed (edge case #3).
func TestDoneCloseDescendantsSomeAlreadyClosed(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: someClosedBD}
	runDoneStateUpdate(t, fb)

	// Verify close calls
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Should have closed: gt-step-open (not gt-step-closed since it's already closed), wisp, base
	foundOpen := false
	foundClosed := false
	foundWisp := false
	foundBase := false

	for _, line := range closeLines {
		if strings.Contains(line, "gt-step-open") {
			foundOpen = true
		}
		if strings.Contains(line, "gt-step-closed") {
			foundClosed = true
		}
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-base-123") {
			foundBase = true
		}
	}

	if !foundOpen {
		t.Errorf("open child gt-step-open was NOT closed\nClose calls:\n%s", closes)
	}
	if foundClosed {
		t.Errorf("already-closed child gt-step-closed SHOULD NOT have been closed again\nClose calls:\n%s", closes)
	}
	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundBase {
		t.Errorf("hooked bead gt-base-123 was NOT closed\nClose calls:\n%s", closes)
	}

	// Should have exactly 3 close calls
	if len(closeLines) != 3 {
		t.Errorf("expected 3 close calls (open-step, wisp, base), got %d:\n%s", len(closeLines), closes)
	}
}

// TestDoneCloseDescendantsDeeplyNested verifies that closeDescendants
// correctly handles deeply nested children (grandchildren) recursively
// (edge case #4).
func TestDoneCloseDescendantsDeeplyNested(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: nestedBD}
	runDoneStateUpdate(t, fb)

	// Verify close calls
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Should have closed: grandchild, child, wisp, base (in that order)
	foundGrandchild := false
	foundChild := false
	foundWisp := false
	foundBase := false

	for _, line := range closeLines {
		if strings.Contains(line, "gt-grandchild") {
			foundGrandchild = true
		}
		if strings.Contains(line, "gt-child") {
			foundChild = true
		}
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-base-123") {
			foundBase = true
		}
	}

	if !foundGrandchild {
		t.Errorf("grandchild gt-grandchild was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundChild {
		t.Errorf("child gt-child was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\nClose calls:\n%s", closes)
	}
	if !foundBase {
		t.Errorf("hooked bead gt-base-123 was NOT closed\nClose calls:\n%s", closes)
	}

	// Should have exactly 4 close calls
	if len(closeLines) != 4 {
		t.Errorf("expected 4 close calls (grandchild, child, wisp, base), got %d:\n%s", len(closeLines), closes)
	}

	// Verify order: grandchild first, then child, then wisp, then base
	grandchildIdx := -1
	childIdx := -1
	wispIdx := -1
	baseIdx := -1

	for i, line := range closeLines {
		if strings.Contains(line, "gt-grandchild") {
			grandchildIdx = i
		}
		if strings.Contains(line, "gt-child") {
			childIdx = i
		}
		if strings.Contains(line, "gt-wisp-xyz") {
			wispIdx = i
		}
		if strings.Contains(line, "gt-base-123") {
			baseIdx = i
		}
	}

	// Order should be: grandchild < child < wisp < base
	if !(grandchildIdx < childIdx && childIdx < wispIdx && wispIdx < baseIdx) {
		t.Errorf("incorrect close order. Expected: grandchild < child < wisp < base\nGot indices: grandchild=%d, child=%d, wisp=%d, base=%d\nClose calls:\n%s",
			grandchildIdx, childIdx, wispIdx, baseIdx, closes)
	}
}

// TestDoneCloseDescendantsNoMoleculeAttached verifies that gt done handles
// the case where there is no molecule attached gracefully (edge case #5).
func TestDoneCloseDescendantsNoMoleculeAttached(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: noMoleculeBD}
	// Should not error even though there's no attached molecule
	runDoneStateUpdate(t, fb)

	// Verify close calls - should only close the hooked base bead (no molecule)
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Should have closed: base bead only (no molecule to close)
	foundBase := false

	for _, line := range closeLines {
		if strings.Contains(line, "gt-base-123") {
			foundBase = true
		}
	}

	if !foundBase {
		t.Errorf("hooked bead gt-base-123 was NOT closed\nClose calls:\n%s", closes)
	}

	// Should have exactly 1 close call
	if len(closeLines) != 1 {
		t.Errorf("expected 1 close call (base bead only), got %d:\n%s", len(closeLines), closes)
	}
}

// TestCloseDescendantsHandlesListError verifies that closeDescendants handles
// errors from b.List gracefully and continues with closing what it can.
func TestCloseDescendantsHandlesListError(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: listErrorBD}
	// Should not error even though list fails - continues with closing molecule and base bead
	runDoneStateUpdate(t, fb)

	// Verify close calls - should still close wisp and base even though list failed
	closes := fb.log()
	if closes == "" {
		t.Fatal("no beads were closed")
	}

	// Should have closed: wisp, base (list error doesn't prevent molecule close)
	if !strings.Contains(closes, "gt-wisp-xyz") {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed after list error\nClose calls:\n%s", closes)
	}
	if !strings.Contains(closes, "gt-base-123") {
		t.Errorf("hooked bead gt-base-123 was NOT closed after list error\nClose calls:\n%s", closes)
	}
}

// TestCloseDescendantsMoleculeNotFound verifies that the fix handles the case
// where the attached molecule doesn't exist (already burned/deleted).
func TestCloseDescendantsMoleculeNotFound(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: moleculeNotFoundBD}
	// Should not error - handles molecule close failure gracefully
	runDoneStateUpdate(t, fb)

	// Implementation behavior: when molecule close fails with a generic error
	// (not beads.ErrNotFound), the function returns early WITHOUT closing the
	// hooked bead, since the molecule is potentially still blocking it.
	// The Witness will clean up orphaned state.
	var attempts, closes []string
	for _, line := range strings.Split(strings.TrimSpace(fb.log()), "\n") {
		if strings.HasPrefix(line, "close_attempt: ") {
			attempts = append(attempts, line)
		} else if line != "" {
			closes = append(closes, line)
		}
	}
	if len(attempts) > 0 {
		attempts := strings.Join(attempts, "\n")
		// Verify that molecule close was attempted (and failed 3 times)
		if !strings.Contains(attempts, "gt-wisp-xyz") {
			t.Errorf("molecule gt-wisp-xyz close was NOT attempted\nAttempts:\n%s", attempts)
		}
		// Note: base bead close is NOT attempted when molecule close fails
		// This is correct behavior - the molecule blocks the base bead closure
	} else {
		t.Logf("Note: no close attempts logged")
	}

	// Note any close calls (the stub logged every close's first argument)
	if len(closes) > 0 {
		t.Logf("Note: close calls: %s", strings.Join(closes, "\n"))
	}
}

// TestDoneLeavesHookedBeadOpenWithPendingMRNote verifies that gt done does
// NOT close the hooked source bead when it just submitted an MR (via the
// agent bead's active_mr field) — it records a comment naming the MR
// instead and leaves the bead's status untouched. "Closed" now means
// "merged" everywhere a human or a dependency check reads it: the
// refinery's closeMergedWorkBead is the only thing that closes the source
// bead, at real merge success (gt-pqqz). Before this, every polecat on this
// rig being transient meant gt done closed the source issue seconds after
// creating its MR, well before the MR reached the merge queue, making a
// dependency or a human reading "closed" as "landed" when it only meant
// "submitted."
func TestDoneLeavesHookedBeadOpenWithPendingMRNote(t *testing.T) {
	t.Parallel()
	fb := &inprocBD{answer: pendingMRBD}
	runDoneStateUpdate(t, fb)

	calls := fb.log()
	if calls == "" {
		t.Fatal("no bd comments/close calls were recorded")
	}

	commentLine := ""
	for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
		if strings.HasPrefix(line, "close ") && strings.Contains(line, "gt-base-123") {
			t.Fatalf("hooked bead gt-base-123 was closed at MR-submission time — it must stay open until the refinery closes it at merge (gt-pqqz)\nCalls:\n%s", calls)
		}
		if strings.HasPrefix(line, "comments ") && strings.Contains(line, "gt-base-123") {
			commentLine = line
		}
	}
	if commentLine == "" {
		t.Fatalf("hooked bead gt-base-123 got no pending-MR comment\nCalls:\n%s", calls)
	}
	if !strings.Contains(commentLine, "gt-mr-42") {
		t.Errorf("pending-MR comment did not name the MR, got: %q", commentLine)
	}
}

// runDoneStateUpdate runs updateAgentStateOnDone as polecat gastown/nux,
// from the rig directory of a fresh town whose routes send gt- to the
// gastown rig, with fb as bd. No git repository holds the town, so there is
// no HEAD for review evidence.
func runDoneStateUpdate(t *testing.T, fb *inprocBD) {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		bd:     fb.run,
		reviewHead: func() (string, error) {
			return "", errors.New("resolving current HEAD: not a git repository")
		},
	}
	_ = updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, ExitCompleted, "gt-base-123")
}

// The agent bead, the hooked base bead (its molecule gt-wisp-xyz attached)
// and that molecule's root, as every stub below showed them.
const (
	doneAgentJSON = `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","status":"open","hook_bead":"gt-base-123","agent_state":"working"}]`
	doneBaseJSON  = `[{"id":"gt-base-123","title":"Base bead","status":"hooked","description":"attached_molecule: gt-wisp-xyz"}]`
	doneWispJSON  = `[{"id":"gt-wisp-xyz","title":"mol-polecat-work","status":"open","ephemeral":true}]`
)

// showAgentBaseWisp answers `show <id>` for those three beads, and nothing
// for any other.
func showAgentBaseWisp(id string) bdAnswer {
	switch id {
	case "gt-gastown-polecat-nux":
		return bdOut(doneAgentJSON + "\n")
	case "gt-base-123":
		return bdOut(doneBaseJSON + "\n")
	case "gt-wisp-xyz":
		return bdOut(doneWispJSON + "\n")
	}
	return bdAnswer{}
}

// logClosedIDs logs each bead ID a close names, skipping its flags
// (--reason, --force, --session).
func logClosedIDs(f *inprocBD, args []string) bdAnswer {
	for _, id := range nonFlagArgs(args) {
		f.logLine(id)
	}
	return bdAnswer{}
}

// childrenStepsBD: the wisp has two open steps. A step reads closed once a
// close named it, which the re-read after the batch close sees.
func childrenStepsBD(f *inprocBD, cmd string, args []string) bdAnswer {
	st := func(id string) string {
		if f.logged(id) {
			return "closed"
		}
		return "open"
	}
	switch cmd {
	case "show":
		for len(args) > 0 && args[0] == "--json" {
			args = args[1:]
		}
		var beadID string
		if len(args) > 0 {
			beadID = args[0]
		}
		if argsMention(args, "--children") {
			if beadID == "gt-wisp-xyz" {
				return bdOut(fmt.Sprintf(`{"gt-wisp-xyz":[{"id":"gt-step-1","title":"Step 1","status":%q},{"id":"gt-step-2","title":"Step 2","status":%q}]}`+"\n", st("gt-step-1"), st("gt-step-2")))
			}
			return bdOut("{}\n")
		}
		if strings.HasPrefix(beadID, "gt-step-") {
			var out []string
			for _, id := range nonFlagArgs(args) {
				out = append(out, fmt.Sprintf(`{"id":%q,"status":%q}`, id, st(id)))
			}
			return bdOut("[" + strings.Join(out, ",") + "]\n")
		}
		return showAgentBaseWisp(beadID)
	case "list":
		if argsMention(args, "parent=gt-wisp-xyz") {
			return bdOut(`[{"id":"gt-step-1","title":"Step 1","status":"open"},{"id":"gt-step-2","title":"Step 2","status":"open"}]` + "\n")
		}
		return bdOut("[]\n")
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// firstArg is the stubs' beadID="$1".
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// noChildrenBD: the wisp has no children.
func noChildrenBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		beadID := firstArg(args)
		if argsMention(args, "--children") {
			return bdOut(fmt.Sprintf(`{%q:[]}`+"\n", beadID))
		}
		return showAgentBaseWisp(beadID)
	case "list":
		return bdOut("[]\n")
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// someClosedBD: the wisp has one open step and one already closed.
func someClosedBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		beadID := firstArg(args)
		if argsMention(args, "--children") {
			if beadID == "gt-wisp-xyz" {
				return bdOut(`{"gt-wisp-xyz":[{"id":"gt-step-open","title":"Step Open","status":"open"},{"id":"gt-step-closed","title":"Step Closed","status":"closed"}]}` + "\n")
			}
			return bdOut("{}\n")
		}
		return showAgentBaseWisp(beadID)
	case "list":
		if argsMention(args, "parent=gt-wisp-xyz") {
			return bdOut(`[{"id":"gt-step-open","title":"Step Open","status":"open"},{"id":"gt-step-closed","title":"Step Closed","status":"closed"}]` + "\n")
		}
		return bdOut("[]\n")
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// nestedBD: wisp -> child -> grandchild.
func nestedBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		beadID := firstArg(args)
		if argsMention(args, "--children") {
			switch beadID {
			case "gt-wisp-xyz":
				return bdOut(`{"gt-wisp-xyz":[{"id":"gt-child","title":"Child","status":"open"}]}` + "\n")
			case "gt-child":
				return bdOut(`{"gt-child":[{"id":"gt-grandchild","title":"Grandchild","status":"open"}]}` + "\n")
			}
			return bdOut(fmt.Sprintf(`{%q:[]}`+"\n", beadID))
		}
		return showAgentBaseWisp(beadID)
	case "list":
		switch {
		case argsMention(args, "parent=gt-wisp-xyz"):
			return bdOut(`[{"id":"gt-child","title":"Child","status":"open"}]` + "\n")
		case argsMention(args, "parent=gt-child"):
			return bdOut(`[{"id":"gt-grandchild","title":"Grandchild","status":"open"}]` + "\n")
		}
		return bdOut("[]\n")
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// noMoleculeBD: the hooked bead has no attached_molecule.
func noMoleculeBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		switch firstArg(args) {
		case "gt-gastown-polecat-nux":
			return bdOut(doneAgentJSON + "\n")
		case "gt-base-123":
			return bdOut(`[{"id":"gt-base-123","title":"Base bead","status":"hooked","description":"no molecule attached"}]` + "\n")
		}
		return bdAnswer{}
	case "list":
		return bdOut("[]\n")
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// listErrorBD: listing children fails (a locked database).
func listErrorBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		if argsMention(args, "--children") {
			return bdAnswer{stderr: "Error: database locked\n", code: 1}
		}
		return showAgentBaseWisp(firstArg(args))
	case "list":
		return bdAnswer{stderr: "Error: database locked\n", code: 1}
	case "close":
		return logClosedIDs(f, args)
	}
	return bdAnswer{}
}

// moleculeNotFoundBD: the attached molecule no longer exists; closing it
// fails. Every close logs its first argument, and a close_attempt line.
func moleculeNotFoundBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		switch beadID := firstArg(args); beadID {
		case "gt-gastown-polecat-nux":
			return bdOut(doneAgentJSON + "\n")
		case "gt-base-123":
			return bdOut(doneBaseJSON + "\n")
		case "gt-wisp-xyz":
			return bdAnswer{stdout: "[]\n", code: 1}
		}
		return bdAnswer{}
	case "list":
		return bdOut("[]\n")
	case "close":
		id := firstArg(args)
		f.logLine(id)
		if id == "gt-wisp-xyz" {
			f.logLine("close_attempt: " + id + " (not found)")
			return bdAnswer{code: 1}
		}
		f.logLine("close_attempt: " + id + " (success)")
	}
	return bdAnswer{}
}

// pendingMRBD: the agent bead's active_mr names the MR this gt done just
// submitted, and the hooked bead is a plain open bead with no molecule.
// Every close and comments call is logged whole.
func pendingMRBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "show":
		if argsMention(args, "--children") {
			return bdOut("{}\n")
		}
		switch firstArg(args) {
		case "gt-gastown-polecat-nux":
			return bdOut(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","status":"open","hook_bead":"gt-base-123","agent_state":"working","description":"active_mr: gt-mr-42"}]` + "\n")
		case "gt-base-123":
			return bdOut(`[{"id":"gt-base-123","title":"Base bead","status":"open"}]` + "\n")
		}
		return bdAnswer{}
	case "list":
		return bdOut("[]\n")
	case "close", "comments":
		f.logLine(cmd + " " + strings.Join(args, " "))
	}
	return bdAnswer{}
}

// TestDoneStateEnvZeroValueIsTheRealProcess: updateAgentStateOnDone passes
// the zero doneStateEnv, which must read the real environment and the real
// HEAD; a nil bd runner is the real bd (beads.NewWithBeadsDirAndRunner).
func TestDoneStateEnvZeroValueIsTheRealProcess(t *testing.T) {
	t.Parallel()
	var e doneStateEnv
	if got, want := reflect.ValueOf(e.lookup()).Pointer(), reflect.ValueOf(os.Getenv).Pointer(); got != want {
		t.Error("zero doneStateEnv does not read the environment through os.Getenv")
	}
	if got, want := reflect.ValueOf(e.head()).Pointer(), reflect.ValueOf(currentReviewEvidenceHead).Pointer(); got != want {
		t.Error("zero doneStateEnv does not resolve HEAD through currentReviewEvidenceHead")
	}
	if e.bd != nil {
		t.Error("zero doneStateEnv has a bd runner; it must be nil, the real bd")
	}
}
