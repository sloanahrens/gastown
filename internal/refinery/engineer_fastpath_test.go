package refinery

import (
	"strings"
	"testing"
)

// TestResolveFastPath guards the om-gate T8 honest fast-path: PreVerified
// alone is not enough. The target must be unchanged since verification, the
// rig's current gate-set hash must match what was recorded, and the rig must
// have no named gates the stamp cannot cover — otherwise gates run normally.
// skipGates never reaches the editorial precondition — that always runs
// regardless (T6) — this only governs the mechanical gates.
func TestResolveFastPath(t *testing.T) {
	t.Parallel()
	t.Run("not pre-verified", func(t *testing.T) {
		workDir, g, cleanup := testGitRepo(t)
		defer cleanup()
		e := newTestEngineer(t, workDir, g)
		e.currentGateSetSHAFn = func() string { return "sha-a" }

		mr := makeMR("gt-wisp-1", "polecat/x/gt-1", "main")
		mr.PreVerified = false

		if e.resolveFastPath(mr) {
			t.Error("resolveFastPath = true, want false when PreVerified is false")
		}
	})

	t.Run("pre-verified, target unchanged, gate set unchanged", func(t *testing.T) {
		workDir, g, cleanup := testGitRepo(t)
		defer cleanup()
		e := newTestEngineer(t, workDir, g)
		e.currentGateSetSHAFn = func() string { return "sha-a" }

		targetHead, err := g.Rev("origin/main")
		if err != nil {
			t.Fatalf("resolving origin/main: %v", err)
		}

		mr := makeMR("gt-wisp-1", "polecat/x/gt-1", "main")
		mr.PreVerified = true
		mr.PreVerifiedBase = targetHead
		mr.PreVerifiedGates = "sha-a"

		if !e.resolveFastPath(mr) {
			t.Error("resolveFastPath = false, want true when base and gate set both match")
		}
	})

	// gt-ypkc: a named gate is outside what gt done's pre-verification run
	// covers, so a stamp can never be trusted on a rig that configures one.
	// The post-squash gate here is the case no polecat-side run reproduces.
	t.Run("pre-verified, but the rig defines named gates", func(t *testing.T) {
		workDir, g, cleanup := testGitRepo(t)
		defer cleanup()
		e := newTestEngineer(t, workDir, g)
		e.currentGateSetSHAFn = func() string { return "sha-a" }
		e.config.Gates = map[string]*GateConfig{
			"boot-check": {Cmd: "make boot-check", Phase: GatePhasePostSquash},
		}

		targetHead, err := g.Rev("origin/main")
		if err != nil {
			t.Fatalf("resolving origin/main: %v", err)
		}

		mr := makeMR("gt-wisp-1", "polecat/x/gt-1", "main")
		mr.PreVerified = true
		mr.PreVerifiedBase = targetHead
		mr.PreVerifiedGates = "sha-a"

		if e.resolveFastPath(mr) {
			t.Error("resolveFastPath = true, want false when the rig has a named gate the stamp cannot cover")
		}
		out := e.output.(interface{ String() string }).String()
		if !strings.Contains(out, "cannot cover this rig's 1 named gate(s)") {
			t.Errorf("expected the named-gate refusal to be logged, got: %s", out)
		}
	})

	t.Run("pre-verified, target moved", func(t *testing.T) {
		workDir, g, cleanup := testGitRepo(t)
		defer cleanup()
		e := newTestEngineer(t, workDir, g)
		e.currentGateSetSHAFn = func() string { return "sha-a" }

		mr := makeMR("gt-wisp-1", "polecat/x/gt-1", "main")
		mr.PreVerified = true
		mr.PreVerifiedBase = "0000000000000000000000000000000000000000"
		mr.PreVerifiedGates = "sha-a"

		if e.resolveFastPath(mr) {
			t.Error("resolveFastPath = true, want false when target has moved since verification")
		}
	})

	t.Run("pre-verified, target unchanged, gate set changed", func(t *testing.T) {
		workDir, g, cleanup := testGitRepo(t)
		defer cleanup()
		e := newTestEngineer(t, workDir, g)
		e.currentGateSetSHAFn = func() string { return "sha-b" } // rig's gate config changed since verification

		targetHead, err := g.Rev("origin/main")
		if err != nil {
			t.Fatalf("resolving origin/main: %v", err)
		}

		mr := makeMR("gt-wisp-1", "polecat/x/gt-1", "main")
		mr.PreVerified = true
		mr.PreVerifiedBase = targetHead
		mr.PreVerifiedGates = "sha-a"

		if e.resolveFastPath(mr) {
			t.Error("resolveFastPath = true, want false when the gate set has changed since verification")
		}
		out := e.output.(interface{ String() string }).String()
		if !strings.Contains(out, "Pre-verification stale — gate set changed") {
			t.Errorf("expected staleness log line, got: %s", out)
		}
	})
}
