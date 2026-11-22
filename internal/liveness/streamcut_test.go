package liveness

import (
	"os"
	"path/filepath"
	"testing"
)

const cutRecord = `{"type":"assistant","isApiErrorMessage":true,"streamCutCause":"stale_connection"}`

func TestStreamCutTail(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{cutRecord + "\n", `{"type":"assis` + "\n" + cutRecord + "\n", cutRecord + "\n" + `{"type":"system"}` + "\n"} {
		if !StreamCutTail([]byte(tail)) {
			t.Errorf("%q: not a cut", tail)
		}
	}
}

func TestStreamCutTailFalseCases(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{
		cutRecord + "\n" + `{"type":"user"}` + "\n", cutRecord + "\n" + `{"type":"assistant"}` + "\n",
		`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}` + "\n", `{"type":"assistant",` + "\n",
		"", `{"type":"assistant","isApiErrorMessage":true}` + "\n", cutRecord,
		`{"type":"user","isApiErrorMessage":true,"streamCutCause":"stale_connection"}` + "\n", `{"type":"assistant","streamCutCause":"stale_connection"}` + "\n",
	} {
		if StreamCutTail([]byte(tail)) {
			t.Fatalf("%q: a non-cut tail read as a cut", tail)
		}
	}
}

// TestStreamCutTailRealRecord checks the classifier's field names against a
// record the harness really wrote, not one written by hand. The fixture is one
// line of a seat's transcript, sanitized: texts, identifiers, cwd and branch
// replaced, every key and flag kept.
func TestStreamCutTailRealRecord(t *testing.T) {
	t.Parallel()
	cut := readFixture(t, "stream_cut_real.jsonl")
	if !StreamCutTail(cut) {
		t.Fatalf("the real cut record read as not a cut")
	}
	ordinary := readFixture(t, "assistant_ok_real.jsonl")
	if StreamCutTail(append(append([]byte{}, cut...), ordinary...)) {
		t.Fatalf("the cut record followed by the seat's next ordinary record read as a cut")
	}
	// The harness writes a system turn_duration record at once after the cut
	// (record 165 of the seat transcript on gt-efj3xs that record 164, this
	// fixture, comes from: gt-6i52o7 holds the observation), so a cut is never
	// the literal last record. A seat that logs a
	// retry as a system record also reads cut while retrying; the revival
	// rule's five-minute quiet window absorbs that (gt-ze3t74).
	systemRecord := []byte(`{"type":"system","subtype":"turn_duration"}` + "\n")
	if !StreamCutTail(append(append([]byte{}, cut...), systemRecord...)) {
		t.Fatalf("the cut record followed by the harness's system record read as not a cut")
	}
}

func TestStreamCutTailRealOrdinaryRecord(t *testing.T) {
	t.Parallel()
	if StreamCutTail(readFixture(t, "assistant_ok_real.jsonl")) {
		t.Fatalf("the real ordinary assistant record read as a cut")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}
