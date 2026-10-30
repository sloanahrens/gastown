package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/tracker/api"
)

// testTownStartGate is a gate over a fixed town whose config check and
// handshake are fn and config, counting the handshakes it runs.
func testTownStartGate(handshake func(ctx context.Context) error, config func() error) (*townStartGate, *int) {
	calls := 0
	g := &townStartGate{
		townRoot: func() string { return "/town" },
		config:   func(string) error { return config() },
		handshake: func(ctx context.Context, _ string) error {
			calls++
			return handshake(ctx)
		},
	}
	return g, &calls
}

func configOK() error { return nil }

// brokenTownConfig is what the town config check returns for a file that
// does not parse.
func brokenTownConfig() error {
	return &config.ParseError{Path: "/town/settings/config.json", Offset: 12, Line: 1, Column: 12, Err: errors.New("invalid character '}'")}
}

func findCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	c := lookupCommand(rootCmd, strings.Fields(path)[1:])
	if c == nil || c.CommandPath() != path {
		t.Fatalf("command %q not in the tree (found %v)", path, c)
	}
	return c
}

// TestBDHandshakeGatedCommandsExist fails when a gated command is renamed or
// removed, so the gate can never silently stop covering a town-running path.
func TestBDHandshakeGatedCommandsExist(t *testing.T) {
	t.Parallel()
	for path := range bdHandshakeGatedCommands {
		findCommand(t, path)
	}
	for _, want := range []string{"gt up", "gt daemon start", "gt daemon run", "gt sling", "gt crew start", "gt session start"} {
		if !bdHandshakeGatedCommands[want] {
			t.Errorf("%q must be gated by the bd handshake", want)
		}
	}
}

func TestBDHandshakeGate_OnlyTownRunningCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path  string
		gated bool
	}{
		{"gt up", true},
		{"gt daemon run", true},
		{"gt crew start", true},
		{"gt status", false},
		{"gt show", false},
		{"gt hook", false},
		{"gt daemon status", false},
		{"gt doctor", false},
	} {
		if got := requiresBDHandshake(findCommand(t, tc.path)); got != tc.gated {
			t.Errorf("requiresBDHandshake(%q) = %v, want %v", tc.path, got, tc.gated)
		}
	}
}

func TestPersistentPreRunRefusesGatedCommandOnFailedHandshake(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("%w: no contract_version", deps.ErrBDHandshake)
	g, calls := testTownStartGate(func(context.Context) error { return refusal }, configOK)

	err := gateTownCommand(findCommand(t, "gt up"), g)
	if !errors.Is(err, deps.ErrBDHandshake) {
		t.Fatalf("gateTownCommand(gt up) = %v, want the handshake refusal", err)
	}
	if *calls != 1 {
		t.Errorf("handshake ran %d times, want 1", *calls)
	}
}

// TestRequireBDHandshakeCachesOnlySuccess: a long-lived process (the daemon
// starts sessions for hours) must recover once bd or Dolt is fixed, so a
// refusal is re-checked on the next call and only a pass is remembered.
func TestRequireBDHandshakeCachesOnlySuccess(t *testing.T) {
	t.Parallel()
	fail := true
	g, calls := testTownStartGate(func(context.Context) error {
		if fail {
			return fmt.Errorf("%w: store unavailable", deps.ErrBDHandshake)
		}
		return nil
	}, configOK)
	if err := g.requireHandshake(); err == nil {
		t.Fatal("first call: want refusal")
	}
	fail = false
	if err := g.requireHandshake(); err != nil {
		t.Fatalf("after bd was fixed: %v", err)
	}
	fail = true
	if err := g.requireHandshake(); err != nil {
		t.Fatalf("a pass must be cached: %v", err)
	}
	if *calls != 2 {
		t.Errorf("handshake ran %d times, want 2 (refusal re-checked, pass cached)", *calls)
	}
}

func TestPersistentPreRunSkipsHandshakeForReadOnlyCommands(t *testing.T) {
	t.Parallel()
	g, calls := testTownStartGate(func(context.Context) error {
		return errors.New("must not run")
	}, configOK)
	if err := gateTownCommand(findCommand(t, "gt hook"), g); err != nil {
		t.Fatalf("gateTownCommand(gt hook) = %v", err)
	}
	if *calls != 0 {
		t.Fatalf("read-only command ran the handshake %d times", *calls)
	}
}

