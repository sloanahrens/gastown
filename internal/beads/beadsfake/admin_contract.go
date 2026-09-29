package beadsfake

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// AdminClient is a database that offers both surfaces the contracts pin.
type AdminClient interface {
	beads.Client
	Admin
}

// RunAdminContract checks the maintenance surface (Admin) every
// implementation must share. newImpl must return an empty database whose
// issue prefix is "gt". SQL, InitDatabase and GCWisps are not modelled by
// the fake (they are scripted or recorded), so they are not pinned here.
func RunAdminContract(t *testing.T, newImpl func(t *testing.T) AdminClient) {
	t.Helper()
	for _, c := range adminCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, newImpl(t))
		})
	}
}

var adminCases = []struct {
	name string
	run  func(t *testing.T, c AdminClient)
}{
	{"config", adminConfig},
	{"counts and stats", adminCounts},
	{"tables", adminTables},
	{"wisp list", adminWispList},
}

func adminConfig(t *testing.T, c AdminClient) {
	if v, err := c.ConfigGet("custom.contract_probe"); err != nil || v != "" {
		t.Errorf("ConfigGet(unset) = %q, %v; want \"\", nil", v, err)
	}
	if v, err := c.ConfigGet("issue_prefix"); err != nil || v != "gt" {
		t.Errorf("ConfigGet(issue_prefix) = %q, %v; want gt", v, err)
	}
	mustDo(t, "ConfigSet", c.ConfigSet("types.custom", "agent,rig"))
	if v, err := c.ConfigGet("types.custom"); err != nil || v != "agent,rig" {
		t.Errorf("ConfigGet after set = %q, %v", v, err)
	}
	mustDo(t, "ConfigSet again", c.ConfigSet("types.custom", "agent"))
	if v, err := c.ConfigGet("types.custom"); err != nil || v != "agent" {
		t.Errorf("ConfigGet after overwrite = %q, %v", v, err)
	}
}

func adminCounts(t *testing.T, c AdminClient) {
	if n, err := c.CountIssues(); err != nil || n != 0 {
		t.Errorf("CountIssues(empty) = %d, %v", n, err)
	}
	mustCreate(t, c, beads.CreateOptions{Title: "one", Priority: -1})
	two := mustCreate(t, c, beads.CreateOptions{Title: "two", Priority: -1})
	mustDo(t, "close", c.Close(two.ID))
	mustCreate(t, c, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	if n, err := c.CountIssues(); err != nil || n != 2 {
		t.Errorf("CountIssues = %d, %v; want 2 (closed counted, wisp not)", n, err)
	}
	raw, err := c.StatsJSON()
	if err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}
	var stats struct {
		Summary struct {
			Total  int `json:"total_issues"`
			Open   int `json:"open_issues"`
			Closed int `json:"closed_issues"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &stats); err != nil {
		t.Fatalf("StatsJSON is not the stats object: %v\n%s", err, raw)
	}
	if s := stats.Summary; s.Total != 2 || s.Open != 1 || s.Closed != 1 {
		t.Errorf("stats summary = %+v, want total 2 open 1 closed 1", s)
	}
}

func adminTables(t *testing.T, c AdminClient) {
	for _, name := range []string{"issues", "wisps", "labels", "dependencies", "wisp_dependencies"} {
		if !c.TableExists(name) {
			t.Errorf("TableExists(%s) = false", name)
		}
	}
	if c.TableExists("no_such_table") {
		t.Error("TableExists(no_such_table) = true")
	}
}

func adminWispList(t *testing.T, c AdminClient) {
	if got, err := c.MolWispList(); err != nil || len(got) != 0 {
		t.Errorf("MolWispList(empty) = %v, %v", ids(got), err)
	}
	open := mustCreate(t, c, beads.CreateOptions{Title: "open wisp", Priority: -1, Ephemeral: true})
	shut := mustCreate(t, c, beads.CreateOptions{Title: "closed wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "close wisp", c.Close(shut.ID))
	mustCreate(t, c, beads.CreateOptions{Title: "durable", Priority: -1})
	got, err := c.MolWispList()
	wantIDs(t, "MolWispList", got, err, open.ID)
	for _, w := range got {
		if w.Status != "open" || !w.Ephemeral || w.Title != "open wisp" {
			t.Errorf("wisp = %+v", w)
		}
		if _, err := time.Parse(time.RFC3339, w.UpdatedAt); err != nil {
			t.Errorf("wisp updated_at %q is not RFC 3339: %v", w.UpdatedAt, err)
		}
	}
}
