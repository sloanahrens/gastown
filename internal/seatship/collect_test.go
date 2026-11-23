package seatship

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCollectFile(t *testing.T, path, data string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(data), 0o644))
}

// collectFixture plants a seat's projects and outbox files and returns the
// input that reads them; projects "absent" or "link" breaks one part's read.
func collectFixture(t *testing.T, submitted bool, projects string, outbox map[string]string, cap int64) CollectInput {
	t.Helper()
	dir := t.TempDir()
	if projects != "absent" {
		writeCollectFile(t, filepath.Join(dir, "projects", "p", "a.jsonl"), "one\ntwo\n")
	}
	if projects == "link" {
		writeCollectFile(t, filepath.Join(dir, "projects", "out.txt"), "secret\n")
		must(t, os.Symlink(filepath.Join(dir, "projects", "out.txt"), filepath.Join(dir, "projects", "x.jsonl")))
	}
	for name, data := range outbox {
		writeCollectFile(t, filepath.Join(dir, "outbox", name), data)
	}
	return CollectInput{Rig: "gastown", Polecat: "rictus", Bead: "gt-t1", Submitted: submitted,
		ProjectsDir: filepath.Join(dir, "projects"), OutboxDir: filepath.Join(dir, "outbox"), OutboxCap: cap, TailBytes: 4}
}

func TestSeatShipCollect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		submitted  bool
		projects   string // "" is a readable seat; "absent"/"link" breaks one part
		outbox     map[string]string
		cap        int64
		wantTail   string
		wantOutbox map[string]string
		wantErr    string // the name Collect must wrap the failing part's error with
	}{
		{"submitted seat has no tail", true, "", nil, 1 << 10, "", map[string]string{}, ""},
		{"unsubmitted seat has its tail", false, "", nil, 1 << 10, "two\n", map[string]string{}, ""},
		{"submitted seat never reads a tail it would refuse", true, "link", nil, 1 << 10, "", map[string]string{}, ""},
		{"usage error named", false, "absent", nil, 1 << 10, "", nil, "seatship: collecting usage:"},
		{"outbox error named", false, "", map[string]string{"result.json": "{}"}, 0, "", nil, "seatship: collecting outbox:"},
		{"tail error named", false, "link", nil, 1 << 10, "", nil, "seatship: collecting transcript tail:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := collectFixture(t, tt.submitted, tt.projects, tt.outbox, tt.cap)
			p, err := Collect(in)
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Fatalf("Collect err = %v, want an error wrapped with %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if p.Rig != in.Rig || p.Polecat != in.Polecat || p.Bead != in.Bead || p.Submitted != in.Submitted {
				t.Errorf("payload = %+v, want the input's rig, polecat, bead and submitted", p)
			}
			if p.Usage == nil || p.Usage.Bead != in.Bead || p.Usage.Class != "polecat" || p.Usage.Kind != "work" || p.Usage.Rig != in.Rig {
				t.Errorf("Usage = %+v, want the seat's polecat work record for %s", p.Usage, in.Bead)
			}
			if !maps.Equal(p.Outbox, tt.wantOutbox) || p.TranscriptTail != tt.wantTail {
				t.Errorf("Outbox = %v, tail = %q; want %v and %q", p.Outbox, p.TranscriptTail, tt.wantOutbox, tt.wantTail)
			}
		})
	}
}

func TestSeatShipCollectRoundTrip(t *testing.T) {
	t.Parallel()
	p, err := Collect(collectFixture(t, false, "", nil, 1<<10))
	must(t, err)
	comment, err := Encode(p)
	must(t, err)
	got, found, err := Decode(comment)
	if err != nil || !found || got.Bead != p.Bead || got.TranscriptTail != "two\n" || got.Usage == nil {
		t.Fatalf("Decode = found %v, payload %+v, err %v; want the seat's payload", found, got, err)
	}
	want, err := json.Marshal(p)
	must(t, err)
	have, err := json.Marshal(got)
	must(t, err)
	if !bytes.Equal(have, want) {
		t.Errorf("decoded payload = %s, want %s", have, want)
	}
}