// TestBDHandshakeClassifiesEveryTownVerb: every start/run-style command is
// either gated or exempt with a stated reason, so a new town-running command
// fails this test until someone decides.
func TestBDHandshakeClassifiesEveryTownVerb(t *testing.T) {
	t.Parallel()
	for path, reason := range bdHandshakeNotTownRunning {
		findCommand(t, path)
		if reason == "" || bdHandshakeGatedCommands[path] {
			t.Errorf("%q: exempt entries need a reason and must not also be gated", path)
		}
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if bdHandshakeTownVerbs[c.Name()] {
			path := c.CommandPath()
			if !bdHandshakeGatedCommands[path] && bdHandshakeNotTownRunning[path] == "" {
				t.Errorf("%q starts or dispatches something: gate it (bdHandshakeGatedCommands) or exempt it with a reason (bdHandshakeNotTownRunning)", path)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	// Every gated command also refuses an unparseable town config file, and
	// does so before the handshake runs (gt-fcxe9.10).
	g, handshakes := testTownStartGate(func(context.Context) error { return nil }, brokenTownConfig)
	for path := range bdHandshakeGatedCommands {
		if err := gateTownCommand(findCommand(t, path), g); !errors.Is(err, config.ErrUnparseable) {
			t.Errorf("%q with an unparseable town config: gateTownCommand = %v, want the config refusal", path, err)
		}
	}
	if *handshakes != 0 {
		t.Errorf("handshake ran %d times; the config refusal must come first", *handshakes)
	}
}

// TestDefaultBDHandshakeRefusesOutsideATown: with no town root the handshake
// has no town store to read, so it refuses rather than reading whatever
// database the working directory resolves to.
func TestDefaultBDHandshakeRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	err := runTownStartHandshake(context.Background(), "")
	if !errors.Is(err, deps.ErrBDHandshake) || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("runTownStartHandshake outside a town = %v", err)
	}
}

// TestSessionGateRefusesAnUnparseableTownConfig: the gate every agent
// session start calls refuses on a broken config file, every time (not
// cached), even after the handshake passed.
func TestSessionGateRefusesAnUnparseableTownConfig(t *testing.T) {
	t.Parallel()
	broken := false
	g, _ := testTownStartGate(func(context.Context) error { return nil }, func() error {
		if broken {
			return brokenTownConfig()
		}
		return nil
	})
	if err := g.require(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	broken = true
	if err := g.require(); !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("config broken after a pass: require() = %v, want the config refusal", err)
	}
}

// writeTownFiles writes files (relative path to body) under a new town root.
func writeTownFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	town := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// TestSessionGateRefusesSpawnOnARealBrokenSettingsFile is G3-04 end to end
// with the real check: a broken settings/config.json refuses the spawn and
// names the file, instead of moving the role to the default agent.
func TestSessionGateRefusesSpawnOnARealBrokenSettingsFile(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"settings/config.json": "{\"role_agents\": {\"polecat\": \"deepseek-flash\",}}",
	})
	err := checkTownConfig(town)
	if !errors.Is(err, config.ErrUnparseable) || !strings.Contains(err.Error(), filepath.Join("settings", "config.json")) || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("checkTownConfig = %v, want a refusal naming settings/config.json and the offset", err)
	}
}

// TestDefaultTownConfigCheckRefusesOutsideATown: with no town root there are
// no config files to check, and "nothing to check" must not read as a pass.
func TestDefaultTownConfigCheckRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	if err := checkTownConfig(""); err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("checkTownConfig outside a town = %v, want a refusal", err)
	}
}

// TestDefaultTownConfigCheckPassesFromInsideATown: a town with valid config
// files passes.
func TestDefaultTownConfigCheckPassesFromInsideATown(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"mayor/daemon.json":    `{"type":"daemon-patrol-config","version":1}`,
		"settings/config.json": `{"type":"town-settings","version":1}`,
	})
	if err := checkTownConfig(town); err != nil {
		t.Fatalf("checkTownConfig = %v", err)
	}
}

// TestDefaultTownConfigCheckRefusesARootWithoutTownJSON: a root found by its
// mayor/ directory alone is not a town the gate lets run (gt-y3pgh.1).
func TestDefaultTownConfigCheckRefusesARootWithoutTownJSON(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/daemon.json": `{"type":"daemon-patrol-config","version":1}`,
	})
	if err := checkTownConfig(town); !errors.Is(err, townconfig.ErrNotATown) {
		t.Fatalf("checkTownConfig without town.json = %v, want ErrNotATown", err)
	}
}

// townOnTracker writes a town whose own route names the tracker backend and
// whose town.json points at baseURL, so the gate asks that server.
func townOnTracker(t *testing.T, baseURL string) string {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("baseURL %q has no port: %v", baseURL, err)
	}
	return writeTownFiles(t, map[string]string{
		"mayor/town.json":     fmt.Sprintf(`{"type":"town","version":2,"name":"t","tracker":{"host":%q,"port":%d}}`, u.Hostname(), port),
		".beads/routes.jsonl": `{"path":".","prefix":"hq-","backend":"tracker"}` + "\n",
	})
}

// neverRuns is a handshake that fails the test if the gate runs it.
func neverRuns(t *testing.T, name string) func(context.Context, string) error {
	return func(context.Context, string) error {
		t.Errorf("the gate ran the %s handshake, want the other one", name)
		return nil
	}
}

