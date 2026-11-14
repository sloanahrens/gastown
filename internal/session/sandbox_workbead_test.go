package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The spawn's work bead reaches the record, under the work_bead key a sampler
// reads, so a seat's rows can be attributed without a bead read per tick
// (gt-nf49je).
func TestOpenShellSpawnWritesTheWorkBead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		workBead string // the bead the spawn is slung; "" is a polecat slung none
	}{
		{name: "a slung bead reaches the record's key", workBead: "gt-2lu00"},
		{name: "a polecat slung no bead still spawns with an empty field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
			b, _ := openshellTestBackend(town, openshellTestProfile, fake)
			spec := openshellSpawn(town, work)
			spec.WorkBeadID = tt.workBead
			if _, err := b.Prepare(context.Background(), spec); err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
			if err != nil || !ok {
				t.Fatalf("record: ok=%v err=%v", ok, err)
			}
			if rec.WorkBead != tt.workBead {
				t.Fatalf("record WorkBead = %q, want the spawn's %q", rec.WorkBead, tt.workBead)
			}

			// The key is the reader's contract: a sampler outside this package
			// attributes a seat's rows from the record's JSON, not from Go fields.
			raw, err := os.ReadFile(filepath.Join(SandboxRegistryDir(town), openshellTestSession+".json"))
			if err != nil {
				t.Fatalf("reading the record file: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("record file is not a JSON object: %v", err)
			}
			got, present := obj["work_bead"]
			if tt.workBead == "" {
				if present {
					t.Fatalf("record file work_bead = %v, want the key absent on a spawn with no bead", got)
				}
				return
			}
			if got != tt.workBead {
				t.Fatalf("record file work_bead = %v, want %q; the field's own key is the contract", got, tt.workBead)
			}
		})
	}
}

// A record written before the field existed must keep reading: a missing or
// empty key is an empty WorkBead, never an error (gt-nf49je).
func TestSandboxRecordWithoutWorkBeadReadsEmpty(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-old-seat"
	// body is a record as written before the field, plus the given trailing
	// field lines (none, or an explicit work_bead).
	body := func(extra string) string {
		return `{
  "session": "` + session + `",
  "rig": "gastown",
  "polecat": "old-seat",
  "profile": "polecat-default",
  "image": "gastown-polecat:dev",
  "created_at": "2026-11-01T00:00:00Z"` + extra + `
}`
	}
	tests := []struct {
		name string
		body string
	}{
		{name: "a record written before the field existed", body: body("")},
		{name: "a record with an explicitly empty work_bead", body: body(`,
  "work_bead": ""`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := filepath.Join(SandboxRegistryDir(town), session+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.body), 0o644); err != nil {
				t.Fatal(err)
			}

			rec, ok, err := ReadSandboxRecord(town, session)
			if err != nil {
				t.Fatalf("reading a pre-field record: %v", err)
			}
			if !ok {
				t.Fatal("record ok=false; a record written before the field is still a record")
			}
			if rec.Rig != "gastown" || rec.Polecat != "old-seat" {
				t.Fatalf("record = %+v; the other fields must still parse", rec)
			}
			if rec.WorkBead != "" {
				t.Fatalf("WorkBead = %q, want empty for a record that names none", rec.WorkBead)
			}
		})
	}
}
