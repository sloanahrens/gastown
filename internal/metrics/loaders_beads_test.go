package metrics

import (
	"reflect"
	"testing"
)

// These fixtures are the Python beads tests' fixtures, line for line
// (scripts/test_town_metrics_loaders2.py), so a ported case has the same inputs
// and expected values as the case it replaces.

const bdListFixture = `[
 {"id": "gt-1", "created_at": "2026-10-01T00:00:00Z", "closed_at": "2026-10-02T00:00:00Z", "issue_type": "task",
  "priority": 1, "labels": ["handoff", "x"], "dependency_count": 2, "comment_count": 3,
  "acceptance_criteria": "- [ ] a",
  "notes": "x\nMERGE REJECTION (attempt 1): conflict\nMERGE REJECTION (attempt 2): gate\n"},
 {"id": "gt-2", "created_at": "2026-10-03T00:00:00Z", "closed_at": null, "issue_type": "bug",
  "labels": null, "dependency_count": 0, "comment_count": 0}
]`

// bdCall is one call a fake bd took.
type bdCall struct {
	dir  string
	args []string
}

// fakeBd answers every call with the same output, and records the calls.
func fakeBd(out string, ok bool, calls *[]bdCall) BdRun {
	return func(dir string, args []string) ([]byte, bool) {
		*calls = append(*calls, bdCall{dir, args})
		if !ok {
			return nil, false
		}
		return []byte(out), true
	}
}

func TestLoadBeadsParsesAndNormalisesLabels(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	var d Dropped
	rows := LoadBeads(fakeBd(bdListFixture, true, &calls), []string{"/g"}, &d)
	if got := []string{rows[0].ID, rows[1].ID}; !reflect.DeepEqual(got, []string{"gt-1", "gt-2"}) {
		t.Fatalf("ids = %v, want gt-1 and gt-2", got)
	}
	if !reflect.DeepEqual(rows[0].Labels, []string{"handoff", "x"}) {
		t.Errorf("labels = %v, want the two labels", rows[0].Labels)
	}
	if rows[0].Priority == nil || *rows[0].Priority != 1 {
		t.Errorf("priority = %v, want 1", rows[0].Priority)
	}
	if !reflect.DeepEqual(rows[1].Labels, []string{}) {
		t.Errorf("labels of a null list = %#v, want an empty list", rows[1].Labels)
	}
	if rows[0].Dir != "/g" || !rows[0].Shaped || rows[1].Shaped {
		t.Errorf("dir, shaped = %q, %v, %v; want /g, true, false", rows[0].Dir, rows[0].Shaped, rows[1].Shaped)
	}
	if rows[0].Rig != "gt" {
		t.Errorf("rig = %q, want gt", rows[0].Rig)
	}
	if want := []string{"conflict", "gate"}; !reflect.DeepEqual(rows[0].RejectionKinds, want) {
		t.Errorf("rejection kinds = %v, want %v", rows[0].RejectionKinds, want)
	}
	if len(rows[1].RejectionKinds) != 0 {
		t.Errorf("rejection kinds of a bead with no notes = %v, want none", rows[1].RejectionKinds)
	}
	if len(calls) != 1 {
		t.Errorf("bd calls = %d, want one bd list per directory", len(calls))
	}
	if want := []string{"list", "--all", "--limit", "0", "--json"}; len(calls) == 1 && !reflect.DeepEqual(calls[0].args, want) {
		t.Errorf("bd args = %v, want %v", calls[0].args, want)
	}
	if d.Count() != 0 {
		t.Errorf("dropped = %+v, want none", d.Items)
	}
}

func TestLoadBeadsCountsABeadSeenFromTwoDirectoriesOnce(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	rows := LoadBeads(fakeBd(bdListFixture, true, &calls), []string{"/g", "/h"}, &Dropped{})
	if got := []string{rows[0].ID, rows[1].ID}; !reflect.DeepEqual(got, []string{"gt-1", "gt-2"}) {
		t.Fatalf("ids = %v, want gt-1 and gt-2 once each", got)
	}
	if rows[0].Dir != "/g" {
		t.Errorf("dir = %q, want the first directory", rows[0].Dir)
	}
}

func TestLoadBeadsFailedListIsAStructuredFailure(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	var d Dropped
	LoadBeads(fakeBd("", false, &calls), []string{"/g"}, &d)
	if want := []string{"bd:/g"}; !reflect.DeepEqual(d.Failed, want) {
		t.Errorf("failed = %v, want %v", d.Failed, want)
	}
}

func TestLoadBeadsRetriesOnceThenDrops(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	var d Dropped
	rows := LoadBeads(fakeBd("", false, &calls), []string{"/g"}, &d)
	if len(rows) != 0 {
		t.Errorf("rows = %d, want none", len(rows))
	}
	if len(calls) != 2 {
		t.Errorf("bd calls = %d, want the call and one retry", len(calls))
	}
	if d.Count() != 1 {
		t.Errorf("dropped = %d, want 1", d.Count())
	}
}

// Output that is not JSON is given up at once. The Python does not retry it,
// and a retry would ask bd the same question for the same answer.
func TestBdJSONGivesUpAtOnceOnOutputThatIsNotJSON(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	var d Dropped
	LoadBeads(fakeBd("not json", true, &calls), []string{"/g"}, &d)
	if len(calls) != 1 {
		t.Errorf("bd calls = %d, want one: no retry for output that is not JSON", len(calls))
	}
	if want := []string{"bd:/g"}; !reflect.DeepEqual(d.Failed, want) {
		t.Errorf("failed = %v, want %v", d.Failed, want)
	}
}

