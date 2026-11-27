//go:build integration

package makegate

import (
	"strings"
	"testing"
)

// checkGateGOFLAGS fails unless every GOFLAGS line a go build, vet or test
// call or the lint step logged in run equals want, and each stage ran.
func checkGateGOFLAGS(t *testing.T, name string, run GateRun, want string) {
	t.Helper()
	seen := map[string]int{}
	for _, call := range run.Calls {
		i := strings.Index(call, "GOFLAGS=")
		stage := ""
		for _, s := range []string{"build", "vet", "test"} {
			if strings.HasPrefix(call, "go "+s+" ") {
				stage = s
			}
		}
		if strings.HasPrefix(call, "golangci-lint ") {
			stage = "lint"
		}
		if i < 0 || stage == "" {
			continue
		}
		seen[stage]++
		if got := call[i+len("GOFLAGS="):]; got != want {
			t.Errorf("%s: a %s call carried GOFLAGS=%q, want %q", name, stage, got, want)
		}
	}
	if len(seen) < 4 {
		t.Fatalf("%s logged GOFLAGS for %d stages; want build, vet, test and lint\ncalls:\n%s", name, len(seen), run.callLog())
	}
}

// TestIntegrationGateGOFLAGS checks that the gate compiles without debug info,
// keeping the caller's GOFLAGS in front, while presubmit passes it alone.
// Mutation: deleting the export drops the flag; adding it to presubmit grows it.
func TestIntegrationGateGOFLAGS(t *testing.T) {
	gate := newGateSandbox(t).Run(nil, "GOFLAGS=-mod=mod")
	g := newGateSandbox(t)
	g.repo = g.Checkout("gastown")
	first := g.Commit(g.repo, "first", gateScratchFiles(t))
	g.Commit(g.repo, "second", map[string]string{"tracker/changed.go": "package tracker\n"})
	presubmit := g.runTarget("presubmit", nil, "GOFLAGS=-mod=mod", "PRESUBMIT_BASE="+first)
	if gate.Code != 0 || presubmit.Code != 0 {
		t.Fatalf("the gate exited %d and presubmit %d on green stubs, want 0\ncalls:\n%s\n%s", gate.Code, presubmit.Code, gate.callLog(), presubmit.callLog())
	}
	checkGateGOFLAGS(t, "gate", gate, "-mod=mod -gcflags=all=-dwarf=false")
	checkGateGOFLAGS(t, "presubmit", presubmit, "-mod=mod")
}
