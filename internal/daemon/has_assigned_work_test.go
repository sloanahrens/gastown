package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasAssignedOpenWork_UsesPinnedBeadsDirInsteadOfRigOrRepoFlag(t *testing.T) {
	t.Parallel()

	// GetRigDirForName's pathWithin resolves symlinks on the town root but the
	// rig dir never exists here, so a symlinked TempDir (macOS /var ->
	// /private/var) would make it return "" and skip the pinned BEADS_DIR path.
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte("{\"prefix\":\"gt-\",\"path\":\"gastown/mayor/rig\"}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	expectedBeadsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	// bd refuses the call unless it is routed by a pinned BEADS_DIR alone,
	// so a success is itself evidence of the routing.
	bd := newFakeCLIFor(func(c cliCall) cliReply {
		for _, a := range c.args {
			if strings.HasPrefix(a, "--repo=") {
				return cliReply{stderr: "unexpected --repo flag with pinned BEADS_DIR\n", code: 1}
			}
			if strings.HasPrefix(a, "--rig=") {
				return cliReply{stderr: "Error: unknown flag: --rig\n", code: 1}
			}
		}
		if got := c.getenv("BEADS_DIR"); got != expectedBeadsDir {
			return cliReply{stderr: "unexpected BEADS_DIR: " + got + "\n", code: 1}
		}
		return cliReply{stdout: `[{"id":"gt-123"}]` + "\n"}
	})

	d := &Daemon{
		config:  &Config{TownRoot: townRoot},
		bdPath:  "bd",
		execCmd: bd.run,
	}

	if !d.hasAssignedOpenWork("gastown", "polecats/rust") {
		t.Fatal("expected assigned work lookup to succeed")
	}

	calls := bd.recorded()
	if len(calls) == 0 {
		t.Fatal("hasAssignedOpenWork made no bd call")
	}
	for _, c := range calls {
		args := strings.Join(c.args, " ")
		if strings.Contains(args, "--rig=") {
			t.Fatalf("expected bd call to avoid --rig, got %q", args)
		}
		if strings.Contains(args, "--repo=") {
			t.Fatalf("expected bd call to avoid --repo with pinned BEADS_DIR, got %q", args)
		}
		if got := c.getenv("BEADS_DIR"); got != expectedBeadsDir {
			t.Fatalf("expected bd call to pin BEADS_DIR to %q, got %q (args %q)", expectedBeadsDir, got, args)
		}
	}
}
