package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestRigAuditRegistered: gt rig offers audit, it runs, and it takes no
// argument. Without the AddCommand line in rig.go's init, Find fails.
func TestRigAuditRegistered(t *testing.T) {
	t.Parallel()
	found, rest, err := rigCmd.Find([]string{"audit"})
	if err != nil || len(rest) != 0 || found.Name() != "audit" || found.Parent() != rigCmd || !found.Runnable() {
		t.Fatalf("gt rig audit: found %q under %q with %q, runnable %v, err %v", found.Name(), found.Parent().Name(), rest, found.Runnable(), err)
	}
	if err := found.Args(found, []string{"extra"}); err == nil {
		t.Errorf("gt rig audit accepts an argument; want it refused")
	}
}

// TestRigAuditCommandRefusesNoTown: a run with no town is an error that
// prints no audit.
func TestRigAuditCommandRefusesNoTown(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	var out bytes.Buffer
	err := rigAuditCommand(&out, func() (string, error) { return "", refused })
	if err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") || out.Len() != 0 {
		t.Errorf("rigAuditCommand = %v, out %q; want a workspace error and no output", err, out.String())
	}
}

// TestRigAuditCommandExitsWithTheAuditCode: a town whose config cannot load
// exits with the audit's code, so a broken town never reads as clean.
func TestRigAuditCommandExitsWithTheAuditCode(t *testing.T) {
	t.Parallel()
	root := testutil.PrivateTempDir(t)
	var out bytes.Buffer
	err := rigAuditCommand(&out, func() (string, error) { return root, nil })
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("rigAuditCommand = %v; want a silent exit 1", err)
	}
	if !strings.HasPrefix(out.String(), "RIG AUDIT: FAILED: ") {
		t.Errorf("output %q does not name the failed audit", out.String())
	}
}