// trackerTownURL is the endpoint a tracker town's town.json names. The fake
// transport answers it in process, so no test dials it.
const trackerTownURL = "http://127.0.0.1:7433"

// trackerAnswer is what the probe's transport answers: a status and body, a
// body that fails mid-read, or a connection that cannot be made.
type trackerAnswer struct {
	status  int
	body    string
	cutBody bool
	down    bool
}

// readyAnswer is a tracker that answers ready at the level this gt speaks.
func readyAnswer() trackerAnswer {
	return trackerAnswer{status: http.StatusOK, body: fmt.Sprintf(`{"status":"ready","schema_level":%d}`, api.SchemaLevel)}
}

func (a trackerAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	if a.down {
		return nil, errors.New("dial tcp 127.0.0.1:7433: connect: connection refused")
	}
	var body io.ReadCloser = io.NopCloser(strings.NewReader(a.body))
	if a.cutBody {
		body = io.NopCloser(bodyFails{})
	}
	return &http.Response{StatusCode: a.status, Body: body, Header: http.Header{}}, nil
}

// bodyFails is a response body that fails on its first read, as a connection
// dropped mid-answer does.
type bodyFails struct{}

func (bodyFails) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// trackerHandshakeAs is the production tracker handshake with the probe's
// transport answering a, so the gate is driven end to end without a socket.
func trackerHandshakeAs(a trackerAnswer) func(context.Context, string) error {
	client := &http.Client{Transport: a}
	return func(ctx context.Context, townRoot string) error {
		return trackerTownHandshakeWith(ctx, townRoot, client)
	}
}

// TestTownStartHandshakeTrackerTownPasses: the gate a tracker town runs asks
// trackerd for readiness and its schema level, and a reachable, migrated
// trackerd passes it (gt-p85pw.110).
func TestTownStartHandshakeTrackerTownPasses(t *testing.T) {
	t.Parallel()
	err := townHandshakeFor(context.Background(), townOnTracker(t, trackerTownURL),
		neverRuns(t, "bd"), trackerHandshakeAs(readyAnswer()))
	if err != nil {
		t.Fatalf("a tracker town with a reachable, migrated trackerd refused the gate: %v", err)
	}
}

// TestTownStartHandshakeTrackerRefusals: every way a tracker town's store can
// fail the gate refuses, names the tracker and its URL, and is an
// ErrTrackerHandshake rather than a bd refusal.
func TestTownStartHandshakeTrackerRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer trackerAnswer
		wants  []string
	}{
		{
			name:   "unreachable",
			answer: trackerAnswer{down: true},
			wants:  []string{"did not answer"},
		},
		{
			name: "answered not ready",
			answer: trackerAnswer{status: http.StatusServiceUnavailable,
				body: `{"kind":"unavailable","message":"database does not accept writes"}`},
			wants: []string{"not ready", "database does not accept writes"},
		},
		{
			name:   "a status code with no reason",
			answer: trackerAnswer{status: http.StatusBadGateway, body: `{}`},
			wants:  []string{"answered 502"},
		},
		{
			name:   "a body that is not the readiness report",
			answer: trackerAnswer{status: http.StatusOK, body: "ok\n"},
			wants:  []string{"not its readiness report"},
		},
		{
			name:   "a body cut short",
			answer: trackerAnswer{status: http.StatusOK, cutBody: true},
			wants:  []string{"answer could not be read"},
		},
		{
			name:   "ready with no schema level",
			answer: trackerAnswer{status: http.StatusOK, body: `{"status":"ready","schema_level":0}`},
			wants:  []string{"no schema level"},
		},
		{
			name: "a tracker at another schema level",
			answer: trackerAnswer{status: http.StatusOK,
				body: fmt.Sprintf(`{"status":"ready","schema_level":%d}`, api.SchemaLevel+1)},
			wants: []string{"schema level", strconv.Itoa(api.SchemaLevel + 1), strconv.Itoa(api.SchemaLevel)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := townOnTracker(t, trackerTownURL)
			err := townHandshakeFor(context.Background(), town, neverRuns(t, "bd"), trackerHandshakeAs(tc.answer))
			if !errors.Is(err, deps.ErrTrackerHandshake) {
				t.Fatalf("gate = %v, want an ErrTrackerHandshake", err)
			}
			if errors.Is(err, deps.ErrBDHandshake) {
				t.Errorf("a tracker refusal reads as a bd refusal: %v", err)
			}
			if !strings.Contains(err.Error(), trackerTownURL) {
				t.Errorf("refusal does not name the tracker URL %s:\n%s", trackerTownURL, err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not say %q:\n%s", want, err)
				}
			}
		})
	}
}

