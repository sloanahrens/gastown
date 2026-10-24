package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The hub holds the panel its reader read, and the state payload carries it under
// mango: the panel the page is handed is the one the reader made of the file.
func TestPollMangoHoldsThePanelInTheStatePayload(t *testing.T) {
	t.Parallel()

	dir := mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
		mangoTargetMap("rejected_before_landing", 48, MangoEarning, "model = opus")))
	h := NewHub(Config{Now: func() time.Time { return mangoNow }, Mango: func() *Mango {
		return ReadMango(dir, mangoNow)
	}})

	h.pollMango()

	var st State
	mustUnmarshalState(t, h, &st)
	if st.Mango == nil || st.Mango.State != MangoFresh {
		t.Fatalf("the state payload holds %+v, want the fresh panel", st.Mango)
	}
	if len(st.Mango.Targets) != 1 || st.Mango.Counts.Earning != 1 {
		t.Errorf("the panel in the payload reads %+v, want its one earning target", st.Mango)
	}
}

// A reader that names a state it cannot read is still held: the page is told the
// panel is missing or in error rather than keeping an older panel alive.
func TestPollMangoReplacesAPanelWithTheStateItReads(t *testing.T) {
	t.Parallel()

	dir := mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
		mangoTargetMap("rejected_before_landing", 48, MangoEarning, "model = opus")))
	h := NewHub(Config{Now: func() time.Time { return mangoNow }, Mango: func() *Mango {
		return ReadMango(dir, mangoNow)
	}})
	h.pollMango()

	if err := os.Remove(filepath.Join(dir, mangoPanelFile)); err != nil {
		t.Fatal(err)
	}
	h.pollMango()

	var st State
	mustUnmarshalState(t, h, &st)
	if st.Mango == nil || st.Mango.State != MangoMissing {
		t.Errorf("after the panel went, the payload holds %+v, want the missing state", st.Mango)
	}
}

// A hub with no Mango reader leaves the key out of the state payload entirely,
// so a page on a town without a metrics writer is handed no panel at all.
func TestStatePayloadOmitsMangoWithNoReader(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{Now: func() time.Time { return mangoNow }})

	raw, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["mango"]; ok {
		t.Errorf("a hub with no Mango reader put mango in its state payload: %s", raw)
	}
}

// mustUnmarshalState decodes the hub's state payload the way the page does.
func mustUnmarshalState(t *testing.T, h *Hub, st *State) {
	t.Helper()
	raw, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, st); err != nil {
		t.Fatal(err)
	}
}
