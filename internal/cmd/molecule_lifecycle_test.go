package cmd

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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

// testMoleculeEnv is gt mol burn/squash in townRoot, whose local beads
// workspace is the town, with db as its store and flags at their defaults.
func testMoleculeEnv(townRoot string, db *doneRecorder) moleculeLifecycleEnv {
	return moleculeLifecycleEnv{
		getwd:        func() (string, error) { return townRoot, nil },
		findTown:     func() (string, error) { return townRoot, nil },
		getenv:       envMap(nil),
		beadsWorkDir: func() (string, error) { return townRoot, nil },
		store:        func(string) moleculeStore { return moleculeFake{db} },
		out:          io.Discard,
		errOut:       io.Discard,
		noDigest:     true, // the digest path is not what these tests pin
	}
}

// moleculeFake is a recorded store with the audited detach.
type moleculeFake struct{ *doneRecorder }

func (f moleculeFake) DetachMoleculeWithAudit(id string, _ beads.DetachOptions) (*beads.Issue, error) {
	return fakeDetach(f, id)
}

// handoffMoleculeDB holds one pinned handoff bead titled handoffTitle with
// molecule attached, the molecule's root, and steps as its children; every
// close is recorded.
func handoffMoleculeDB(t *testing.T, handoffTitle, molecule string, steps ...beads.Issue) *doneRecorder {
	t.Helper()
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-handoff-1", Title: handoffTitle, Status: string(beads.StatusPinned), Description: "attached_molecule: " + molecule},
		beads.Issue{ID: molecule, Title: "mol", Status: string(beads.StatusHooked), Ephemeral: true},
	)
	seedChildren(t, db, molecule, steps...)
	return &doneRecorder{Client: db, mu: &sync.Mutex{}, closes: &[]string{}}
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
		e := testMoleculeEnv(t.TempDir(), handoffMoleculeDB(t, "x", "y"))
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
	e := testMoleculeEnv(t.TempDir(), handoffMoleculeDB(t, "x", "y"))
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
	e := testMoleculeEnv(townRoot, handoffMoleculeDB(t, "refinery Handoff", "gt-wisp-patrol1"))
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
	db := handoffMoleculeDB(t, "witness Handoff", "gt-wisp-mol1")
	if err := moleculeBurn(testMoleculeEnv(townRoot, db), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if got := db.closed(); !reflect.DeepEqual(got, []string{"gt-wisp-mol1"}) {
		t.Errorf("closes = %v, want the molecule root gt-wisp-mol1", got)
	}
	if h, err := db.Show("gt-handoff-1"); err != nil || beads.ParseAttachmentFields(h) != nil {
		t.Errorf("handoff = %+v, %v; want the molecule detached", h, err)
	}
}

// TestSquashClosesWispRoot: squash closes the molecule root after detaching,
// as burn does (#1828).
func TestSquashClosesWispRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	db := handoffMoleculeDB(t, "refinery Handoff", "gt-wisp-patrol1")
	if err := moleculeSquash(squashCmd(context.Background()), testMoleculeEnv(townRoot, db), []string{"refinery"}); err != nil {
		t.Fatalf("squash: %v", err)
	}
	if got := db.closed(); !reflect.DeepEqual(got, []string{"gt-wisp-patrol1"}) {
		t.Errorf("closes = %v, want the molecule root gt-wisp-patrol1", got)
	}
}

// TestSquashClosesDescendantsAndRoot: the open step is closed, the closed one
// is left alone, and the root is closed after its children.
func TestSquashClosesDescendantsAndRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	db := handoffMoleculeDB(t, "witness Handoff", "gt-wisp-mol2",
		beads.Issue{ID: "gt-step-1", Title: "Step 1", Status: "open"},
		beads.Issue{ID: "gt-step-2", Title: "Step 2", Status: "closed"})
	if err := moleculeBurn(testMoleculeEnv(townRoot, db), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if got, want := db.closed(), []string{"gt-step-1", "gt-wisp-mol2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v (the closed step left alone, the root last)", got, want)
	}
}
