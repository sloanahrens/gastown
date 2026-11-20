package cmd

import "testing"

// TestDoneAgentFromTheCommonWorkPath pins the agent gt done resolves for a
// sandbox seat that starts through the shared link /sandbox/gt/work: the seat
// reads the link path as its cwd while the sandbox record keeps the real
// worktree path, so detection must name the same agent from either (gt-u12jff,
// gt-f80lq2). The directory and the environment are arguments, so the test
// reads no town tree and no host environment.
func TestDoneAgentFromTheCommonWorkPath(t *testing.T) {
	t.Parallel()

	const (
		townRoot = "/sandbox/gt"
		worktree = "/sandbox/gt/gastown/polecats/slit/gastown"
		common   = "/sandbox/gt/work"
	)
	getenv := envMap(map[string]string{
		"GT_ROLE":    "gastown/polecats/slit",
		"GT_RIG":     "gastown",
		"GT_POLECAT": "slit",
	})

	real, realActor, realOK := detectDoneAgent(worktree, townRoot, getenv)
	linked, linkedActor, linkedOK := detectDoneAgent(common, townRoot, getenv)

	if !realOK || !linkedOK {
		t.Fatalf("detection ok = %v from %s, %v from %s; want true from both", realOK, worktree, linkedOK, common)
	}
	if real.WorkDir != worktree || linked.WorkDir != common {
		t.Fatalf("work dirs = %q, %q; want the directories each call was given", real.WorkDir, linked.WorkDir)
	}
	if real.Role != linked.Role || real.Rig != linked.Rig || real.Polecat != linked.Polecat || real.TownRoot != linked.TownRoot {
		t.Fatalf("agent from %s = %+v, from %s = %+v; want the same identity", worktree, real, common, linked)
	}
	if realActor != linkedActor || realActor != "gastown/polecats/slit" {
		t.Fatalf("actors = %q from %s, %q from %s; want gastown/polecats/slit from both", realActor, worktree, linkedActor, common)
	}
}