func TestHandoffBytesSumsNotesAndComments(t *testing.T) {
	t.Parallel()
	show := `[{"notes": "abcd", "comments": [{"text": "ef"}, {"text": "ghi"}]}]`
	var calls []bdCall
	got := HandoffBytes(fakeBd(show, true, &calls), map[string]string{"gt-1": "/g"}, &Dropped{}, 150, nil)
	if want := map[string]int{"gt-1": 9}; !reflect.DeepEqual(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	if want := []string{"show", "--json", "--", "gt-1"}; len(calls) != 1 || !reflect.DeepEqual(calls[0].args, want) {
		t.Errorf("bd calls = %+v, want one call with args %v", calls, want)
	}
}

// A bd show that fails is retried once and then named as dropped; no size is
// written to the cache for it.
func TestHandoffBytesFailedShowIsDroppedAndNotCached(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	var d Dropped
	cache := map[string]int{}
	got := HandoffBytes(fakeBd("", false, &calls), map[string]string{"gt-1": "/g"}, &d, 150, cache)
	if len(got) != 0 || len(cache) != 0 {
		t.Errorf("sizes = %v, cache = %v; want neither for a failed show", got, cache)
	}
	if len(calls) != 2 {
		t.Errorf("bd calls = %d, want the call and one retry", len(calls))
	}
	if want := []DroppedItem{{Source: "bd:gt-1", Reason: "bd show failed"}}; !reflect.DeepEqual(d.Items, want) {
		t.Errorf("dropped = %+v, want %+v", d.Items, want)
	}
}

// A show whose output is JSON but holds no bead is dropped the same way.
func TestHandoffBytesShowWithNoBeadIsDropped(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"null", "[]", "{}"} {
		var calls []bdCall
		var d Dropped
		got := HandoffBytes(fakeBd(out, true, &calls), map[string]string{"gt-1": "/g"}, &d, 150, nil)
		if len(got) != 0 {
			t.Errorf("output %s: sizes = %v, want none", out, got)
		}
		if want := []DroppedItem{{Source: "bd:gt-1", Reason: "bd show failed"}}; !reflect.DeepEqual(d.Items, want) {
			t.Errorf("output %s: dropped = %+v, want %+v", out, d.Items, want)
		}
	}
}

// A bd list whose output is JSON null, an object or a list of non-beads is a
// failed source, not a listing with no beads.
func TestLoadBeadsRefusesAListingThatIsNotAnArrayOfBeads(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"null", `{"id": "gt-1"}`, "[1, 2]"} {
		var calls []bdCall
		var d Dropped
		rows := LoadBeads(fakeBd(out, true, &calls), []string{"/g"}, &d)
		if len(rows) != 0 {
			t.Errorf("output %s: rows = %d, want none", out, len(rows))
		}
		if want := []string{"bd:/g"}; !reflect.DeepEqual(d.Failed, want) {
			t.Errorf("output %s: failed = %v, want %v", out, d.Failed, want)
		}
	}
}

// A nil log is a log nobody reads: a refusal must not panic the loader.
func TestLoadersAcceptANilDroppedLog(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	if rows := LoadBeads(fakeBd("", false, &calls), []string{"/g"}, nil); len(rows) != 0 {
		t.Errorf("rows = %d, want none", len(rows))
	}
	if got := HandoffBytes(fakeBd("", false, &calls), map[string]string{"gt-1": "/g"}, nil, 150, nil); len(got) != 0 {
		t.Errorf("sizes = %v, want none", got)
	}
}

func TestHandoffBytesRespectsTheCallCap(t *testing.T) {
	t.Parallel()
	show := `[{"notes": "a", "comments": []}]`
	var calls []bdCall
	var d Dropped
	got := HandoffBytes(fakeBd(show, true, &calls), map[string]string{"a": "/g", "b": "/g", "c": "/g"}, &d, 2, nil)
	if len(calls) != 2 {
		t.Errorf("bd calls = %d, want the cap of 2", len(calls))
	}
	if want := map[string]int{"a": 1, "b": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	if want := []DroppedItem{{Source: "bd:c", Reason: "not fetched: call cap 2 reached"}}; !reflect.DeepEqual(d.Items, want) {
		t.Errorf("dropped = %+v, want %+v", d.Items, want)
	}
}

func TestHandoffBytesWritesFetchedSizesBackToTheCache(t *testing.T) {
	t.Parallel()
	show := `[{"notes": "abcd", "comments": []}]`
	var calls []bdCall
	cache := map[string]int{}
	HandoffBytes(fakeBd(show, true, &calls), map[string]string{"gt-1": "/g"}, &Dropped{}, 150, cache)
	if want := map[string]int{"gt-1": 4}; !reflect.DeepEqual(cache, want) {
		t.Errorf("cache = %v, want %v", cache, want)
	}
}

func TestHandoffBytesCacheSkipsCalls(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	got := HandoffBytes(fakeBd("", true, &calls), map[string]string{"gt-1": "/g"}, &Dropped{}, 150, map[string]int{"gt-1": 7})
	if want := map[string]int{"gt-1": 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	if len(calls) != 0 {
		t.Errorf("bd calls = %d, want none for a cached bead", len(calls))
	}
}
