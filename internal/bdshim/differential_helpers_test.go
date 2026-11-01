package bdshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdshim/golden"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store/storetest"
)

// The fixture town the differential test runs in: one rig on the tracker,
// whose beads carry the prefix the recorder initialized the fixture database
// with. Every row routes to it, by its ids or by the working directory.
const routeFixture = `{"path":".","prefix":"fx-","backend":"tracker"}`

// fixtureActor is the actor the recorder pinned for bd's environment
// (cmd/bdshim-record/container.go), so a row's links and beads read with the
// same author bd recorded.
const fixtureActor = "bdshim-record"

// The recorder's outputs, read from the package the shim's test runs in.
const (
	goldenDirectory = "../../cmd/bdshim/testdata/golden"
	fixtureFile     = "../../cmd/bdshim/testdata/fixture.jsonl"
)

// differentialRows is the argv of every golden, keyed by the file stem the
// recorder writes (cmd/bdshim-record/table.go). The stem drops a row's
// operands, so a golden file alone cannot say what bd was asked; a golden the
// table does not name fails the run rather than being skipped.
var differentialRows = []differentialRow{
	// Read rows.
	{stem: "show-json", argv: []string{"show", "--json", "fx-1"}},
	{stem: "list-json", argv: []string{"list", "--json"}},
	{stem: "list-json-status-open", argv: []string{"list", "--json", "--status", "open"}},
	{stem: "list-json-all-limit-0", argv: []string{"list", "--json", "--all", "--limit", "0"}},
	{stem: "list-json-parent-fx-parent", argv: []string{"list", "--json", "--parent", "fx-parent"}},
	{stem: "list-json-type-task", argv: []string{"list", "--json", "--type", "task"}},
	{stem: "list-json-label-gt-agent", argv: []string{"list", "--json", "--label", "gt:agent"}},
	{stem: "list-id-fx-1-include-infra-all-json-flat-no-pager", argv: []string{"list", "--id", "fx-1", "--include-infra", "--all", "--json", "--flat", "--no-pager"}},
	{stem: "ready-json", argv: []string{"ready", "--json"}},
	{stem: "ready-json-limit-5", argv: []string{"ready", "--json", "--limit", "5"}},
	{stem: "ready-json-unassigned", argv: []string{"ready", "--json", "--unassigned"}},
	{stem: "query-json", argv: []string{"query", "--json", "status=open"}},
	{stem: "query-json-all-limit-0", argv: []string{"query", "--json", "--all", "--limit", "0", "status=open"}},
	{stem: "search-json-limit-10-status-open", argv: []string{"search", "--json", "--limit", "10", "--status", "open", "fixture"}},
	{stem: "blocked-json", argv: []string{"blocked", "--json"}},
	{stem: "comments-json", argv: []string{"comments", "--json", "fx-1"}},
	{stem: "children-json", argv: []string{"children", "--json", "fx-parent"}},
	{stem: "count-status-open", argv: []string{"count", "--status", "open"}},
	{stem: "count-by-status", argv: []string{"count", "--by-status"}},
	{stem: "stats-json", argv: []string{"stats", "--json"}},
	{stem: "config-list", argv: []string{"config", "list"}},
	{stem: "config-get", argv: []string{"config", "get", "export.auto"}},
	{stem: "kv-list", argv: []string{"kv", "list"}},
	{stem: "kv-get", argv: []string{"kv", "get", "fixture-key"}},
	{stem: "dep-list-direction-down-json", argv: []string{"dep", "list", "--direction", "down", "--json", "fx-1"}},
	{stem: "label-list-json", argv: []string{"label", "list", "--json", "fx-1"}},
	{stem: "events-tail", argv: []string{"events", "tail", "0"}},

	// Mutating rows: each runs against a store seeded afresh, as the recorder
	// gave each of them a copy of the fixture.
	{stem: "create-title-fixture-created-type-task-priority-3-json", argv: []string{"create", "--title", "fixture-created", "--type", "task", "--priority", "3", "--json"}},
	{stem: "update-status-in-progress", argv: []string{"update", "--status", "in_progress", "fx-1"}},
	{stem: "close-reason-fixture-close", argv: []string{"close", "--reason", "fixture-close", "fx-1"}},
	{stem: "reopen", argv: []string{"reopen", "fx-1"}},
	{stem: "delete-force", argv: []string{"delete", "--force", "fx-1"}},
	{stem: "defer-reason-fixture-defer", argv: []string{"defer", "--reason", "fixture-defer", "fx-1"}},
	{stem: "undefer", argv: []string{"undefer", "fx-1"}},
	{stem: "unclaim", argv: []string{"unclaim", "fx-1"}},
	{stem: "dep-add", argv: []string{"dep", "add", "fx-1", "fx-2"}},
	{stem: "label-add", argv: []string{"label", "add", "fx-1", "fixture-label"}},
	{stem: "comment", argv: []string{"comment", "fx-1", "fixture note"}},
	{stem: "kv-set", argv: []string{"kv", "set", "fixture-key", "fixture-value"}},
	{stem: "config-set", argv: []string{"config", "set", "export.auto", "false"}},
}

