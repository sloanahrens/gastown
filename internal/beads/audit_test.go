package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetachMoleculeWithAuditWritesTheAuditLogThroughBeads covers the one path
// that keeps detach audit logging alive now that logDetachAudit is unexported
// (gt-p85pw.6): DetachMoleculeWithAudit finds the sink by a type assertion on
// the Client, so a rename or a dropped assertion branch would leave every
// detach unlogged without failing anything else. This drives the whole path
// over a *Beads and reads the log back, so the branch is pinned behaviourally
// and the companion `var _ detachAuditLogger = (*Beads)(nil)` pins the name at
// compile time.
func TestDetachMoleculeWithAuditWritesTheAuditLogThroughBeads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating beads dir: %v", err)
	}

	const pinnedShow = `[{"id":"gt-pin","title":"pinned role bead","status":"pinned",` +
		`"description":"attached_molecule: gt-mol-1\nattached_at: 2026-10-25T20:00:00Z\n"}]`
	r := newRecorder(func(args []string) reply {
		if len(args) > 0 && args[0] == "show" {
			return reply{stdout: pinnedShow}
		}
		return reply{}
	})
	b := newRecordedBeads(dir, r)

	if _, err := DetachMoleculeWithAudit(b, "gt-pin", DetachOptions{
		Operation: "burn",
		Agent:     "marble",
		Reason:    "worker died",
	}); err != nil {
		t.Fatalf("DetachMoleculeWithAudit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(beadsDir, "audit.log"))
	if err != nil {
		t.Fatalf("reading audit.log (the *Beads sink did not run): %v", err)
	}
	var entry DetachAuditEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("audit.log is not one JSON entry: %v (raw %q)", err, data)
	}
	if entry.Operation != "burn" || entry.DetachedBy != "marble" || entry.Reason != "worker died" {
		t.Errorf("audit entry = %+v, want the burn by marble for the recorded reason", entry)
	}
	if entry.PinnedBeadID != "gt-pin" || entry.DetachedMolecule != "gt-mol-1" || entry.PreviousState != "pinned" {
		t.Errorf("audit entry = %+v, want the pin and the molecule it held", entry)
	}
	if entry.Timestamp == "" {
		t.Error("audit entry has no timestamp")
	}

	// The detach itself still ran: the update clears the attachment fields.
	var updated bool
	for _, argv := range r.argvs() {
		if strings.HasPrefix(argv, "update ") {
			updated = true
		}
	}
	if !updated {
		t.Errorf("no bd update ran; calls were %v", r.argvs())
	}
}
