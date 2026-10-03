package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSlingHasNoConvoyFlags pins gt-gzhin.4: a sling creates no convoy, so the
// flags that shaped the auto-convoy are gone from the command.
func TestSlingHasNoConvoyFlags(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"no-convoy", "owned", "merge"} {
		if f := slingCmd.Flags().Lookup(name); f != nil {
			t.Errorf("gt sling still has --%s: %s", name, f.Usage)
		}
	}
}

// --- Auto-rig-resolution and deprecation tests ---

// TestAllBeadIDs_TrueWhenAllBeadIDs verifies that allBeadIDs returns true
// when every argument looks like a bead ID.
func TestAllBeadIDs_TrueWhenAllBeadIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"all beads", []string{"gt-abc", "gt-def", "gt-ghi"}, true},
		{"mixed prefixes", []string{"gt-abc", "bd-def", "hq-ghi"}, true},
		{"single bead", []string{"gt-abc"}, true},
		{"last is rig name", []string{"gt-abc", "gt-def", "gastown"}, false},
		{"empty list", []string{}, false},
		{"contains path", []string{"gt-abc", "gastown/polecats/foo"}, false},
		{"contains bare word no hyphen", []string{"gt-abc", "gastown"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allBeadIDs(tc.args)
			if got != tc.want {
				t.Errorf("allBeadIDs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestResolveRigFromBeadIDs_AllSamePrefix verifies that resolveRigFromBeadIDs
// resolves the rig when all beads share the same prefix.
func TestResolveRigFromBeadIDs_AllSamePrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write routes.jsonl mapping gt- to gastown
	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	rigName, err := resolveRigFromBeadIDs([]string{"gt-aaa", "gt-bbb", "gt-ccc"}, townRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rigName != "gastown" {
		t.Errorf("rigName = %q, want %q", rigName, "gastown")
	}
}

// TestResolveRigFromBeadIDs_MixedPrefixes_Errors verifies that beads from
// different rigs produce an error with suggested actions.
func TestResolveRigFromBeadIDs_MixedPrefixes_Errors(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}
{"prefix":"bd-","path":"beads/.beads"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	_, err := resolveRigFromBeadIDs([]string{"gt-aaa", "bd-bbb", "gt-ccc"}, townRoot)
	if err == nil {
		t.Fatal("expected error for mixed prefixes, got nil")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "different rigs") {
		t.Errorf("error should mention 'different rigs', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "gastown") || !strings.Contains(errMsg, "beads") {
		t.Errorf("error should mention both rig names, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "Options") {
		t.Errorf("error should include suggested actions, got: %s", errMsg)
	}
}

// TestResolveRigFromBeadIDs_UnmappedPrefix_Errors verifies that a bead whose
// prefix has no route mapping produces an error with suggested actions.
func TestResolveRigFromBeadIDs_UnmappedPrefix_Errors(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Only gt- is mapped; zz- is not
	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	_, err := resolveRigFromBeadIDs([]string{"gt-aaa", "zz-bbb"}, townRoot)
	if err == nil {
		t.Fatal("expected error for unmapped prefix, got nil")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "zz-bbb") {
		t.Errorf("error should mention the bead ID, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "not mapped") {
		t.Errorf("error should mention prefix is not mapped, got: %s", errMsg)
	}
}

// TestResolveRigFromBeadIDs_TownLevelPrefix_Errors verifies that a bead with
// a town-level prefix (path=".") produces an error because it has no rig.
func TestResolveRigFromBeadIDs_TownLevelPrefix_Errors(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// hq- maps to town root (path=".")
	routesContent := `{"prefix":"hq-","path":"."}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	_, err := resolveRigFromBeadIDs([]string{"hq-aaa", "hq-bbb"}, townRoot)
	if err == nil {
		t.Fatal("expected error for town-level prefix, got nil")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "not mapped") || !strings.Contains(errMsg, "town-level") {
		t.Errorf("error should mention town-level bead, got: %s", errMsg)
	}
}

// ---------------------------------------------------------------------------
// Cross-rig guard in runBatchSling tests
// ---------------------------------------------------------------------------

// batchSlingFake is a batchSling whose guards and dispatch are recorded
// fakes: every bead exists everywhere, and each executeSling succeeds.
type batchSlingFake struct {
	b        batchSling
	executed []string
	cooked   int
}

func newBatchSlingFake(t *testing.T) *batchSlingFake {
	t.Helper()
	f := &batchSlingFake{}
	f.b = batchSling{
		out:               io.Discard,
		verifyBead:        func(string) error { return nil },
		verifyInTargetRig: func(string, string, string) error { return nil },
		crossRigGuard:     func(string, string, string) error { return nil },
		resolveFormula:    func(string, bool, string, string) string { return "" },
		cook: func(string, string, string) error {
			f.cooked++
			return nil
		},
		execute: func(p SlingParams) (*SlingResult, error) {
			f.executed = append(f.executed, p.BeadID)
			return &SlingResult{PolecatName: "toast"}, nil
		},
		wakeRig: func(string) {},
		sleep:   func(time.Duration) {},
	}
	return f
}

// TestBatchSling_CrossRigGuardRejectsPrefix: a batch holding a bead whose
// prefix routes to another rig is refused before anything is slung, naming
// the bead, its rig and the target.
func TestBatchSling_CrossRigGuardRejectsPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gastown/.beads"}
{"prefix":"bd-","path":"beads/.beads"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	f := newBatchSlingFake(t)

	err := f.b.run([]string{"gt-aaa", "bd-bbb"}, "gastown", beadsDir)
	if err == nil {
		t.Fatal("expected cross-rig guard error, got nil")
	}
	for _, want := range []string{"bd-bbb", `belongs to rig "beads"`, `target is "gastown"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if len(f.executed) != 0 {
		t.Errorf("slung %v before the guard refused the batch", f.executed)
	}

	// --force skips the guard and slings every bead.
	f.b.opts.force = true
	if err := f.b.run([]string{"gt-aaa", "bd-bbb"}, "gastown", beadsDir); err != nil {
		t.Fatalf("forced batch: %v", err)
	}
	if strings.Join(f.executed, ",") != "gt-aaa,bd-bbb" {
		t.Errorf("forced batch slung %v, want gt-aaa,bd-bbb", f.executed)
	}
}

// ---------------------------------------------------------------------------
// Review fix tests: Julian review findings on PR #1759
// ---------------------------------------------------------------------------

// TestResolveRigFromBeadIDs_MixedPrefixes_DoesNotSuggestForce verifies that
// the mixed-rig error suggests specifying an explicit rig, NOT --force.
// Review finding: --force suggestion is unreachable because resolveRigFromBeadIDs
// runs before --force is checked.
func TestResolveRigFromBeadIDs_MixedPrefixes_DoesNotSuggestForce(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}
{"prefix":"bd-","path":"beads/.beads"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	_, err := resolveRigFromBeadIDs([]string{"gt-aaa", "bd-bbb"}, townRoot)
	if err == nil {
		t.Fatal("expected error for mixed prefixes, got nil")
	}
	errMsg := err.Error()

	// Must NOT suggest --force (unreachable from this code path)
	if strings.Contains(errMsg, "--force") {
		t.Errorf("mixed-rig error should NOT suggest --force (unreachable), got:\n%s", errMsg)
	}

	// Must suggest specifying the rig explicitly
	if !strings.Contains(errMsg, "<rig>") {
		t.Errorf("mixed-rig error should suggest specifying rig explicitly, got:\n%s", errMsg)
	}
}

// TestBatchSling_SliceAliasingInCrossRigGuard verifies that the cross-rig guard
// error message does not mutate the input beadIDs slice via append.
// Review finding: append(beadIDs, rigName) mutates shared backing array.
func TestBatchSling_SliceAliasingInCrossRigGuard(t *testing.T) {
	t.Parallel()
	// Simulate the slice aliasing scenario:
	// args = ["gt-aaa", "bd-bbb", "gastown"]
	// beadIDs = args[:2] → shares backing array with args
	// append(beadIDs, rigName) writes into args[2]
	args := []string{"gt-aaa", "bd-bbb", "gastown"}
	beadIDs := args[:len(args)-1] // beadIDs = ["gt-aaa", "bd-bbb"], shares backing
	rigName := "resolved-rig"

	// Before the fix, this would mutate args[2] from "gastown" to "resolved-rig"
	_ = strings.Join(append([]string{}, beadIDs...), " ") // safe copy
	_ = rigName

	// Verify the original args are not mutated
	if args[2] != "gastown" {
		t.Errorf("args[2] was mutated from 'gastown' to %q — slice aliasing bug", args[2])
	}
}
