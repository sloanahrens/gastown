package beads_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These tests run the agent-bead helpers over beadsfake, the way any Client
// runs them now that they are free functions over beads.Client (gt-7iwy0.4.6).
// The helpers are exported from package beads, so the tests live in the
// external test package to import the fake without an import cycle.

// findAgentBeads creates the durable agent bead id and returns it.
func findAgentBeads(t *testing.T, c beads.Client, id string, fields *beads.AgentFields) *beads.Issue {
	t.Helper()
	issue, err := c.Create(beads.CreateOptions{
		ID:          id,
		Title:       id,
		Description: beads.FormatAgentDescription(id, fields),
		Labels:      []string{"gt:agent"},
		Priority:    -1,
	})
	if err != nil {
		t.Fatalf("creating agent bead %s: %v", id, err)
	}
	return issue
}

// TestListAgentBeadsOverClient covers the two sources ListAgentBeads merges:
// the durable issues carrying gt:agent, and the wisps that are agent beads —
// found by their label here, and by their ID shape in the shared-ID test
// below.
func TestListAgentBeadsOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	want := "gt-gastown-polecat-toast"
	findAgentBeads(t, c, want, &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "working"})

	// A wisp is an agent bead when it carries the label, even though bd's
	// wisp JSON is not guaranteed to report one (beads_agent.go's
	// isAgentBeadByID is the fallback for that).
	wisp, err := c.Create(beads.CreateOptions{Title: "wisp agent", Labels: []string{"gt:agent"}, Ephemeral: true, Priority: -1})
	if err != nil {
		t.Fatalf("creating agent wisp: %v", err)
	}
	// A wisp that is not an agent bead must not appear.
	if _, err := c.Create(beads.CreateOptions{Title: "ordinary wisp", Labels: []string{"gt:work"}, Ephemeral: true, Priority: -1}); err != nil {
		t.Fatalf("creating ordinary wisp: %v", err)
	}

	agents, err := beads.ListAgentBeads(c)
	if err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("ListAgentBeads = %d agents (%v), want the durable bead and the agent wisp", len(agents), agentIDs(agents))
	}
	if agents[want] == nil {
		t.Errorf("ListAgentBeads is missing the durable agent bead %s", want)
	}
	if agents[wisp.ID] == nil {
		t.Errorf("ListAgentBeads is missing the agent wisp %s", wisp.ID)
	}
}

// wispSource is a beadsfake with a scripted wisp read. A Fake keys every row
// by ID in one map, so it cannot hold a durable issue and a wisp under the
// same ID the way bd's two tables can; ListAgentBeads' precedence between
// them — and its ID-shape fallback for a wisp with no usable metadata — are
// what this Client exists to exercise.
type wispSource struct {
	*beadsfake.Fake
	wisps []*beads.Issue
}

var _ beads.Client = (*wispSource)(nil)

func (w *wispSource) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Ephemeral {
		return w.wisps, nil
	}
	return w.Fake.List(opts)
}

// TestListAgentBeadsIssueWinsOverWisp covers the duplicate-ID rule: an agent
// bead present in both tables is reported from the issues table, whose row
// carries the labels and type bd list's metadata comes from.
func TestListAgentBeadsIssueWinsOverWisp(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()

	const shared = "gt-gastown-polecat-toast"
	findAgentBeads(t, fake, shared, &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "working"})

	c := &wispSource{Fake: fake, wisps: []*beads.Issue{
		// The same ID as the durable bead: the issue must win.
		{ID: shared, Title: "wisp copy", Labels: []string{"gt:agent"}, Type: "task", Ephemeral: true},
		// No label and no type: the ID shape (prefix-rig-role) is the only
		// thing marking this one an agent bead.
		{ID: "gt-gastown-crew-max", Title: "crew by ID shape", Ephemeral: true},
		// Neither label, type nor ID shape: not an agent bead.
		{ID: "gt-wisp-plain", Title: "plain wisp", Labels: []string{"gt:work"}, Type: "task", Ephemeral: true},
	}}

	agents, err := beads.ListAgentBeads(c)
	if err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("ListAgentBeads = %d agents (%v), want the shared ID and the ID-shape wisp", len(agents), agentIDs(agents))
	}
	if got := agents[shared]; got == nil {
		t.Fatalf("ListAgentBeads is missing %s", shared)
	} else if got.Title != shared {
		t.Errorf("%s came from the wisp table (title %q); the issues-table row must win", shared, got.Title)
	}
	if agents["gt-gastown-crew-max"] == nil {
		t.Errorf("ListAgentBeads dropped the wisp whose only agent marker is its ID shape")
	}
}

