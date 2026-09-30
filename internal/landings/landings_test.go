package landings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// d2Line is a landing as crew/sloan/d2-land's land.LandingsFile.Append writes
// it (json.Marshal of land.LandingRecord).
const d2Line = `{"bead":"gt-abc","rig":"gastown","branch":"polecat/opal/gt-abc","head":"1111111111111111111111111111111111111111","target":"main","base":"2222222222222222222222222222222222222222","landed_commit":"3333333333333333333333333333333333333333","patch_id":"4444444444444444444444444444444444444444","gate_result":"pass","om_verdict":"approve","om_score":0.92,"route":"worker","landed_at":"2026-09-30T14:05:06Z"}`

func TestRecord_MatchesD2LandFields(t *testing.T) {
	var rec Record
	if err := json.Unmarshal([]byte(d2Line), &rec); err != nil {
		t.Fatal(err)
	}
	want := Record{
		Bead: "gt-abc", Rig: "gastown", Branch: "polecat/opal/gt-abc",
		Head: "1111111111111111111111111111111111111111", Target: "main",
		Base:         "2222222222222222222222222222222222222222",
		LandedCommit: "3333333333333333333333333333333333333333",
		PatchID:      "4444444444444444444444444444444444444444",
		GateResult:   "pass", OMVerdict: "approve", OMScore: 0.92, Route: "worker",
		LandedAt: time.Date(2026, 9, 30, 14, 5, 6, 0, time.UTC),
	}
	if !reflect.DeepEqual(rec, want) {
		t.Fatalf("got %+v\nwant %+v", rec, want)
	}
	// Every d2-land key maps to a field and back: no key is dropped.
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if keys(t, out) != keys(t, []byte(d2Line)) {
		t.Fatalf("key set drifted:\n got %s\nwant %s", keys(t, out), keys(t, []byte(d2Line)))
	}
}

func keys(t *testing.T, b []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	s := ""
	for _, k := range ks {
		s += k + " "
	}
	return s
}

func TestPath(t *testing.T) {
	p, err := Path("/town", "gastown")
	if err != nil || p != filepath.Join("/town", ".runtime", "landings", "gastown.jsonl") {
		t.Fatalf("Path = %q, %v", p, err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := Path("/town", bad); err == nil {
			t.Errorf("Path accepted rig %q", bad)
		}
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func beadLine(bead string) string {
	return `{"bead":"` + bead + `","rig":"gastown","landed_at":"2026-09-30T14:05:06Z"}` + "\n"
}

func beadsOf(recs []Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Bead)
	}
	return out
}

func TestReader_ReadsOnlyNewCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	r := &Reader{Path: path}

	recs, bad, err := r.ReadNew()
	if err != nil || len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("missing file: %v %v %v", recs, bad, err)
	}

	appendFile(t, path, beadLine("a")+beadLine("b"))
	recs, _, err = r.ReadNew()
	if err != nil || !reflect.DeepEqual(beadsOf(recs), []string{"a", "b"}) {
		t.Fatalf("first read: %v %v", beadsOf(recs), err)
	}

	// A line still being written is held until its newline arrives.
	full := beadLine("c")
	appendFile(t, path, full[:10])
	recs, bad, err = r.ReadNew()
	if err != nil || len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("partial line read early: %v %v %v", beadsOf(recs), bad, err)
	}
	appendFile(t, path, full[10:])
	recs, _, err = r.ReadNew()
	if err != nil || !reflect.DeepEqual(beadsOf(recs), []string{"c"}) {
		t.Fatalf("completed line: %v %v", beadsOf(recs), err)
	}

	recs, _, err = r.ReadNew()
	if err != nil || len(recs) != 0 {
		t.Fatalf("reread returned %v %v", beadsOf(recs), err)
	}
}

func TestReader_MalformedLineIsReportedAndSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	appendFile(t, path, beadLine("a")+"not json\n\n"+beadLine("b"))
	recs, bad, err := (&Reader{Path: path}).ReadNew()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beadsOf(recs), []string{"a", "b"}) || !reflect.DeepEqual(bad, []string{"not json"}) {
		t.Fatalf("recs %v bad %q", beadsOf(recs), bad)
	}
}

func TestReader_ReplacedFileRereadsFromStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	r := &Reader{Path: path}
	appendFile(t, path, beadLine("a")+beadLine("b"))
	if _, _, err := r.ReadNew(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(beadLine("z")), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, _, err := r.ReadNew()
	if err != nil || !reflect.DeepEqual(beadsOf(recs), []string{"z"}) {
		t.Fatalf("after replace: %v %v", beadsOf(recs), err)
	}
}