// TestTownStartHandshakeDoltTownKeepsTheBDHandshake: a town whose own store is
// Dolt runs the bd handshake and returns its verdict unchanged, whatever a
// rig's route names — a bd rig's gate is the gate it always was.
func TestTownStartHandshakeDoltTownKeepsTheBDHandshake(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("%w: bd not found on PATH", deps.ErrBDHandshake)
	for _, tc := range []struct {
		name   string
		routes string
	}{
		{"no route table at all", ""},
		{"an empty route table", "# no routes\n"},
		{"the town's own route on Dolt", `{"path":".","prefix":"hq-"}` + "\n"},
		{
			name:   "a rig on the tracker while the town's own store is Dolt",
			routes: `{"path":".","prefix":"hq-"}` + "\n" + `{"path":"rig/mayor/rig","prefix":"rg-","backend":"tracker"}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.routes != "" {
				town = writeTownFiles(t, map[string]string{".beads/routes.jsonl": tc.routes})
			}
			bdRuns := 0
			err := townHandshakeFor(context.Background(), town,
				func(context.Context, string) error { bdRuns++; return refusal },
				neverRuns(t, "tracker"))
			if !errors.Is(err, refusal) {
				t.Fatalf("gate = %v, want the bd handshake's refusal", err)
			}
			if bdRuns != 1 {
				t.Errorf("bd handshake ran %d times, want 1", bdRuns)
			}
		})
	}
}

// TestTownStartHandshakeRefusesAnUnknownOrUnreadableRoute: a route table the
// gate cannot read, and a backend that is neither Dolt nor the tracker, both
// refuse rather than falling back to Dolt, and neither handshake runs.
func TestTownStartHandshakeRefusesAnUnknownOrUnreadableRoute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setUp func(t *testing.T) string
		want  []string
	}{
		{
			name: "a backend that is neither",
			setUp: func(t *testing.T) string {
				return writeTownFiles(t, map[string]string{
					".beads/routes.jsonl": `{"path":".","prefix":"hq-","backend":"postgres"}` + "\n",
				})
			},
			want: []string{"postgres", "dolt", "tracker"},
		},
		{
			name: "a route table that cannot be read",
			setUp: func(t *testing.T) string {
				town := t.TempDir()
				if err := os.MkdirAll(filepath.Join(town, ".beads", "routes.jsonl"), 0o755); err != nil {
					t.Fatal(err)
				}
				return town
			},
			want: []string{"reading the town's route table"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tc.setUp(t)
			err := townHandshakeFor(context.Background(), town, neverRuns(t, "bd"), neverRuns(t, "tracker"))
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not say %q:\n%s", want, err)
				}
			}
		})
	}
}

// TestTownStartHandshakeRefusesAnUnaskableTracker: a tracker town whose
// town.json names an endpoint that cannot be asked, or that cannot be read at
// all, refuses instead of passing.
func TestTownStartHandshakeRefusesAnUnaskableTracker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		town string
		want string
	}{
		{
			name: "a host that is not a URL host",
			town: `{"type":"town","version":2,"name":"t","tracker":{"host":"bad host","port":7433}}`,
			want: "cannot be asked",
		},
		{
			name: "a town.json that does not parse",
			town: `{"type":"town","version":2,"name":"t","tracker":{"port":"not-a-port"}}`,
			want: "tracker endpoint cannot be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runTownStartHandshake(context.Background(), writeTownFiles(t, map[string]string{
				"mayor/town.json":     tc.town,
				".beads/routes.jsonl": `{"path":".","prefix":"hq-","backend":"tracker"}` + "\n",
			}))
			if !errors.Is(err, deps.ErrTrackerHandshake) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gate = %v, want an ErrTrackerHandshake saying %q", err, tc.want)
			}
		})
	}
}

// fakeBDStore answers the bd handshake's two reads from a report a test writes,
// so the gate's bd path runs no bd subprocess (gt-pxlg).
type fakeBDStore struct {
	info beads.VersionInfo
}

func (f fakeBDStore) Version() (beads.VersionInfo, error) { return f.info, nil }

func (f fakeBDStore) SchemaLevel() (int, error) { return f.info.DBSchemaVersion, nil }

// TestTownStartHandshakeDoltTownRefusesAnUpstreamBD: the bd handshake still
// decides a Dolt town's gate, and an upstream bd build refuses it.
func TestTownStartHandshakeDoltTownRefusesAnUpstreamBD(t *testing.T) {
	t.Parallel()
	upstream := fakeBDStore{info: beads.VersionInfo{Version: "1.0.5", Build: "release"}}
	err := checkTownBDHandshake(context.Background(), upstream)
	if !errors.Is(err, deps.ErrBDHandshake) || !strings.Contains(err.Error(), "no build_id") {
		t.Fatalf("checkTownBDHandshake with an upstream bd = %v, want the bd handshake's refusal", err)
	}
}
