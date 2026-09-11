package refinery

import "testing"

// TestCombineGateSetSHA guards the om-gate T8 review's second major finding:
// GateSetSHA only hashed the polecat's *_command set, so an operator editing
// merge_queue.gates (the named-gate map doMerge actually runs when non-empty)
// left PreVerifiedGates matching the stale hash and the fast-path kept
// skipping gates the refinery would now run differently.
func TestCombineGateSetSHA(t *testing.T) {
	const base = "polecat-command-set-sha"

	nilGates := combineGateSetSHA(base, nil)
	emptyGates := combineGateSetSHA(base, map[string]*GateConfig{})
	if nilGates != emptyGates {
		t.Errorf("nil and empty gates maps should hash the same: nil=%s empty=%s", nilGates, emptyGates)
	}

	withLint := combineGateSetSHA(base, map[string]*GateConfig{
		"lint": {Cmd: "golangci-lint run"},
	})
	if withLint == nilGates {
		t.Error("adding a named gate must change the combined hash")
	}

	editedLint := combineGateSetSHA(base, map[string]*GateConfig{
		"lint": {Cmd: "golangci-lint run --fix"},
	})
	if editedLint == withLint {
		t.Error("editing a named gate's command must change the combined hash")
	}

	withLintAndTest := combineGateSetSHA(base, map[string]*GateConfig{
		"lint": {Cmd: "golangci-lint run"},
		"test": {Cmd: "go test ./..."},
	})
	if withLintAndTest == withLint {
		t.Error("adding a second named gate must change the combined hash")
	}

	// Map iteration order is randomized by Go; the hash must not depend on it.
	reordered := combineGateSetSHA(base, map[string]*GateConfig{
		"test": {Cmd: "go test ./..."},
		"lint": {Cmd: "golangci-lint run"},
	})
	if reordered != withLintAndTest {
		t.Error("combined hash must be independent of map iteration order")
	}

	if combineGateSetSHA("sha-a", nil) == combineGateSetSHA("sha-b", nil) {
		t.Error("a change to the polecat-side hash must still change the combined hash")
	}
}

// TestCurrentGateSetSHAFn_IncludesRefineryGates exercises the production
// closure NewEngineer installs (not a test fake): editing the Engineer's own
// loaded merge_queue.gates map must change what resolveFastPath compares
// PreVerifiedGates against.
func TestCurrentGateSetSHAFn_IncludesRefineryGates(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	baseline := e.currentGateSetSHAFn()

	e.config.Gates = map[string]*GateConfig{
		"lint": {Cmd: "golangci-lint run"},
	}
	withGate := e.currentGateSetSHAFn()
	if withGate == baseline {
		t.Fatal("currentGateSetSHAFn() unchanged after configuring a merge_queue.gates entry — the refinery's own gate map is invisible to the fast-path staleness check")
	}

	e.config.Gates["lint"].Cmd = "golangci-lint run --fix"
	edited := e.currentGateSetSHAFn()
	if edited == withGate {
		t.Fatal("currentGateSetSHAFn() unchanged after editing a configured gate's command")
	}
}