// knownUnhandledVerbs is the tracker verbs the shim has no handler for yet
// (X3 to X6 of gt-p85pw), keyed as the registry is: bd's verb. A golden row
// whose verb is here is skipped. The list only shrinks: a test fails on an
// entry whose verb has gained a handler, and on a golden row whose verb is
// neither handled nor listed.
var knownUnhandledVerbs = map[string]bool{
	"comments": true,
	"stats":    true,
	"config":   true,
	"kv":       true,
	"dep":      true,
	"label":    true,
	"events":   true,
	"create":   true,
	"update":   true,
	"close":    true,
	"reopen":   true,
	"delete":   true,
	"defer":    true,
	"undefer":  true,
	"unclaim":  true,
	"comment":  true,
}

// newDifferentialCase is the run the committed goldens get: the real golden
// directory, the fixture file the recorder seeded from, and the shim's own
// handler registry.
func newDifferentialCase(t *testing.T) differentialCase {
	t.Helper()
	fixture, keep := readFixture(t)
	town := newTown(t, routeFixture)
	// A handler that resolves the town rather than walking for the route table
	// finds this one, as it would in a town, instead of finding none.
	writeFile(t, filepath.Join(town, "mayor", "town.json"), `{"type":"town","version":2,"name":"differential-test"}`+"\n")
	return differentialCase{
		dir:      goldenDirectory,
		rows:     differentialRows,
		handlers: handlers,
		known:    knownUnhandledVerbs,
		keep:     keep,
		run: func(argv []string) ([]byte, int, error) {
			return runFixtureRow(t, town, fixture, argv)
		},
	}
}

// fixtureBead is the part of a fixture line the tracker store is seeded from.
// The recorder's import carries the rest; a field neither this test nor the
// store's create takes is left out, and the golden normalizes the values a run
// decides (timestamps, minted ids) away.
type fixtureBead struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Description  string           `json:"description"`
	Status       string           `json:"status"`
	IssueType    string           `json:"issue_type"`
	Priority     int              `json:"priority"`
	CloseReason  string           `json:"close_reason"`
	Labels       []string         `json:"labels"`
	Dependencies []fixtureDep     `json:"dependencies"`
	Comments     []fixtureComment `json:"comments"`
}

// fixtureDep is one dependency a fixture line holds.
type fixtureDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// fixtureComment is one comment a fixture line holds.
type fixtureComment struct {
	IssueID string `json:"issue_id"`
	Author  string `json:"author"`
	Text    string `json:"text"`
}

// readFixture reads the fixture file the recorder seeded its database from, and
// returns every id it holds: an id the fixture names is one the goldens keep, so
// golden.Normalize leaves it alone.
func readFixture(t *testing.T) ([]fixtureBead, map[string]bool) {
	t.Helper()
	data, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("reading the fixture %s: %v", fixtureFile, err)
	}
	var fixture []fixtureBead
	keep := make(map[string]bool)
	for line, text := range bytes.Split(data, []byte("\n")) {
		text = bytes.TrimSpace(text)
		if len(text) == 0 {
			continue
		}
		var bead fixtureBead
		if err := json.Unmarshal(text, &bead); err != nil {
			t.Fatalf("fixture line %d is not a bead: %v", line+1, err)
		}
		fixture = append(fixture, bead)
		var raw map[string]any
		if err := json.Unmarshal(text, &raw); err != nil {
			t.Fatalf("fixture line %d is not a bead: %v", line+1, err)
		}
		golden.CollectIDs(raw, keep)
	}
	if len(fixture) == 0 {
		t.Fatalf("the fixture %s holds no bead", fixtureFile)
	}
	return fixture, keep
}

