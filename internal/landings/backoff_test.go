package landings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// backoffLine is a snapshot as the landing worker (gt-fn9e6.44) writes it:
// json.Marshal of its backoff state.
const backoffLine = `{"rig":"gastown","at":"2026-10-05T12:00:00Z","beads":[{"bead":"gt-abc","stage":"push","failures":3,"next_try":"2026-10-05T12:04:00Z","error":"the pre-push hook refused land/gt-abc"}]}`

func TestBackoffState_MatchesTheWorkersKeys(t *testing.T) {
	t.Parallel()
	var st BackoffState
	if err := json.Unmarshal([]byte(backoffLine), &st); err != nil {
		t.Fatal(err)
	}
	want := BackoffState{
		Rig: "gastown", At: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
		Beads: []BackoffRecord{{
			Bead: "gt-abc", Stage: "push", Failures: 3,
			NextTry: time.Date(2026, 10, 5, 12, 4, 0, 0, time.UTC),
			Error:   "the pre-push hook refused land/gt-abc",
		}},
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %+v\nwant %+v", st, want)
	}
	// Every key the worker writes maps to a field and back: no key is
	// dropped, which is what makes the reader a mirror rather than a guess.
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if keys(t, out) != keys(t, []byte(backoffLine)) {
		t.Fatalf("key set drifted:\n got %s\nwant %s", keys(t, out), keys(t, []byte(backoffLine)))
	}
}

func TestBackoffPath(t *testing.T) {
	t.Parallel()
	got, err := BackoffPath("/town", "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/town/.runtime/landings/gastown.backoff.json" {
		t.Fatalf("path %q", got)
	}
	for _, rig := range []string{"", "..", "a/b", `a\b`} {
		if _, err := BackoffPath("/town", rig); err == nil {
			t.Errorf("BackoffPath(%q) was accepted", rig)
		}
	}
}

// TestReadBackoff: a rig the worker has never written for has nothing
// failing, which is green rather than unreadable. An unreadable file is an
// error, because a question nobody can answer is not a healthy answer.
func TestReadBackoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.backoff.json")
	st, err := ReadBackoff(missing)
	if err != nil || st.Rig != "" || len(st.Beads) != 0 {
		t.Fatalf("ReadBackoff(missing) = %+v, %v; want an empty state and no error", st, err)
	}

	path := filepath.Join(dir, "gastown.backoff.json")
	if err := os.WriteFile(path, []byte(backoffLine), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = ReadBackoff(path)
	if err != nil || len(st.Beads) != 1 || st.Beads[0].Bead != "gt-abc" || st.Beads[0].Failures != 3 {
		t.Fatalf("ReadBackoff = %+v, %v; want the written snapshot", st, err)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBackoff(path); err == nil {
		t.Fatal("ReadBackoff read a malformed snapshot without an error")
	}
}
