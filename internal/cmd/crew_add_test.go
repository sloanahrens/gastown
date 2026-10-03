package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// failingUpsertStore fails every read and write, the store side of a bd that
// is unreachable while a crew worker is being added.
type failingUpsertStore struct{ *beadsfake.Fake }

func (f *failingUpsertStore) Show(string) (*beads.Issue, error) {
	return nil, errors.New("bd unreachable")
}

func (f *failingUpsertStore) Create(beads.CreateOptions) (*beads.Issue, error) {
	return nil, errors.New("bd unreachable")
}

func TestUpsertCrewAgentBead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"bom-","path":"bti_ops_match"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	t.Run("uses create or reopen with canonical crew metadata", func(t *testing.T) {
		fake := beadsfake.New()
		id, err := upsertCrewAgentBead(fake, townRoot, "bti_ops_match", "neo")
		if err != nil {
			t.Fatalf("upsertCrewAgentBead: %v", err)
		}

		if id != "bom-bti_ops_match-crew-neo" {
			t.Fatalf("returned id = %q, want %q", id, "bom-bti_ops_match-crew-neo")
		}
		issue, err := fake.Show(id)
		if err != nil {
			t.Fatalf("showing the upserted bead: %v", err)
		}
		wantTitle := "Crew worker neo in bti_ops_match - human-managed persistent workspace."
		if issue.Title != wantTitle {
			t.Fatalf("upsert title = %q, want %q", issue.Title, wantTitle)
		}
		if !beads.HasLabel(issue, "gt:agent") {
			t.Fatalf("upserted bead %s carries labels %v, want gt:agent", id, issue.Labels)
		}
		fields := beads.ParseAgentFields(issue.Description)
		if fields.RoleType != "crew" {
			t.Fatalf("RoleType = %q, want %q", fields.RoleType, "crew")
		}
		if fields.Rig != "bti_ops_match" {
			t.Fatalf("Rig = %q, want %q", fields.Rig, "bti_ops_match")
		}
		if fields.AgentState != "idle" {
			t.Fatalf("AgentState = %q, want %q", fields.AgentState, "idle")
		}
	})

	t.Run("propagates upsert errors", func(t *testing.T) {
		fake := &failingUpsertStore{Fake: beadsfake.New()}
		id, err := upsertCrewAgentBead(fake, townRoot, "bti_ops_match", "neo")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if id != "" {
			t.Fatalf("id = %q, want empty on error", id)
		}
	})
}
