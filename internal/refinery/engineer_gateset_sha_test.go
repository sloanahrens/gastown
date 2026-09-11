package refinery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

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

// TestGateSetSHA_ProducerConsumerAgree guards the om-gate T8 review's
// attempt-2 critical finding: gt done stamped pre_verified_gates with
// config.GateSetSHA(mq) alone, while the refinery's production
// currentGateSetSHAFn combined mqSHA with the refinery's own gates map via a
// second, differently-shaped hash function — the two values could never be
// equal, so resolveFastPath always reported "gate set changed" and the
// fast-path never fired.
//
// This test builds the SAME rig-root config.json both sides read, computes
// the stamp the way gt done does (rig.ResolveMergeQueueConfig +
// rig.LoadNamedGateCommands + config.CombineGateSetSHA), builds a production
// Engineer against that same config.json, and asserts its
// currentGateSetSHAFn() agrees exactly — a compile-time guarantee that both
// sides call the identical config.CombineGateSetSHA algorithm.
func TestGateSetSHA_ProducerConsumerAgree(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	configJSON := `{
		"merge_queue": {
			"test_command": "go test ./...",
			"lint_command": "golangci-lint run",
			"gates": {
				"extra": {"cmd": "make extra-check"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Producer side: what gt done stamps as pre_verified_gates.
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
	producerSHA := config.CombineGateSetSHA(mq, rig.LoadNamedGateCommands(townRoot, rigName))

	// Consumer side: the refinery's production closure, unmodified.
	r := &rig.Rig{Name: rigName, Path: rigPath}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	consumerSHA := e.currentGateSetSHAFn()

	if producerSHA != consumerSHA {
		t.Fatalf("producer stamp %q != consumer check %q — the fast-path can never fire", producerSHA, consumerSHA)
	}
}
