package attention

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// AppendRefusal then ReadRefusals is the ledger round trip: the fields survive
// and the file is created 0600, since a refusal names a polecat and its branch.
func TestRefusalLedgerRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	want := Refusal{
		TS: at, Bead: "gt-x", Rig: "gastown", Worker: "emerald",
		Branch: "polecat/emerald/gt-x", Head: "abcdef1234567890",
		Kind: KindRevertGuard, Summary: "branch reverts 2 merged commit(s): abcd1234 do the thing",
	}
	if err := AppendRefusal(town, want); err != nil {
		t.Fatalf("AppendRefusal: %v", err)
	}
	got, err := ReadRefusals(town)
	if err != nil {
		t.Fatalf("ReadRefusals: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("refusals = %+v, want %+v", got, want)
	}
	fi, err := os.Stat(RefusalsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}

// A line the reader cannot parse is skipped: a process killed mid-write leaves
// one, and losing that line beats failing the read that lists the rest.
func TestReadRefusalsSkipsUnparseableLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := AppendRefusal(town, Refusal{Bead: "gt-1", Kind: KindRevertGuard}); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte("not json\n"), mustJSONLine(t, Refusal{Bead: "gt-2", Kind: KindRevertGuard})...)
	if err := os.WriteFile(RefusalsPath(town), bad, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadRefusals(town)
	if err != nil {
		t.Fatalf("ReadRefusals: %v", err)
	}
	if len(got) != 1 || got[0].Bead != "gt-2" {
		t.Errorf("refusals = %+v, want only the parseable line", got)
	}
}

// ReadRefusals on a town that has never recorded one is empty, not an error.
func TestReadRefusalsMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := ReadRefusals(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("ReadRefusals = %+v, %v; want empty and no error", got, err)
	}
}

func mustJSONLine(t *testing.T, r Refusal) []byte {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// ReadRefusalFile reads the rotated ledger that ReadRefusals leaves out, and a
// file that is not there holds none.
func TestReadRefusalFileReadsTheRotatedLedger(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(Dir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	data := append([]byte("not json\n"), mustJSONLine(t, Refusal{Bead: "gt-old", Kind: KindRevertGuard})...)
	if err := os.WriteFile(RotatedRefusalsPath(town), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadRefusalFile(RotatedRefusalsPath(town))
	if err != nil || len(got) != 1 || got[0].Bead != "gt-old" {
		t.Fatalf("ReadRefusalFile(rotated) = %+v, %v; want only gt-old", got, err)
	}
	if cur, err := ReadRefusals(town); err != nil || len(cur) != 0 {
		t.Errorf("ReadRefusals = %+v, %v; want the rotated file left out", cur, err)
	}
	if none, err := ReadRefusalFile(RefusalsPath(town)); err != nil || len(none) != 0 {
		t.Errorf("ReadRefusalFile(missing) = %+v, %v; want empty and no error", none, err)
	}
}
