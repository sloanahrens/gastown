//go:build integration

package trackerclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// TestIntegrationRealDataReadsTheSameThroughTheTrackerAsThroughBd compares what
// `bd show --json` returns for real issues with what the tracker adapter
// returns for the same issues imported into Postgres. It needs two running
// systems and is skipped unless they are named:
//
//	TRACKER_DIFF_BEADS_DIR  a .beads directory whose metadata points at a Dolt server holding the data
//	TRACKER_DIFF_URL        trackerd serving the same data after trackerimport
//	TRACKER_DIFF_TOKEN_FILE a token file for it
//	TRACKER_DIFF_RIG        the rig prefix to compare (default gt)
//	TRACKER_DIFF_SAMPLE     how many issues to compare (default 300)
//
// It prints every field that differs with a count and examples, and fails when
// any field of the compared set differs.
func TestIntegrationRealDataReadsTheSameThroughTheTrackerAsThroughBd(t *testing.T) {
	beadsDir, url, tokenFile := os.Getenv("TRACKER_DIFF_BEADS_DIR"), os.Getenv("TRACKER_DIFF_URL"), os.Getenv("TRACKER_DIFF_TOKEN_FILE")
	if beadsDir == "" || url == "" || tokenFile == "" {
		t.Skip("set TRACKER_DIFF_BEADS_DIR, TRACKER_DIFF_URL and TRACKER_DIFF_TOKEN_FILE to compare real data")
	}
	rig := envOr("TRACKER_DIFF_RIG", "gt")
	sample, err := strconv.Atoi(envOr("TRACKER_DIFF_SAMPLE", "300"))
	if err != nil || sample < 1 {
		t.Fatalf("TRACKER_DIFF_SAMPLE must be a positive integer, got %q", os.Getenv("TRACKER_DIFF_SAMPLE"))
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	tc, err := client.New(client.Config{BaseURL: url, TokenFile: tokenFile, HTTP: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	ad := trackerclient.New(tc, rig)

	all, err := tc.ListAll(context.Background(), api.ListFilter{Rig: rig, Statuses: []string{"all"}, IncludeInfra: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(all))
	for _, is := range all {
		ids = append(ids, is.ID)
	}
	sort.Strings(ids)
	step := max(1, len(ids)/max(sample, 1))
	var picked []string
	for i := 0; i < len(ids); i += step {
		picked = append(picked, ids[i])
	}
	t.Logf("comparing %d of %d durable issues of rig %s", len(picked), len(ids), rig)

	diffs := map[string][]string{}
	compared := 0
	for _, id := range picked {
		want, err := bdShow(beadsDir, id)
		if err != nil {
			t.Errorf("bd show %s: %v", id, err)
			continue
		}
		got, err := ad.Show(id)
		if err != nil {
			diffs["Show error"] = append(diffs["Show error"], fmt.Sprintf("%s: %v", id, err))
			continue
		}
		compared++
		for field, d := range compareIssues(want, got) {
			diffs[field] = append(diffs[field], id+": "+d)
		}
	}
	compareReadyBoards(t, beadsDir, tc, rig)
	// bd blocked lists blocked wisps too, which the tracker's Blocked leaves out as
	// its Ready does (the dashboard's queue panel counts work, not molecule steps).
	// The comparison is of the durable issues; the wisps are counted in the log.
	compareBdListWithTracker(t, "bd blocked", []string{"blocked", "--json"}, beadsDir, true, func() ([]string, error) {
		page, err := tc.Blocked(context.Background(), api.ReadyFilter{Rig: rig, Limit: 1000})
		if err != nil {
			return nil, err
		}
		for page.NextCursor != "" {
			next, err := tc.Blocked(context.Background(), api.ReadyFilter{Rig: rig, Limit: 1000, Cursor: page.NextCursor})
			if err != nil {
				return nil, err
			}
			page.Issues, page.NextCursor = append(page.Issues, next.Issues...), next.NextCursor
		}
		return idsOfAPI(page.Issues), nil
	})
	compareBdListWithTracker(t, "bd list (not closed, durable)", []string{"list", "--json", "--limit", "0"}, beadsDir, false, func() ([]string, error) {
		all, err := tc.ListAll(context.Background(), api.ListFilter{Rig: rig})
		return idsOfAPI(all), err
	})
	compareCommentsAndChildren(t, beadsDir, ad, picked)

	fields := make([]string, 0, len(diffs))
	for f := range diffs {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	t.Logf("compared %d issues; %d field(s) differ", compared, len(fields))
	if compared == 0 || compared < len(picked)/2 {
		t.Fatalf("only %d of %d sampled issues were compared: the test proved nothing", compared, len(picked))
	}
	for _, f := range fields {
		ex := diffs[f]
		t.Errorf("%s differs in %d of %d issues, for example:\n    %s", f, len(ex), compared, strings.Join(head(ex, 3), "\n    "))
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// bdTimeout bounds every bd call: a hung Dolt server fails the test, it does
// not hang it.
const bdTimeout = 60 * time.Second

// runBd runs the real bd against the Dolt copy and returns its stdout.
func runBd(beadsDir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", args...)
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// bdShow runs bd show against the Dolt copy.
func bdShow(beadsDir, id string) (*beads.Issue, error) {
	out, err := runBd(beadsDir, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	var arr []beads.Issue
	if err := json.Unmarshal(out, &arr); err != nil || len(arr) != 1 {
		return nil, fmt.Errorf("bd show output is not one issue: %v", err)
	}
	return &arr[0], nil
}

// compareIssues returns, per differing field, a short description. It compares
// what the beads.Issue type carries and the adapter fills.
func compareIssues(want, got *beads.Issue) map[string]string {
	d := map[string]string{}
	eq := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			d[name] = fmt.Sprintf("bd %q, tracker %q", trim(fmt.Sprint(a)), trim(fmt.Sprint(b)))
		}
	}
	eq("Title", want.Title, got.Title)
	eq("Description", want.Description, got.Description)
	eq("Design", want.Design, got.Design)
	eq("Notes", want.Notes, got.Notes)
	eq("Status", want.Status, got.Status)
	eq("Priority", want.Priority, got.Priority)
	eq("Type", want.Type, got.Type)
	eq("CreatedAt", want.CreatedAt, got.CreatedAt)
	eq("CreatedBy", want.CreatedBy, got.CreatedBy)
	eq("UpdatedAt", want.UpdatedAt, got.UpdatedAt)
	eq("ClosedAt", want.ClosedAt, got.ClosedAt)
	eq("CloseReason", want.CloseReason, got.CloseReason)
	eq("Parent", want.Parent, got.Parent)
	eq("Assignee", want.Assignee, got.Assignee)
	eq("Ephemeral", want.Ephemeral, got.Ephemeral)
	eq("AcceptanceCriteria", want.AcceptanceCriteria, got.AcceptanceCriteria)
	eq("WispType", want.WispType, got.WispType)
	eq("EventKind", want.EventKind, got.EventKind)
	eq("HookBead", want.HookBead, got.HookBead)
	eq("AgentState", want.AgentState, got.AgentState)
	wl, gl := slices.Clone(want.Labels), slices.Clone(got.Labels)
	sort.Strings(wl)
	sort.Strings(gl)
	if len(wl) == 0 && len(gl) == 0 {
		wl, gl = nil, nil
	}
	eq("Labels", wl, gl)
	// bd shows only dependencies on issues of its own database. The tracker holds
	// every rig in one database, so a dependency on another rig's issue is a
	// local one and is shown; compare the ones within the issue's own rig.
	eq("Dependencies", depKeys(sameRig(want.ID, want.Dependencies)), depKeys(sameRig(got.ID, got.Dependencies)))
	return d
}

func depKeys(ds []beads.IssueDep) []string {
	out := make([]string, 0, len(ds))
	for _, x := range ds {
		out = append(out, x.ID+"|"+x.DependencyType+"|"+x.Status)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func trim(s string) string {
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

// compareReadyBoards compares the ids bd ready and the tracker's Ready return
// for the dispatcher's own query: unassigned, with its label and type
// exclusions, no limit.
func compareReadyBoards(t *testing.T, beadsDir string, tc *client.Client, rig string) {
	t.Helper()
	labels := []string{"gt:agent", "gt:convoy", "gt:escalation", "gt:formula", "gt:handoff", "gt:keep", "gt:merge-request", "gt:merge-slot", "gt:message", "gt:queue", "gt:rig", "gt:role", "gt:standing-orders", "gt:wisp"}
	types := []string{"epic", "wisp", "message", "agent", "convoy", "role", "rig", "molecule", "gate"}
	out, err := runBd(beadsDir, "ready", "--json", "--unassigned", "--exclude-label", strings.Join(labels, ","), "--exclude-type", strings.Join(types, ","), "--limit", "0")
	if err != nil {
		t.Errorf("%v", err)
		return
	}
	var bdReady []beads.Issue
	if err := json.Unmarshal(out, &bdReady); err != nil {
		t.Errorf("bd ready output is not an issue list: %v", err)
		return
	}
	want := map[string]bool{}
	for _, is := range bdReady {
		want[is.ID] = true
	}
	got, err := tc.ReadyAll(context.Background(), api.ReadyFilter{Rig: rig, Unassigned: true, ExcludeLabels: labels, ExcludeTypes: types})
	if err != nil {
		t.Errorf("tracker ReadyAll: %v", err)
		return
	}
	have := map[string]bool{}
	for _, is := range got {
		have[is.ID] = true
	}
	var onlyBd, onlyTracker []string
	for id := range want {
		if !have[id] {
			onlyBd = append(onlyBd, id)
		}
	}
	for id := range have {
		if !want[id] {
			onlyTracker = append(onlyTracker, id)
		}
	}
	sort.Strings(onlyBd)
	sort.Strings(onlyTracker)
	t.Logf("ready board: bd %d, tracker %d", len(want), len(have))
	if len(onlyBd)+len(onlyTracker) > 0 {
		t.Errorf("the ready boards differ: only in bd %v; only in the tracker %v", head(onlyBd, 10), head(onlyTracker, 10))
	}
}

func idsOfAPI(is []api.Issue) []string {
	out := make([]string, len(is))
	for i, x := range is {
		out[i] = x.ID
	}
	return out
}

// compareBdListWithTracker runs a bd verb that lists issues and compares the
// ids with what the tracker answers for the same question.
func compareBdListWithTracker(t *testing.T, what string, args []string, beadsDir string, durableOnly bool, tracker func() ([]string, error)) {
	t.Helper()
	out, err := runBd(beadsDir, args...)
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	var list []beads.Issue
	if err := json.Unmarshal(out, &list); err != nil {
		t.Errorf("%s output is not an issue list: %v", what, err)
		return
	}
	if len(list) == 0 {
		t.Errorf("%s returned nothing: the data is not what this test needs", what)
		return
	}
	want := map[string]bool{}
	wisps := 0
	for _, is := range list {
		if durableOnly && (is.Ephemeral || strings.Contains(is.ID, "-wisp-")) {
			wisps++
			continue
		}
		want[is.ID] = true
	}
	if wisps > 0 {
		t.Logf("%s: %d wisps in bd's answer set aside", what, wisps)
	}
	got, err := tracker()
	if err != nil {
		t.Errorf("%s: tracker side: %v", what, err)
		return
	}
	have := map[string]bool{}
	for _, id := range got {
		have[id] = true
	}
	var onlyBd, onlyTracker []string
	for id := range want {
		if !have[id] {
			onlyBd = append(onlyBd, id)
		}
	}
	for id := range have {
		if !want[id] {
			onlyTracker = append(onlyTracker, id)
		}
	}
	sort.Strings(onlyBd)
	sort.Strings(onlyTracker)
	t.Logf("%s: bd %d, tracker %d", what, len(want), len(have))
	if len(onlyBd)+len(onlyTracker) > 0 {
		t.Errorf("%s differs: only in bd (%d) %v; only in the tracker (%d) %v", what, len(onlyBd), head(onlyBd, 8), len(onlyTracker), head(onlyTracker, 8))
	}
}

// compareCommentsAndChildren compares, for the sampled issues, bd comments and
// bd children with the adapter's Comments and Children.
func compareCommentsAndChildren(t *testing.T, beadsDir string, ad *trackerclient.Client, ids []string) {
	t.Helper()
	commentDiffs, childDiffs, checked := 0, 0, 0
	for _, id := range ids {
		out, err := runBd(beadsDir, "comments", id, "--json")
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		var want []beads.Comment
		if err := json.Unmarshal(out, &want); err != nil {
			t.Errorf("bd comments %s output is not a comment list: %v", id, err)
			continue
		}
		got, err := ad.Comments(id)
		if err != nil {
			t.Errorf("Comments(%s): %v", id, err)
			continue
		}
		checked++
		wk, gk := commentKeys(want), commentKeys(got)
		if !reflect.DeepEqual(wk, gk) {
			if commentDiffs++; commentDiffs <= 3 {
				t.Errorf("comments of %s differ:\n    bd      %v\n    tracker %v", id, trimList(wk), trimList(gk))
			}
		}
		kids, err := ad.Children(id)
		if err != nil {
			t.Errorf("Children(%s): %v", id, err)
			continue
		}
		cout, err := runBd(beadsDir, "children", id, "--json")
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		var bdKids []beads.Issue
		if err := json.Unmarshal(cout, &bdKids); err != nil {
			t.Errorf("bd children %s output is not an issue list: %v", id, err)
			continue
		}
		if !slices.Equal(sortedIDs(bdKids), sortedIDs(kids)) {
			if childDiffs++; childDiffs <= 3 {
				t.Errorf("children of %s differ: bd %v, tracker %v", id, sortedIDs(bdKids), sortedIDs(kids))
			}
		}
	}
	t.Logf("comments and children compared for %d issues: %d comment diffs, %d children diffs", checked, commentDiffs, childDiffs)
}

func commentKeys(cs []beads.Comment) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Author+"|"+c.CreatedAt+"|"+c.Text)
	}
	return out
}

func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, trim(s))
	}
	return out
}

func sortedIDs[T beads.Issue | *beads.Issue](is []T) []string {
	out := make([]string, 0, len(is))
	for _, x := range is {
		switch v := any(x).(type) {
		case beads.Issue:
			out = append(out, v.ID)
		case *beads.Issue:
			out = append(out, v.ID)
		}
	}
	sort.Strings(out)
	return out
}

// head is the first n of in, or all of it when shorter.
func head(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// sameRig keeps the dependencies whose target has the same id prefix as owner.
func sameRig(owner string, deps []beads.IssueDep) []beads.IssueDep {
	prefix, _, _ := strings.Cut(owner, "-")
	var out []beads.IssueDep
	for _, d := range deps {
		if p, _, _ := strings.Cut(d.ID, "-"); p == prefix {
			out = append(out, d)
		}
	}
	return out
}
