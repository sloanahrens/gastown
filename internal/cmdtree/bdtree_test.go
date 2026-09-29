package cmdtree

import (
	"strings"
	"testing"
)

func TestLoadBdTree(t *testing.T) {
	t.Parallel()
	tree, snap, err := LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	if snap.ContractVersion != 1 {
		t.Errorf("contract_version = %d; want 1 (refresh the snapshot and this test together)", snap.ContractVersion)
	}
	if snap.Commit == "" || snap.Source == "" {
		t.Errorf("snapshot provenance missing: source=%q commit=%q", snap.Source, snap.Commit)
	}
	for _, cmd := range []string{"sync", "list", "mol wisp", "mol wisp mol-gastown-boot", "close", "dep add", "dep gt-abc"} {
		if r := tree.Resolve(strings.Fields(cmd)); !r.OK {
			t.Errorf("bd %s does not resolve (unknown %q)", cmd, r.Unknown)
		}
	}
	for _, cmd := range []string{"daemons", "mol bogus", "mol list", "dolt bogus"} {
		if r := tree.Resolve(strings.Fields(cmd)); r.OK {
			t.Errorf("bd %s resolves; want unknown", cmd)
		}
	}
}