// TestUpdateAgentStateOverClient covers the read-modify-write: the state
// lands in the description and the bead's other fields survive it.
func TestUpdateAgentStateOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	const id = "gt-gastown-polecat-toast"
	findAgentBeads(t, c, id, &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "spawning", HookBead: "gt-7iwy0.4.6",
	})

	if err := beads.UpdateAgentState(c, id, "working"); err != nil {
		t.Fatalf("UpdateAgentState: %v", err)
	}

	_, fields, err := beads.GetAgentBead(c, id)
	if err != nil {
		t.Fatalf("GetAgentBead: %v", err)
	}
	if fields == nil {
		t.Fatal("GetAgentBead returned no fields after UpdateAgentState")
	}
	if fields.AgentState != "working" {
		t.Errorf("AgentState = %q, want working", fields.AgentState)
	}
	if fields.RoleType != "polecat" || fields.Rig != "gastown" || fields.HookBead != "gt-7iwy0.4.6" {
		t.Errorf("UpdateAgentState lost the bead's other fields: %+v", fields)
	}

	// A bead that is not there is an error, not a silent success.
	if err := beads.UpdateAgentState(c, "gt-gastown-polecat-absent", "working"); err == nil {
		t.Error("UpdateAgentState on a missing bead returned nil, want an error")
	}
}

// TestAgentNotificationLevelOverClient covers the DND level read and write,
// including the default a bead with no level answers.
func TestAgentNotificationLevelOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	const id = "gt-gastown-polecat-toast"
	findAgentBeads(t, c, id, &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "working"})

	// Unset reads as the normal default.
	if level, err := beads.GetAgentNotificationLevel(c, id); err != nil || level != beads.NotifyNormal {
		t.Errorf("GetAgentNotificationLevel with no level = %q, %v; want %q, nil", level, err, beads.NotifyNormal)
	}

	if err := beads.UpdateAgentNotificationLevel(c, id, beads.NotifyMuted); err != nil {
		t.Fatalf("UpdateAgentNotificationLevel(muted): %v", err)
	}
	if level, err := beads.GetAgentNotificationLevel(c, id); err != nil || level != beads.NotifyMuted {
		t.Errorf("GetAgentNotificationLevel = %q, %v; want %q, nil", level, err, beads.NotifyMuted)
	}

	// "" resets to the default.
	if err := beads.UpdateAgentNotificationLevel(c, id, ""); err != nil {
		t.Fatalf("UpdateAgentNotificationLevel(\"\"): %v", err)
	}
	if level, err := beads.GetAgentNotificationLevel(c, id); err != nil || level != beads.NotifyNormal {
		t.Errorf("GetAgentNotificationLevel after reset = %q, %v; want %q, nil", level, err, beads.NotifyNormal)
	}

	// An unknown level is refused before anything is written.
	if err := beads.UpdateAgentNotificationLevel(c, id, "loud"); err == nil {
		t.Error("UpdateAgentNotificationLevel(loud) returned nil, want a refusal")
	}

	// A missing agent bead reads as the default; it is not an error.
	if level, err := beads.GetAgentNotificationLevel(c, "gt-gastown-polecat-absent"); err != nil || level != beads.NotifyNormal {
		t.Errorf("GetAgentNotificationLevel of a missing bead = %q, %v; want %q, nil", level, err, beads.NotifyNormal)
	}
}

// agentIDs returns the IDs of a map of agent beads, for failure messages.
func agentIDs(agents map[string]*beads.Issue) []string {
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	return ids
}

