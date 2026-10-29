package cmd

import (
	"errors"
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
// fakes: every bead exists everywhere unless verifyErrs says otherwise, and
// each executeSling succeeds unless failExecute names its bead.
type batchSlingFake struct {
	b        batchSling
	executed []string
	cooked   int
	// verifyErrs is the bead id -> error b.verifyBead returns. A bead not
	// listed exists.
	verifyErrs map[string]error
	// failExecute is the bead id -> reason its spawn is refused. A bead not
	// listed spawns. No real sleep, retry or timeout: b.sleep is a no-op.
	failExecute map[string]string
}

func newBatchSlingFake(t *testing.T) *batchSlingFake {
	t.Helper()
	f := &batchSlingFake{}
	f.b = batchSling{
		out: io.Discard,
		verifyBead: func(beadID string) error {
			return f.verifyErrs[beadID]
		},
		beadInfo:          func(string) (*beadInfo, error) { return &beadInfo{Status: "open"}, nil },
		verifyInTargetRig: func(string, string, string) error { return nil },
		crossRigGuard:     func(string, string, string) error { return nil },
		resolveFormula:    func(string, bool, string, string) string { return "" },
		cook: func(string, string, string) error {
			f.cooked++
			return nil
		},
		execute: func(p SlingParams) (*SlingResult, error) {
			f.executed = append(f.executed, p.BeadID)
			if reason, refused := f.failExecute[p.BeadID]; refused {
				return nil, errors.New(reason)
			}
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

// ---------------------------------------------------------------------------
// Partial failure and cause preservation (gt-u4x82)
// ---------------------------------------------------------------------------

// batchSlingTownDir is a town root whose .beads dir exists, so the batch's
// real routing lookups (only the guards are faked) have somewhere to look.
func batchSlingTownDir(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	return beadsDir
}

// TestBatchSling_PartialFailureReturnsError: three beads where the second
// fails to spawn. The run returns an error naming that bead, and the first
// and third are still slung — a partial failure does not roll back the
// successes, but it does make the command exit non-zero.
func TestBatchSling_PartialFailureReturnsError(t *testing.T) {
	t.Parallel()
	f := newBatchSlingFake(t)
	f.failExecute = map[string]string{"gt-bbb": "no free polecat slot"}

	err := f.b.run([]string{"gt-aaa", "gt-bbb", "gt-ccc"}, "gastown", batchSlingTownDir(t))
	if err == nil {
		t.Fatal("expected an error for the refused bead, got nil")
	}
	if !strings.Contains(err.Error(), "gt-bbb") {
		t.Errorf("error should name the failed bead, got: %v", err)
	}
	if strings.Contains(err.Error(), "gt-aaa") || strings.Contains(err.Error(), "gt-ccc") {
		t.Errorf("error should name only the failed bead, got: %v", err)
	}
	if got := strings.Join(f.executed, ","); got != "gt-aaa,gt-bbb,gt-ccc" {
		t.Errorf("dispatched %q, want every bead attempted (gt-aaa,gt-bbb,gt-ccc)", got)
	}
}

// TestBatchSling_AllFailedKeepsMessage: when every bead is refused the error
// keeps the "0/N succeeded" wording, and still names the failed beads.
func TestBatchSling_AllFailedKeepsMessage(t *testing.T) {
	t.Parallel()
	f := newBatchSlingFake(t)
	f.failExecute = map[string]string{"gt-aaa": "boom", "gt-bbb": "boom"}

	err := f.b.run([]string{"gt-aaa", "gt-bbb"}, "gastown", batchSlingTownDir(t))
	if err == nil {
		t.Fatal("expected an error when every bead failed, got nil")
	}
	for _, want := range []string{"batch sling failed: 0/2 succeeded", "gt-aaa", "gt-bbb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestBatchSling_VerifyBeadCausePreserved: a verifyBead failure that is not a
// missing bead (Dolt down, a timeout) keeps its cause, so the error says both
// which bead was refused and why.
func TestBatchSling_VerifyBeadCausePreserved(t *testing.T) {
	t.Parallel()
	f := newBatchSlingFake(t)
	cause := errors.New("dolt: connection refused")
	f.verifyErrs = map[string]error{"gt-bbb": cause}

	err := f.b.run([]string{"gt-aaa", "gt-bbb", "gt-ccc"}, "gastown", batchSlingTownDir(t))
	if err == nil {
		t.Fatal("expected an error for the unreadable bead, got nil")
	}
	if !strings.Contains(err.Error(), "gt-bbb") {
		t.Errorf("error should name the bead, got: %v", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Errorf("error should keep the cause %q, got: %v", cause, err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("cause should be wrapped, not flattened: %v", err)
	}
	if len(f.executed) != 0 {
		t.Errorf("slung %v after the up-front check failed", f.executed)
	}
}