// fixtureTime is when the recorder's fixture database holds every bead: one
// import, one timestamp. Seeding at a fixed time reproduces both the golden's
// times and bd's list order, which breaks equal-priority ties by id; a real
// clock would stamp each bead separately and order them by seed time instead.
var fixtureTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedFixture returns a tracker store holding the fixture's beads, with their
// dependencies, comments and closed state.
func seedFixture(t *testing.T, fixture []fixtureBead) *storetest.Fake {
	t.Helper()
	ctx := api.WithIdentity(context.Background(), api.Identity{Actor: fixtureActor, Scope: api.ScopeAdmin})
	st := storetest.NewFakeWithClock(func() time.Time { return fixtureTime })
	for _, bead := range fixture {
		_, err := st.CreateIssue(ctx, api.CreateParams{
			ID: bead.ID, Title: bead.Title, Description: bead.Description,
			Type: bead.IssueType, Priority: bead.Priority, Labels: bead.Labels, Actor: fixtureActor,
		})
		if err != nil {
			t.Fatalf("seeding bead %s: %v", bead.ID, err)
		}
	}
	for _, bead := range fixture {
		for _, dep := range bead.Dependencies {
			if err := st.AddDependency(ctx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
				t.Fatalf("seeding dependency %s -> %s: %v", dep.IssueID, dep.DependsOnID, err)
			}
		}
		for _, comment := range bead.Comments {
			if err := st.AddComment(ctx, comment.IssueID, comment.Author, comment.Text); err != nil {
				t.Fatalf("seeding comment on %s: %v", comment.IssueID, err)
			}
		}
		if bead.Status == "closed" {
			if err := st.CloseIssues(ctx, []string{bead.ID}, bead.CloseReason, false); err != nil {
				t.Fatalf("seeding closed bead %s: %v", bead.ID, err)
			}
		}
	}
	return st
}

// runFixtureRow answers one row the way a real bd command would reach the
// handlers: the shim's own entry point, with a town of the fixture's, a tracker
// server holding the fixture, and the environment a seat carries. The store is
// seeded afresh, so a mutating row cannot change another row's answer.
func runFixtureRow(t *testing.T, town string, fixture []fixtureBead, argv []string) ([]byte, int, error) {
	t.Helper()
	env, stdout, _ := fixtureEnv(t, town, fixture, argv)
	// A row the router sends anywhere but the tracker rig cannot be compared
	// with a handler's answer, so it fails as a fixture the test cannot answer
	// rather than being diffed against bd.real.
	d, err := Decide(env.Getenv, env.Getwd, argv)
	if err != nil {
		return nil, 0, fmt.Errorf("routing %v: %w", argv, err)
	}
	if !d.Tracker {
		return nil, 0, fmt.Errorf("the fixture town routes %v to bd.real, not to its tracker rig", argv)
	}
	code := Main(env)
	return stdout.Bytes(), code, nil
}

// fixtureEnv is the environment one row reaches the shim's handlers with: a
// town of the fixture's, a tracker server holding a fresh copy of the fixture,
// and the token a seat carries. It also hands back the streams the answer and
// the shim's own messages are captured on.
func fixtureEnv(t *testing.T, town string, fixture []fixtureBead, argv []string) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ctx := context.Background()
	st := seedFixture(t, fixture)
	token, err := st.IssueTokenScoped(ctx, fixtureActor, api.ScopeAdmin, nil)
	if err != nil {
		t.Fatalf("issuing the fixture store's token: %v", err)
	}
	//testpolicy:allow no-network — a handler builds its tracker client from the environment (backend.FromEnv), so there is no HTTP seam to script: the row is only answered if the handler takes its own request path to this server, which is in-process over the in-memory store on loopback and never leaves the test
	ts := httptest.NewServer(server.New(server.Config{
		Store: st, Auth: st, Health: fixtureHealth{}, SchemaLevel: api.SchemaLevel,
	}).Handler())
	t.Cleanup(ts.Close)

	getenv := func(name string) string {
		switch name {
		case "TRACKER_URL":
			return ts.URL
		case "TRACKER_TOKEN":
			return token
		}
		return ""
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{
		Args:   argv,
		Getenv: getenv,
		Getwd:  func() (string, error) { return town, nil },
		Executable: func() (string, error) {
			return "", errors.New("the differential test has no bd.real")
		},
		Run: func(string, []string) error {
			return errors.New("the differential test never runs bd.real")
		},
		Handlers: handlers,
		Stdout:   stdout,
		Stderr:   stderr,
	}
	return env, stdout, stderr
}

// runInShim is a run that answers argv through Main against town, with the
// handlers the test registered and the answer captured.
func runInShim(t *testing.T, town string, handlers map[string]Handler) func([]string) ([]byte, int, error) {
	t.Helper()
	return func(argv []string) ([]byte, int, error) {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		env := Env{
			Args:   argv,
			Getenv: func(string) string { return "" },
			Getwd:  func() (string, error) { return town, nil },
			Executable: func() (string, error) {
				return "", errors.New("the differential test has no bd.real")
			},
			Run: func(string, []string) error {
				return errors.New("the differential test never runs bd.real")
			},
			Handlers: handlers,
			Stdout:   stdout,
			Stderr:   stderr,
		}
		code := Main(env)
		return stdout.Bytes(), code, nil
	}
}

// fixtureHealth is a tracker store that answers the server's probes.
type fixtureHealth struct{}

func (fixtureHealth) Ping(context.Context) error       { return nil }
func (fixtureHealth) WriteProbe(context.Context) error { return nil }
