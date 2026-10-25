package contract

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "a graph creates a molecule: wisp root and steps, parent links, a blocks chain, and a link to an existing issue", Run: graphMolecule})
	register(Case{Name: "a graph of durable nodes gets ordinary ids and an external link", Run: graphDurable})
	register(Case{Name: "a graph that cannot be completed leaves nothing behind", Run: graphAtomic})
	register(Case{Name: "a malformed graph is refused as invalid", Run: graphValidation})
}

func graphCount(t *testing.T, s store.Store) int {
	t.Helper()
	st, err := s.Stats(ctx(), "")
	if err != nil {
		t.Fatal(err)
	}
	wisps, err := s.ListIssues(ctx(), api.ListFilter{Ephemeral: true, Statuses: []string{"all"}, IncludeInfra: true, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return st.Total + len(wisps.Issues)
}

func graphMolecule(t *testing.T, s store.Store) {
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "the work bead"})
	eph := func(key, title, typ string) api.GraphNode {
		return api.GraphNode{Key: key, CreateParams: api.CreateParams{Title: title, Type: typ, Ephemeral: true, Labels: []string{"gt:wisp"}}}
	}
	res, err := s.CreateGraph(asActor("town/polecats/a"), api.GraphRequest{
		Rig:   "gt",
		Nodes: []api.GraphNode{eph("root", "mol-polecat-work", "molecule"), eph("load", "Load context", "task"), eph("impl", "Implement", "task"), eph("submit", "Submit", "task")},
		Edges: []api.GraphEdge{
			{From: "load", To: "root", Type: "parent-child"}, {From: "impl", To: "root", Type: "parent-child"}, {From: "submit", To: "root", Type: "parent-child"},
			{From: "impl", To: "load", Type: "blocks"}, {From: "submit", To: "impl", Type: "blocks"},
			{From: "root", To: work.ID, Type: "blocks"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.IDs) != 4 {
		t.Fatalf("IDs = %v, want one per node", res.IDs)
	}
	for key, id := range res.IDs {
		is := mustGet(t, s, id)
		if !is.Ephemeral || !strings.Contains(id, "-wisp-") || is.CreatedBy != "town/polecats/a" {
			t.Errorf("node %s = %s %+v, want a wisp created by the token's actor", key, id, is)
		}
	}
	for _, key := range []string{"load", "impl", "submit"} {
		if p := mustGet(t, s, res.IDs[key]).Parent; p != res.IDs["root"] {
			t.Errorf("step %s has parent %q, want the root %s", key, p, res.IDs["root"])
		}
	}
	kids, err := s.ChildrenOf(ctx(), []string{res.IDs["root"]})
	if err != nil || len(kids[res.IDs["root"]]) != 3 {
		t.Errorf("ChildrenOf(root) = %v, %v; want the three steps", kids, err)
	}
	// Inside the molecule only the first step is ready: the chain is real.
	ready := mustReady(t, s, api.ReadyFilter{Molecule: res.IDs["root"]})
	if len(ready.Issues) != 1 || ready.Issues[0].ID != res.IDs["load"] {
		t.Errorf("ready steps = %v, want only the first (load)", ids(ready))
	}
	// The root depends on the existing bead, so a read of the root shows it.
	root := mustGet(t, s, res.IDs["root"])
	found := false
	for _, d := range root.Dependencies {
		found = found || (d.ID == work.ID && d.DependencyType == "blocks")
	}
	if !found {
		t.Errorf("root dependencies = %+v, want blocks on %s", root.Dependencies, work.ID)
	}
}

func graphDurable(t *testing.T, s store.Store) {
	res, err := s.CreateGraph(ctx(), api.GraphRequest{
		Rig:   "gt",
		Nodes: []api.GraphNode{{Key: "a", CreateParams: api.CreateParams{Title: "durable a"}}, {Key: "b", CreateParams: api.CreateParams{Title: "durable b", Priority: 1}}},
		Edges: []api.GraphEdge{{From: "b", To: "a", Type: "blocks"}, {From: "a", To: "external:hq:hq-abc", Type: "tracks"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, id := range res.IDs {
		if strings.Contains(id, "-wisp-") || mustGet(t, s, id).Ephemeral {
			t.Errorf("durable node %s got %s, want an ordinary id", key, id)
		}
	}
	if b := mustGet(t, s, res.IDs["b"]); len(b.BlockedBy) != 1 || b.BlockedBy[0] != res.IDs["a"] || b.Priority != 1 {
		t.Errorf("b = %+v, want blocked by a and priority 1", b)
	}
}

func graphAtomic(t *testing.T, s store.Store) {
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "already here"})
	before := graphCount(t, s)
	node := func(k string) api.GraphNode {
		return api.GraphNode{Key: k, CreateParams: api.CreateParams{Title: "n " + k}}
	}
	for name, tc := range map[string]struct {
		req  api.GraphRequest
		want *api.Error
	}{
		"a cycle among the nodes": {api.GraphRequest{Rig: "gt", Nodes: []api.GraphNode{node("a"), node("b"), node("c")},
			Edges: []api.GraphEdge{{From: "a", To: "b", Type: "blocks"}, {From: "b", To: "c", Type: "blocks"}, {From: "c", To: "a", Type: "blocks"}}}, api.ErrConflict},
		"a link to an issue that does not exist": {api.GraphRequest{Rig: "gt", Nodes: []api.GraphNode{node("a"), node("b")},
			Edges: []api.GraphEdge{{From: "a", To: "b", Type: "blocks"}, {From: "b", To: "gt-nosuch", Type: "blocks"}}}, api.ErrNotFound},
		"a rig that does not exist": {api.GraphRequest{Rig: "nosuchrig", Nodes: []api.GraphNode{node("a")}}, api.ErrInvalid},
	} {
		if _, err := s.CreateGraph(ctx(), tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: CreateGraph = %v, want %v", name, err, tc.want.Kind)
		}
		if after := graphCount(t, s); after != before {
			t.Errorf("%s: %d issues after, %d before; a failed graph must leave nothing", name, after, before)
		}
	}
}

func graphValidation(t *testing.T, s store.Store) {
	n := func(k string) api.GraphNode { return api.GraphNode{Key: k, CreateParams: api.CreateParams{Title: "t"}} }
	many := make([]api.GraphNode, api.MaxGraphNodes+1)
	for i := range many {
		many[i] = n(fmt.Sprintf("k%d", i))
	}
	bad := map[string]api.GraphRequest{
		"no rig":                    {Nodes: []api.GraphNode{n("a")}},
		"no nodes":                  {Rig: "gt"},
		"too many nodes":            {Rig: "gt", Nodes: many},
		"a node without a key":      {Rig: "gt", Nodes: []api.GraphNode{{CreateParams: api.CreateParams{Title: "t"}}}},
		"a repeated key":            {Rig: "gt", Nodes: []api.GraphNode{n("a"), n("a")}},
		"a node without a title":    {Rig: "gt", Nodes: []api.GraphNode{{Key: "a"}}},
		"a node that sets an id":    {Rig: "gt", Nodes: []api.GraphNode{{Key: "a", CreateParams: api.CreateParams{Title: "t", ID: "gt-x"}}}},
		"a node that sets a parent": {Rig: "gt", Nodes: []api.GraphNode{{Key: "a", CreateParams: api.CreateParams{Title: "t", Parent: "gt-x"}}}},
		"a node that sets an actor": {Rig: "gt", Nodes: []api.GraphNode{{Key: "a", CreateParams: api.CreateParams{Title: "t", Actor: "x"}}}},
		"an edge from a non-key":    {Rig: "gt", Nodes: []api.GraphNode{n("a")}, Edges: []api.GraphEdge{{From: "gt-x", To: "a", Type: "blocks"}}},
		"an edge to itself":         {Rig: "gt", Nodes: []api.GraphNode{n("a")}, Edges: []api.GraphEdge{{From: "a", To: "a", Type: "blocks"}}},
		"an edge with a bad type":   {Rig: "gt", Nodes: []api.GraphNode{n("a"), n("b")}, Edges: []api.GraphEdge{{From: "a", To: "b", Type: "Bad Type"}}},
		"a bad external reference":  {Rig: "gt", Nodes: []api.GraphNode{n("a")}, Edges: []api.GraphEdge{{From: "a", To: "external:nope", Type: "tracks"}}},
		"an edge with no end":       {Rig: "gt", Nodes: []api.GraphNode{n("a")}, Edges: []api.GraphEdge{{From: "a", Type: "blocks"}}},
	}
	before := graphCount(t, s)
	for name, req := range bad {
		if _, err := s.CreateGraph(ctx(), req); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: CreateGraph = %v, want api.ErrInvalid", name, err)
		}
	}
	if after := graphCount(t, s); after != before {
		t.Errorf("%d issues after the refused graphs, %d before", after, before)
	}
}
