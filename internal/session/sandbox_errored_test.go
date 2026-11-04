package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// TestNukeErroredSandboxRefusals: each way CleanupErroredSandbox declines to
// delete, and the one way it does (gt-jhoi1). A refusal leaves the record in
// place; only a terminal failure phase removes it.
func TestNukeErroredSandboxRefusals(t *testing.T) {
	t.Parallel()
	const sess = "gt-test-rig-Toast"
	tests := []struct {
		name        string
		badRecord   bool
		noRecord    bool
		phase       string
		statusErr   error
		deleteErr   error
		nilDeleter  bool
		wantErr     bool
		wantDeleted bool
		wantRemove  bool
		wantRecord  bool
	}{
		{name: "failed_phase_is_removed", phase: "Failed", wantDeleted: true, wantRemove: true},
		{name: "unreadable_record_is_an_error", badRecord: true, wantErr: true, wantRecord: true},
		{name: "no_record_is_nothing_to_do", noRecord: true},
		{name: "gateway_says_sandbox_not_found_keeps_the_record", statusErr: errors.New("sandbox not found: " + sess), wantRecord: true},
		{name: "gateway_that_cannot_answer_is_an_error", statusErr: errors.New("gateway timed out"), wantErr: true, wantRecord: true},
		{name: "failed_delete_is_an_error_and_keeps_the_record", phase: "Error", deleteErr: errors.New("gateway unreachable"), wantDeleted: true, wantErr: true, wantRecord: true},
		{name: "nil_deleter_refuses_and_keeps_the_record", phase: "Error", nilDeleter: true, wantErr: true, wantRecord: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			recordPath := filepath.Join(SandboxRegistryDir(town), sess+".json")
			if !tt.noRecord {
				if tt.badRecord {
					if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(recordPath, []byte("{not json"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else if err := WriteSandboxRecord(town, SandboxRecord{Session: sess, Rig: "test-rig", Polecat: "Toast", Gateway: "k8s"}); err != nil {
					t.Fatal(err)
				}
			}
			status := func(gateway, name string) (sandbox.Status, error) {
				if tt.statusErr != nil {
					return sandbox.Status{}, tt.statusErr
				}
				return sandbox.Status{Name: name, Phase: tt.phase}, nil
			}
			var deleted []string
			var deleter func(string, SandboxRecord) error
			if !tt.nilDeleter {
				deleter = func(_ string, rec SandboxRecord) error {
					deleted = append(deleted, rec.Session)
					return tt.deleteErr
				}
			}

			removed, err := CleanupErroredSandbox(status, deleter, town, sess)

			if (err != nil) != tt.wantErr {
				t.Fatalf("CleanupErroredSandbox err = %v, want error %v", err, tt.wantErr)
			}
			if removed != tt.wantRemove {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemove)
			}
			if tt.wantDeleted != (len(deleted) == 1) {
				t.Errorf("deleted %v, want the sandbox delete attempted %v", deleted, tt.wantDeleted)
			}
			_, statErr := os.Stat(recordPath)
			if present := statErr == nil; present != tt.wantRecord {
				t.Errorf("record present = %v, want %v", present, tt.wantRecord)
			}
		})
	}
}
