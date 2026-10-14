package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeRigSearch answers a rig's search from a table: JSON for a rig that
// answers, an error for one that does not. It records the argv each rig saw.
type fakeRigSearch struct {
	json map[string]string
	errs map[string]error
	seen map[string][]string
}

func (f *fakeRigSearch) search(rig beadSearchRig, args []string) ([]byte, error) {
	if f.seen == nil {
		f.seen = map[string][]string{}
	}
	f.seen[rig.name] = args
	if err := f.errs[rig.name]; err != nil {
		return nil, err
	}
	return []byte(f.json[rig.name]), nil
}

func searchRigs(names ...string) []beadSearchRig {
	rigs := make([]beadSearchRig, len(names))
	for i, n := range names {
		rigs[i] = beadSearchRig{name: n, beadsDir: "/town/" + n + "/.beads"}
	}
	return rigs
}

func TestBeadSearchGroupsMatchesByRig(t *testing.T) {
	t.Parallel()
	f := &fakeRigSearch{json: map[string]string{
		"gastown": `[{"id":"gt-1","title":"restart nudge lost","status":"closed","priority":1},
		             {"id":"gt-2","title":"nudge on restart","status":"open","priority":2}]`,
		"beads": `[{"id":"be-9","title":"nudge restart","status":"open","priority":0}]`,
	}}
	var out bytes.Buffer
	if err := runBeadSearch(&out, searchRigs("gastown", "beads"), f.search, []string{"restart", "nudge"}, 20, ""); err != nil {
		t.Fatal(err)
	}
	want := `gastown: gt-1 [P1] closed - restart nudge lost
gastown: gt-2 [P2] open - nudge on restart
beads: be-9 [P0] open - nudge restart
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestBeadSearchReportsUnavailableRig(t *testing.T) {
	t.Parallel()
	f := &fakeRigSearch{
		json: map[string]string{"gastown": `[{"id":"gt-1","title":"hit","status":"open","priority":2}]`},
		errs: map[string]error{
			"beads":   &beads.CLIError{Args: []string{"search"}, Stderr: []byte("connection refused\nsecond line"), Err: errors.New("exit 1")},
			"slowrig": fmt.Errorf("timed out after 5s: %w", context.DeadlineExceeded),
		},
	}
	var out bytes.Buffer
	if err := runBeadSearch(&out, searchRigs("gastown", "beads", "slowrig"), f.search, []string{"hit"}, 20, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"gastown: gt-1 [P2] open - hit\n",
		"rig beads: unavailable (connection refused)\n",
		"rig slowrig: unavailable (timed out after 5s)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestBeadSearchEveryRigUnavailableFails(t *testing.T) {
	t.Parallel()
	f := &fakeRigSearch{errs: map[string]error{"a": errors.New("down"), "b": errors.New("down")}}
	var out bytes.Buffer
	if err := runBeadSearch(&out, searchRigs("a", "b"), f.search, []string{"x"}, 20, ""); err == nil {
		t.Error("search succeeded with every rig unavailable")
	}
	if !strings.Contains(out.String(), "rig a: unavailable (down)") {
		t.Errorf("unavailable rig not printed:\n%s", out.String())
	}
}

func TestBeadSearchNoMatches(t *testing.T) {
	t.Parallel()
	f := &fakeRigSearch{json: map[string]string{"a": `[]`, "b": `[]`}}
	var out bytes.Buffer
	if err := runBeadSearch(&out, searchRigs("a", "b"), f.search, []string{"x"}, 20, ""); err != nil {
		t.Fatal(err)
	}
	if out.String() != "no matches in any rig\n" {
		t.Errorf("output %q", out.String())
	}
}

// TestBeadSearchPassesLimitAndStatusThrough pins the read-only bd argv: the
// words after "--", --status only when set, and one match more than --limit
// requested so a capped rig says so.
func TestBeadSearchPassesLimitAndStatusThrough(t *testing.T) {
	t.Parallel()
	f := &fakeRigSearch{json: map[string]string{
		"a": `[{"id":"a-1","title":"one","status":"open","priority":2},
		       {"id":"a-2","title":"two","status":"open","priority":2},
		       {"id":"a-3","title":"three","status":"open","priority":2}]`,
	}}
	var out bytes.Buffer
	if err := runBeadSearch(&out, searchRigs("a"), f.search, []string{"-dash", "word"}, 2, "open"); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"search", "--json", "--limit", "3", "--status", "open", "--", "-dash", "word"}
	if !reflect.DeepEqual(f.seen["a"], wantArgs) {
		t.Errorf("bd argv = %q, want %q", f.seen["a"], wantArgs)
	}
	want := "a: a-1 [P2] open - one\na: a-2 [P2] open - two\na: more matches not shown (--limit 2)\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}

	if got := beadSearchArgs([]string{"x"}, 20, ""); !reflect.DeepEqual(got, []string{"search", "--json", "--limit", "21", "--", "x"}) {
		t.Errorf("argv without --status = %q", got)
	}
	for _, bad := range []int{0, -1} {
		if err := runBeadSearch(&out, searchRigs("a"), f.search, []string{"x"}, bad, ""); err == nil {
			t.Errorf("--limit %d accepted", bad)
		}
	}
}

func TestBeadSearchRigsReadsRegistrySorted(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(town+"/mayor", 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, town+"/mayor/rigs.json", `{"version":1,"rigs":{"zeta":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"ze"}},"alpha":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"al"}}}}`)
	rigs, err := beadSearchRigs(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 2 || rigs[0].name != "alpha" || rigs[1].name != "zeta" {
		t.Fatalf("rigs = %+v", rigs)
	}
	if want := town + "/alpha/.beads"; rigs[0].beadsDir != want {
		t.Errorf("beadsDir = %q, want %q", rigs[0].beadsDir, want)
	}
}
