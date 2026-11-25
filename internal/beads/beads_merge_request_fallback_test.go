package beads

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// mergeRequestFallbackWisps is the `bd mol wisp list --json --all` answer the
// refusal fallback reads: two merge requests (one open, one closed — --all is
// what leaves the status filter to finishMergeRequests) and an agent wisp that
// carries another label.
const mergeRequestFallbackWisps = `{
  "count": 3,
  "schema_version": 1,
  "wisps": [
    {"id":"gt-wisp-mr-open","title":"Merge: gt-source","status":"open","priority":1,"type":"merge-request","labels":["gt:merge-request"],"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z"},
    {"id":"gt-wisp-mr-closed","title":"Merge: gt-old","status":"closed","priority":1,"type":"merge-request","labels":["gt:merge-request"],"created_at":"2026-06-28T00:00:00Z","updated_at":"2026-06-28T00:00:00Z"},
    {"id":"gt-gastown-polecat-agent","title":"polecat agent","status":"open","priority":2,"type":"agent","labels":["gt:agent"],"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z"}
  ]
}`

// mergeRequestFallbackShow is the `bd show --json` hydration. The description
// and the dependency are absent from the wisp list, so finding them on a
// result means the hydration ran as it does after a sql read.
const mergeRequestFallbackShow = `[
  {"id":"gt-wisp-mr-open","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"ephemeral":true,"labels":["gt:merge-request"],"dependencies":[{"id":"gt-source","status":"closed","issue_type":"task"}]},
  {"id":"gt-wisp-mr-closed","title":"Merge: gt-old","description":"branch: polecat/test/gt-old@def\ntarget: main\nsource_issue: gt-old\nrig: gastown\n","status":"closed","priority":1,"ephemeral":true,"labels":["gt:merge-request"]}
]`

// mergeRequestFallbackBD answers a ListMergeRequests run whose bd sql read is
// sql, serving wispList for the `mol wisp list` fallback and the show
// hydration above. Every other call is an empty answer.
func mergeRequestFallbackBD(sql, wispList reply) *recorder {
	return newRecorder(func(args []string) reply {
		switch args[0] {
		case "sql":
			return sql
		case "mol":
			return wispList
		case "show":
			return reply{stdout: mergeRequestFallbackShow}
		default:
			return reply{stdout: "[]"}
		}
	})
}

// wispListArgv is the wisp read a run recorded, "" when it ran none.
func wispListArgv(r *recorder) string {
	for _, call := range r.calls() {
		if call.args[0] == "mol" {
			return strings.Join(call.args, " ")
		}
	}
	return ""
}

func TestListMergeRequestsSQLRefused(t *testing.T) {
	t.Parallel()

	t.Run("the refusal falls back to the labelled wisps of the wisp list", func(t *testing.T) {
		t.Parallel()
		r := mergeRequestFallbackBD(
			reply{stderr: gatewaySQLRefusal, err: exitError{1}},
			reply{stdout: mergeRequestFallbackWisps},
		)
		b := newRecordedBeads(t.TempDir(), r)

		mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
		if err != nil {
			t.Fatalf("ListMergeRequests() error = %v, want the wisp list answer", err)
		}
		ids := make([]string, 0, len(mrs))
		for _, mr := range mrs {
			ids = append(ids, mr.ID)
		}
		if want := []string{"gt-wisp-mr-open", "gt-wisp-mr-closed"}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("ListMergeRequests() ids = %v, want %v: the labelled wisps, and nothing else in the list", ids, want)
		}
		if mrs[0].Description == "" || len(mrs[0].Dependencies) != 1 {
			t.Errorf("gt-wisp-mr-open = %+v, want the bd show hydration's description and dependency", mrs[0])
		}
		if got := wispListArgv(r); got != "mol wisp list --json --all" {
			t.Errorf("wisp read = %q, want %q: --all keeps closed merge requests for finishMergeRequests to filter itself", got, "mol wisp list --json --all")
		}
	})

	t.Run("the status filter still applies to the fallback wisps", func(t *testing.T) {
		t.Parallel()
		r := mergeRequestFallbackBD(
			reply{stderr: gatewaySQLRefusal, err: exitError{1}},
			reply{stdout: mergeRequestFallbackWisps},
		)
		b := newRecordedBeads(t.TempDir(), r)

		mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "open", Priority: -1})
		if err != nil {
			t.Fatalf("ListMergeRequests() error = %v", err)
		}
		if len(mrs) != 1 || mrs[0].ID != "gt-wisp-mr-open" {
			t.Fatalf("ListMergeRequests() = %v, want only the open merge request: opts.Status must filter the fallback wisps too", mrs)
		}
	})

	t.Run("a sql failure that is not the gateway refusal is returned", func(t *testing.T) {
		t.Parallel()
		r := mergeRequestFallbackBD(
			reply{stderr: "Error: connection refused", err: exitError{1}},
			reply{stdout: mergeRequestFallbackWisps},
		)
		b := newRecordedBeads(t.TempDir(), r)

		mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
		if err == nil {
			t.Fatalf("ListMergeRequests() = %d merge requests, nil error; only the gateway's refusal may fall back", len(mrs))
		}
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("ListMergeRequests() error = %v, want the sql failure carrying ErrUnavailable", err)
		}
		if got := wispListArgv(r); got != "" {
			t.Errorf("wisp read = %q after a sql failure that was not the gateway refusal, want none", got)
		}
	})

	t.Run("a failed wisp list fallback is an error", func(t *testing.T) {
		t.Parallel()
		r := mergeRequestFallbackBD(
			reply{stderr: gatewaySQLRefusal, err: exitError{1}},
			reply{stderr: "Error: no wisps table", err: exitError{1}},
		)
		b := newRecordedBeads(t.TempDir(), r)

		mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
		if err == nil {
			t.Fatalf("ListMergeRequests() = %d merge requests, nil error; an unreadable fallback must not read as an empty merge queue", len(mrs))
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ListMergeRequests() error = %v, want ErrUnavailable in its chain", err)
		}
	})

	t.Run("a wisp list fallback that printed prose is an error", func(t *testing.T) {
		t.Parallel()
		r := mergeRequestFallbackBD(
			reply{stderr: gatewaySQLRefusal, err: exitError{1}},
			reply{stdout: "No wisps found.\n"},
		)
		b := newRecordedBeads(t.TempDir(), r)

		mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
		if err == nil {
			t.Fatalf("ListMergeRequests() = %d merge requests, nil error; a non-JSON fallback answer must not read as an empty merge queue", len(mrs))
		}
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "parsing bd mol wisp list") {
			t.Fatalf("ListMergeRequests() error = %v, want the parse failure carrying ErrUnavailable", err)
		}
	})
}