// TestCreateAgentBeadOverClient covers the create: the ID is the caller's, the
// bead carries gt:agent, and the description round-trips the fields. A
// flag-like title is refused before anything is written.
func TestCreateAgentBeadOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	const id = "gt-gastown-polecat-toast"
	issue, err := beads.CreateAgentBead(c, id, "Polecat toast", &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "spawning",
	})
	if err != nil {
		t.Fatalf("CreateAgentBead: %v", err)
	}
	if issue == nil || issue.ID != id {
		t.Fatalf("CreateAgentBead = %#v, want the bead under %s", issue, id)
	}

	stored, err := c.Show(id)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !beads.IsAgentBead(stored) {
		t.Errorf("bead %s is not an agent bead: labels %v type %q", id, stored.Labels, stored.Type)
	}
	fields := beads.ParseAgentFields(stored.Description)
	if fields.RoleType != "polecat" || fields.Rig != "gastown" || fields.AgentState != "spawning" {
		t.Errorf("created fields = %+v, want role_type=polecat rig=gastown agent_state=spawning", fields)
	}

	if _, err := beads.CreateAgentBead(c, "gt-gastown-polecat-bad", "--help", nil); err == nil {
		t.Error("CreateAgentBead accepted a flag-like title")
	}
}

// TestCreateOrReopenAgentBeadOverClient covers the upsert: the second call for
// an ID already in use takes the update path rather than failing the create,
// and a closed bead is reopened on the way.
func TestCreateOrReopenAgentBeadOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	const id = "gt-gastown-polecat-toast"
	if _, err := beads.CreateOrReopenAgentBead(c, id, "Polecat toast", &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "spawning",
	}); err != nil {
		t.Fatalf("CreateOrReopenAgentBead (create): %v", err)
	}

	issue, err := beads.CreateOrReopenAgentBead(c, id, "Polecat toast", &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "working", HookBead: "gt-7iwy0.4.7",
	})
	if err != nil {
		t.Fatalf("CreateOrReopenAgentBead (reopen): %v", err)
	}
	fields := beads.ParseAgentFields(issue.Description)
	if fields.AgentState != "working" || fields.HookBead != "gt-7iwy0.4.7" {
		t.Fatalf("reopened fields = %+v, want agent_state=working hook_bead=gt-7iwy0.4.7", fields)
	}

	// A closed bead is reopened, not left closed with fresh fields on it.
	if err := c.Close(id); err != nil {
		t.Fatalf("Close: %v", err)
	}
	issue, err = beads.CreateOrReopenAgentBead(c, id, "Polecat toast", &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "spawning",
	})
	if err != nil {
		t.Fatalf("CreateOrReopenAgentBead (closed bead): %v", err)
	}
	if issue.Status == "closed" {
		t.Fatalf("CreateOrReopenAgentBead left the bead closed: %#v", issue)
	}
}

// TestResetAgentBeadForReuseOverClient covers the nuke reset: mutable fields
// are cleared, agent_state reads nuked, and the immutable identity fields
// survive.
func TestResetAgentBeadForReuseOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	const id = "gt-gastown-polecat-toast"
	used := &beads.AgentFields{
		RoleType: "polecat", Rig: "gastown", AgentState: "done",
		HookBead: "gt-work", ActiveMR: "gt-wisp-mr", CleanupStatus: "has_uncommitted",
		ExitType: "COMPLETED", MRID: "gt-wisp-mr", Branch: "polecat/toast/gt-work",
		LastSourceIssue: "gt-work", MRFailed: true, PushFailed: true,
		CompletionTime: "2026-10-02T00:00:00Z",
	}
	findAgentBeads(t, c, id, used)

	if err := beads.ResetAgentBeadForReuse(c, id, "nuked"); err != nil {
		t.Fatalf("ResetAgentBeadForReuse: %v", err)
	}

	stored, err := c.Show(id)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if stored.Title != id {
		t.Errorf("title = %q, want the immutable %q preserved", stored.Title, id)
	}
	fields := beads.ParseAgentFields(stored.Description)
	if fields.AgentState != string(beads.AgentStateNuked) {
		t.Errorf("agent_state = %q, want %q", fields.AgentState, beads.AgentStateNuked)
	}
	if fields.HookBead != "" || fields.ActiveMR != "" || fields.CleanupStatus != "" || fields.Mode != "" {
		t.Errorf("reset left mutable fields behind: %+v", fields)
	}
	if fields.ExitType != "" || fields.MRID != "" || fields.Branch != "" || fields.LastSourceIssue != "" ||
		fields.MRFailed || fields.PushFailed || fields.CompletionTime != "" {
		t.Errorf("reset left completion metadata behind: %+v", fields)
	}
	if fields.RoleType != "polecat" || fields.Rig != "gastown" {
		t.Errorf("reset lost the identity fields: %+v", fields)
	}
}
