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
// (plan row B2a).
func TestOpenShellSpawnWritesTheWorkBead(t *testing.T) {
	t.Parallel()
	const bead = "gt-2lu00"
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	spec := openshellSpawn(town, work)
	spec.WorkBeadID = bead
	if _, err := b.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.WorkBead != bead {
		t.Fatalf("record WorkBead = %q, want the spawn's %q", rec.WorkBead, bead)
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
	if got := obj["work_bead"]; got != bead {
		t.Fatalf("record file work_bead = %v, want %q; the field's own key is the contract", got, bead)
	}
}

// A record written before the field existed must keep reading: the missing key
// is an empty WorkBead, never an error (plan row B2a).
func TestSandboxRecordWithoutWorkBeadReadsEmpty(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const session = "gt-gastown-old-seat"
	body := `{
  "session": "` + session + `",
  "rig": "gastown",
  "polecat": "old-seat",
  "profile": "polecat-default",
  "image": "gastown-polecat:dev",
  "created_at": "2026-11-01T00:00:00Z"
}`
	path := filepath.Join(SandboxRegistryDir(town), session+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
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
}

// A polecat slung no bead still spawns: the record carries an empty WorkBead
// and the write succeeds (plan row B2a).
func TestOpenShellSpawnWithoutAWorkBeadWritesEmpty(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	spec := openshellSpawn(town, work)
	if spec.WorkBeadID != "" {
		t.Fatalf("the fixture names work bead %q; this test is the no-bead spawn", spec.WorkBeadID)
	}
	if _, err := b.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare without a work bead: %v", err)
	}
	rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.WorkBead != "" {
		t.Fatalf("WorkBead = %q, want empty", rec.WorkBead)
	}
}
