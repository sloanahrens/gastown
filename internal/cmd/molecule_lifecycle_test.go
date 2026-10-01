package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestExtractRoleFromIdentity verifies that role names are correctly extracted
// from agent identity strings, including trailing slashes and compound paths.
func TestExtractRoleFromIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		want   string
	}{
		{"gastown/refinery", "refinery"},
		{"gastown/witness", "witness"},
		{"gastown/crew/jack", "jack"},
		{"gastown/polecats/nux", "nux"},
		{"mayor/", "mayor"},
		{"deacon/", "deacon"},
		{"deacon/boot", "boot"},
		{"refinery", "refinery"},
	}
	for _, tc := range tests {
		got := extractRoleFromIdentity(tc.target)
		if got != tc.want {
			t.Errorf("extractRoleFromIdentity(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// TestDoneClosesAttachedMolecule verifies that gt done closes both the hooked
// bead AND its attached molecule (wisp).
//
// Current bug: gt done only closes the hooked bead. If base bead is hooked
// with attached_molecule pointing to wisp, the wisp becomes orphaned.
//
// Expected behavior: gt done should:
// 1. Check for attached_molecule in hooked bead
// 2. Close the attached molecule (wisp) first
// 3. Close the hooked bead (base bead)
//
// This ensures no orphaned wisps remain after work completes.
func TestDoneClosesAttachedMolecule(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create rig structure - use simple rig name that matches routes lookup
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Create routes - path first part must match GT_RIG for prefix lookup
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	// The in-process bd holds:
	// - Agent bead gt-gastown-polecat-nux with hook_bead = gt-abc123 (base bead)
	// - Base bead gt-abc123 with attached_molecule: gt-wisp-xyz, status=hooked
	// - Wisp gt-wisp-xyz (the attached molecule)
	shows := map[string]string{
		"gt-gastown-polecat-nux": `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","status":"open","hook_bead":"gt-abc123","agent_state":"working"}]`,
		"gt-abc123":              `[{"id":"gt-abc123","title":"Bug to fix","status":"hooked","description":"attached_molecule: gt-wisp-xyz"}]`,
		"gt-wisp-xyz":            `[{"id":"gt-wisp-xyz","title":"mol-polecat-work","status":"open","ephemeral":true}]`,
	}
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		ids := nonFlagArgs(args)
		switch cmd {
		case "show":
			if len(ids) == 0 {
				return bdOut("[]")
			}
			if argsMention(args, "--children") {
				return bdOut(`{"` + ids[0] + `":[]}`)
			}
			if out, ok := shows[ids[0]]; ok {
				return bdOut(out)
			}
			return bdOut("[]")
		case "close":
			if len(ids) > 0 {
				f.logLine("close " + ids[0])
			}
		}
		return bdOut("")
	}}
	env := doneStateEnv{
		getenv:     envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		bd:         bd.run,
		reviewHead: func() (string, error) { return "", errors.New("no review evidence in this test") },
	}

	// Pass issueID directly — hq-l6mm5 removed agent bead hook slot lookup
	_ = updateAgentStateOnDoneIn(env, rigPath, townRoot, ExitCompleted, "gt-abc123")

	closed := closeLines(bd)
	foundWisp, foundBase := false, false
	for _, line := range closed {
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-abc123") {
			foundBase = true
		}
	}
	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\n"+
			"gt done should close the attached_molecule before closing the hooked bead.\n"+
			"This leaves orphaned wisps after work completes.\n"+
			"Beads closed: %v", closed)
	}
	if !foundBase {
		t.Errorf("hooked bead gt-abc123 was NOT closed\nBeads closed: %v", closed)
	}
}

// testMoleculeEnv is gt mol burn/squash in townRoot, whose local beads
// workspace is the town, with bd answered by bd and flags at their defaults.
func testMoleculeEnv(townRoot string, bd *inprocBD) moleculeLifecycleEnv {
	return moleculeLifecycleEnv{
		getwd:        func() (string, error) { return townRoot, nil },
		findTown:     func() (string, error) { return townRoot, nil },
		getenv:       envMap(nil),
		beadsWorkDir: func() (string, error) { return townRoot, nil },
		bd:           bd.run,
		out:          io.Discard,
		errOut:       io.Discard,
		noDigest:     true, // the digest path is not what these tests pin
	}
}

// handoffMoleculeBD is an in-process bd holding one pinned handoff bead with
// molecule attached; children maps a parent to its --children answer. Every
// close is logged as "close <args>".
func handoffMoleculeBD(handoffTitle, molecule string, children map[string]string) *inprocBD {
	handoff := `[{"id":"gt-handoff-1","title":"` + handoffTitle + `","status":"pinned","description":"attached_molecule: ` + molecule + `"}]`
	return &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		switch cmd {
		case "list":
			if argsMention(args, "status=pinned") {
				return bdOut(handoff)
			}
			for parent, kids := range children {
				if argsMention(args, "parent="+parent) {
					return bdOut(kids)
				}
			}
			return bdOut("[]")
		case "show":
			if argsMention(args, "--children") {
				if kids, ok := children[args[0]]; ok {
					return bdOut(`{"` + args[0] + `":` + kids + `}`)
				}
				return bdOut(`{"` + args[0] + `":[]}`)
			}
			return bdOut(handoff)
		case "create":
			return bdOut(`{"id":"gt-digest-1","title":"Digest"}`)
		case "close":
			f.logLine("close " + strings.Join(args, " "))
		}
		return bdOut("")
	}}
}

