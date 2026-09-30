package landworker

import "github.com/steveyegge/gastown/internal/land"

// GatePolicy wraps a rig's merged-tree gate before Land runs it. It is the
// seam the flake policy (gt-v4ssj.5) plugs into: a policy sees each
// GateResult, per-package results included, and may rerun a failed package
// before Land reads the result and writes a rejection. The worker itself
// never reruns a gate.
type GatePolicy func(rig string, g land.Gate) land.Gate

// NoRerun is the policy until gt-v4ssj.5 lands: the gate's first result is
// final.
func NoRerun(_ string, g land.Gate) land.Gate { return g }