// closeLines is the bd close calls, in order.
func closeLines(bd *inprocBD) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(bd.log()), "\n") {
		if strings.HasPrefix(l, "close ") {
			out = append(out, l)
		}
	}
	return out
}

func squashCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	return cmd
}

// TestSquashJitterRejectsBadDurations: an invalid or negative --jitter fails
// before any workspace lookup, with a message naming the problem.
func TestSquashJitterRejectsBadDurations(t *testing.T) {
	t.Parallel()
	for jitter, want := range map[string]string{"bogus": "invalid --jitter duration", "-5s": "non-negative"} {
		e := testMoleculeEnv(t.TempDir(), handoffMoleculeBD("x", "y", nil))
		e.jitter = jitter
		e.findTown = func() (string, error) { t.Error("workspace looked up before the jitter was validated"); return "", nil }
		err := moleculeSquash(squashCmd(context.Background()), e, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--jitter %s: err = %v, want %q", jitter, err, want)
		}
	}
}

// TestSquashJitterZeroDuration: --jitter 0s is accepted, and the run goes on
// to the workspace lookup, which fails here.
func TestSquashJitterZeroDuration(t *testing.T) {
	t.Parallel()
	e := testMoleculeEnv(t.TempDir(), handoffMoleculeBD("x", "y", nil))
	e.jitter = "0s"
	e.findTown = func() (string, error) { return "", errors.New("not in a Gas Town workspace") }
	err := moleculeSquash(squashCmd(context.Background()), e, nil)
	if err == nil || strings.Contains(err.Error(), "jitter") {
		t.Fatalf("err = %v, want the workspace error, not a jitter error", err)
	}
}

// TestSquashJitterContextCancellation: the jitter sleep, once reached,
// returns as soon as the command's context is done instead of blocking.
func TestSquashJitterContextCancellation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	e := testMoleculeEnv(townRoot, handoffMoleculeBD("refinery Handoff", "gt-wisp-patrol1", nil))
	e.jitter = "10m" // the sleep would block if cancellation didn't work
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := moleculeSquash(squashCmd(ctx), e, []string{"refinery"})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("jitter sleep should have been cancelled, but took %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's cancellation", err)
	}
}

// TestBurnClosesWispRoot: burn detaches the molecule and then closes its
// root, or the wisp root stays hooked forever (#1828).
func TestBurnClosesWispRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("witness Handoff", "gt-wisp-mol1", nil)
	if err := moleculeBurn(testMoleculeEnv(townRoot, bd), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if closes := strings.Join(closeLines(bd), "\n"); !strings.Contains(closes, "gt-wisp-mol1") {
		t.Errorf("molecule root gt-wisp-mol1 was not closed; close calls:\n%s", closes)
	}
}

// TestSquashClosesWispRoot: squash closes the molecule root after detaching,
// as burn does (#1828).
func TestSquashClosesWispRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("refinery Handoff", "gt-wisp-patrol1", nil)
	if err := moleculeSquash(squashCmd(context.Background()), testMoleculeEnv(townRoot, bd), []string{"refinery"}); err != nil {
		t.Fatalf("squash: %v", err)
	}
	if closes := strings.Join(closeLines(bd), "\n"); !strings.Contains(closes, "gt-wisp-patrol1") {
		t.Errorf("molecule root gt-wisp-patrol1 was not closed; close calls:\n%s", closes)
	}
}

// TestSquashClosesDescendantsAndRoot: the open step is closed, the closed one
// is left alone, and the root is closed after its children.
func TestSquashClosesDescendantsAndRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("witness Handoff", "gt-wisp-mol2", map[string]string{
		"gt-wisp-mol2": `[{"id":"gt-step-1","title":"Step 1","status":"open"},{"id":"gt-step-2","title":"Step 2","status":"closed"}]`,
	})
	if err := moleculeBurn(testMoleculeEnv(townRoot, bd), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	lines := closeLines(bd)
	step1, root := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "gt-step-1") && step1 < 0 {
			step1 = i
		}
		if strings.Contains(l, "gt-wisp-mol2") {
			root = i
		}
		if strings.Contains(l, "gt-step-2") {
			t.Errorf("the already-closed gt-step-2 was closed again: %s", l)
		}
	}
	if step1 < 0 || root < 0 || root < step1 {
		t.Fatalf("close order: step-1 at %d, root at %d, want both with the root last; close calls:\n%s", step1, root, strings.Join(lines, "\n"))
	}
}
