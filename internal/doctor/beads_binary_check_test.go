package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/tracker/api"
)

const forkVersionJSON = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"version":"1.2.2"}`

// The reports the stub in the integration tier and these cases answer with,
// parsed as the store hands them to the handshake.
var (
	forkReport  = beads.VersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}
	preD1Report = beads.VersionInfo{Version: "1.2.2", Build: "537accb", Commit: "537accbea9ca"}
)

// fakeStore answers the handshake's two reads: the report a test scripts and
// the migration level of the database behind it.
type fakeStore struct {
	info     beads.VersionInfo
	verErr   error
	level    int
	levelErr error
}

func (f fakeStore) Version() (beads.VersionInfo, error) { return f.info, f.verErr }

func (f fakeStore) SchemaLevel() (int, error) {
	if f.levelErr != nil {
		return 0, f.levelErr
	}
	return f.level, nil
}

func handshakeCheck(info beads.VersionInfo, versionErr error, dbLevel int) *BeadsBinaryCheck {
	c := NewBeadsBinaryCheck()
	c.lookPath = func(string) (string, error) { return "/opt/bd", nil }
	c.store = fakeStore{info: info, verErr: versionErr, level: dbLevel}
	return c
}

func TestBeadsBinaryCheck_Metadata(t *testing.T) {
	t.Parallel()
	check := NewBeadsBinaryCheck()
	if check.Name() != "beads-binary" {
		t.Errorf("Name() = %q, want %q", check.Name(), "beads-binary")
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
	if check.CanFix() {
		t.Error("CanFix() should return false: gastown never installs bd")
	}
}

func TestBeadsBinaryCheck_HandshakePasses(t *testing.T) {
	t.Parallel()
	result := handshakeCheck(forkReport, nil, 66).Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("status = %v: %s %v", result.Status, result.Message, result.Details)
	}
	for _, want := range []string{"da4983e", "schema<=66", "contract 1", "database at 66"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q lacks %q", result.Message, want)
		}
	}
}

func TestBeadsBinaryCheck_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		check *BeadsBinaryCheck
		want  string
	}{
		{"installed pre-D1 build", handshakeCheck(preD1Report, nil, 66), "no contract_version"},
		{"schema mismatch", handshakeCheck(forkReport, nil, 65), "database is at 65"},
		{"not on PATH", handshakeCheck(beads.VersionInfo{}, beads.ErrNotInstalled, 0), "not found on PATH"},
		{"unparseable version", handshakeCheck(beads.VersionInfo{}, errors.New(`bd version --json output is not JSON: "some garbage output"`), 0), "not JSON"},
		{"version fails", handshakeCheck(beads.VersionInfo{}, errors.New("bd version --json: exit status 1"), 0), "bd version --json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := tc.check.Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != StatusError {
				t.Fatalf("status = %v, want StatusError: %s", result.Status, result.Message)
			}
			if !strings.Contains(result.Message, tc.want) {
				t.Errorf("message %q lacks %q", result.Message, tc.want)
			}
			details := strings.Join(result.Details, "\n")
			if !strings.Contains(details, "found:") || !strings.Contains(details, "required:") {
				t.Errorf("details must name found vs required:\n%s", details)
			}
			if result.FixHint != deps.BDInstallHint {
				t.Errorf("fix hint = %q, want %q byte for byte", result.FixHint, deps.BDInstallHint)
			}
		})
	}
}

// TestBeadsBinaryCheck_DoltUnreachableNamesDolt is the refusal when bd passed
// its version check and the database did not answer: the report names Dolt and
// its commands, and never sends the operator to install bd (gt-j572r).
func TestBeadsBinaryCheck_DoltUnreachableNamesDolt(t *testing.T) {
	t.Parallel()
	c := NewBeadsBinaryCheck()
	c.lookPath = func(string) (string, error) { return "/opt/bd", nil }
	c.handshakeTimeout = 50 * time.Millisecond
	c.store = fakeStore{info: forkReport, levelErr: fmt.Errorf("%w: bd exit 25: store unavailable", beads.ErrStoreUnavailable)}
	result := c.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want StatusError: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "Dolt is not reachable") {
		t.Errorf("message %q does not name Dolt", result.Message)
	}
	for _, want := range []string{"gt dolt status", "gt dolt start"} {
		if !strings.Contains(result.FixHint, want) {
			t.Errorf("fix hint %q lacks %q", result.FixHint, want)
		}
	}
	if strings.Contains(result.FixHint, "make safe-install") {
		t.Errorf("fix hint %q sends the operator to install bd", result.FixHint)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "passed its version check") {
		t.Errorf("details must say the bd on PATH is not the problem:\n%s", details)
	}
	for _, unwanted := range []string{"make safe-install", "required:"} {
		if strings.Contains(details, unwanted) {
			t.Errorf("details must not blame bd with %q:\n%s", unwanted, details)
		}
	}
}

// A tracker town whose town.json cannot be read has no endpoint to ask, so the
// check is ERROR and names the file, rather than asking a guessed address.
func TestBeadsBinaryCheck_TrackerStoreUnreadableEndpoint(t *testing.T) {
	t.Parallel()
	town := writeTownRoutes(t, `{"path":".","prefix":"hq-","backend":"tracker"}`+"\n")
	writeTownJSON(t, town, `{"type":"town",`)
	result := trackerBinaryCheck(failingTransport{}).Run(&CheckContext{TownRoot: town})
	if result.Status != StatusError {
		t.Fatalf("status = %v: %s %v, want StatusError", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "cannot be read") {
		t.Errorf("message %q does not say the endpoint cannot be read", result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "town.json") {
		t.Errorf("details %v do not name the file", result.Details)
	}
}

// droppedTransport answers 200 and then drops the connection mid-answer: the
// body fails on every read.
type droppedTransport struct{}

func (droppedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(droppedBody{}),
		Request:    r,
	}, nil
}

// droppedBody is a body whose read fails, as a reset connection's does.
type droppedBody struct{}

func (droppedBody) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// A readiness URL no request can be built for is an error that names the
// tracker, and nothing is asked: the transport here would answer ready.
func TestReadyzTrackerRefusesAnUnbuildableURL(t *testing.T) {
	t.Parallel()
	tr := readyzTracker{url: "http://[::1", client: &http.Client{Transport: failingTransport{}}}
	_, err := tr.Readyz(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be asked") {
		t.Fatalf("Readyz() error = %v, want the tracker named as unaskable", err)
	}
}

// trackerStoreURL is the endpoint trackerStoreTown's town.json names.
const trackerStoreURL = "http://tracker.example:7777"

// trackerStoreTown is a town whose own route names the tracker backend, and
// whose town.json points the tracker at trackerStoreURL.
func trackerStoreTown(t *testing.T) string {
	t.Helper()
	town := writeTownRoutes(t, `{"path":".","prefix":"hq-","backend":"tracker"}`+"\n")
	writeTownJSON(t, town, `{"type":"town","version":2,"name":"t","tracker":{"host":"tracker.example","port":7777}}`)
	return town
}

// writeTownJSON writes the town's mayor/town.json.
func writeTownJSON(t *testing.T, town, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trackerBinaryCheck is a check whose bd store would pass: a tracker town must
// be decided by the tracker alone, so a pass from bd would hide a tracker that
// is not ready.
func trackerBinaryCheck(transport http.RoundTripper) *BeadsBinaryCheck {
	c := handshakeCheck(forkReport, nil, 66)
	c.trackerHTTP = &http.Client{Transport: transport}
	return c
}

// A tracker town is asked the tracker's question: ready at the schema level
// this gt speaks is OK, and anything else is ERROR naming the tracker's URL.
func TestBeadsBinaryCheck_TrackerStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		transport http.RoundTripper
		wantOK    bool
		want      string
	}{
		{"ready at the schema level", handlerTransport{trackerReadyz(http.StatusOK,
			fmt.Sprintf(`{"status":"ready","schema_level":%d}`, api.SchemaLevel))}, true,
			fmt.Sprintf("schema level %d", api.SchemaLevel)},
		{"not ready", handlerTransport{trackerReadyz(http.StatusServiceUnavailable,
			`{"kind":"unavailable","message":"database does not accept writes"}`)}, false,
			"database does not accept writes"},
		{"answers ok but not ready", handlerTransport{trackerReadyz(http.StatusOK,
			fmt.Sprintf(`{"status":"starting","schema_level":%d}`, api.SchemaLevel))}, false, `"starting"`},
		{"another schema level", handlerTransport{trackerReadyz(http.StatusOK,
			fmt.Sprintf(`{"status":"ready","schema_level":%d}`, api.SchemaLevel+1))}, false,
			fmt.Sprintf("schema level %d", api.SchemaLevel+1)},
		{"unreachable", failingTransport{}, false, "did not answer"},
		{"answers with no readiness report", handlerTransport{trackerReadyz(http.StatusOK, "not json")}, false,
			"not its readiness report"},
		{"fails with no reason", handlerTransport{trackerReadyz(http.StatusInternalServerError, `{}`)}, false,
			"answered 500 to its readiness probe"},
		{"drops the answer", droppedTransport{}, false, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := trackerBinaryCheck(tc.transport).Run(&CheckContext{TownRoot: trackerStoreTown(t)})
			if tc.wantOK {
				if result.Status != StatusOK {
					t.Fatalf("status = %v: %s %v, want OK", result.Status, result.Message, result.Details)
				}
				if !strings.Contains(result.Message, trackerStoreURL) || !strings.Contains(result.Message, tc.want) {
					t.Errorf("message %q lacks the tracker URL %q or %q", result.Message, trackerStoreURL, tc.want)
				}
				return
			}
			if result.Status != StatusError {
				t.Fatalf("status = %v: %s, want StatusError", result.Status, result.Message)
			}
			all := result.Message + "\n" + strings.Join(result.Details, "\n")
			if !strings.Contains(all, trackerStoreURL) {
				t.Errorf("report does not name the tracker URL %q:\n%s", trackerStoreURL, all)
			}
			if !strings.Contains(all, tc.want) {
				t.Errorf("report lacks %q:\n%s", tc.want, all)
			}
			if strings.Contains(all, "make safe-install") {
				t.Errorf("a tracker refusal sends the operator to install bd:\n%s", all)
			}
		})
	}
}

// A Dolt store names no tracker, so the tracker is never asked: the bd
// handshake decides, as it always has.
func TestBeadsBinaryCheck_DoltStoreNeverAsksTracker(t *testing.T) {
	t.Parallel()
	town := writeTownRoutes(t, `{"path":".","prefix":"hq-"}`+"\n")
	result := trackerBinaryCheck(failingTransport{}).Run(&CheckContext{TownRoot: town})
	if result.Status != StatusOK {
		t.Fatalf("status = %v: %s %v, want the bd handshake to pass", result.Status, result.Message, result.Details)
	}
}

// An unreadable route table does not say the store is Dolt. The bd handshake
// runs, and a town whose town.json configures a tracker must pass the tracker
// too, as the start gate requires (gt-p85pw.110).
func TestBeadsBinaryCheck_UnreadableRoutes(t *testing.T) {
	t.Parallel()
	unreadable := func(t *testing.T, townJSON string) string {
		town := t.TempDir()
		if err := os.MkdirAll(filepath.Join(town, ".beads", beads.RoutesFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		if townJSON != "" {
			writeTownJSON(t, town, townJSON)
		}
		return town
	}
	t.Run("a configured tracker that is not ready is ERROR", func(t *testing.T) {
		t.Parallel()
		town := unreadable(t, `{"type":"town","version":2,"name":"t","tracker":{"host":"tracker.example","port":7777}}`)
		result := trackerBinaryCheck(handlerTransport{trackerReadyz(http.StatusServiceUnavailable,
			`{"kind":"unavailable","message":"database does not accept writes"}`)}).Run(&CheckContext{TownRoot: town})
		if result.Status != StatusError {
			t.Fatalf("status = %v: %s %v, want StatusError", result.Status, result.Message, result.Details)
		}
		if all := result.Message + "\n" + strings.Join(result.Details, "\n"); !strings.Contains(all, trackerStoreURL) {
			t.Errorf("report does not name the tracker URL %q:\n%s", trackerStoreURL, all)
		}
	})
	t.Run("no configured tracker keeps the bd handshake", func(t *testing.T) {
		t.Parallel()
		result := trackerBinaryCheck(failingTransport{}).Run(&CheckContext{TownRoot: unreadable(t, "")})
		if result.Status != StatusOK {
			t.Fatalf("status = %v: %s %v, want the bd handshake to pass", result.Status, result.Message, result.Details)
		}
	})
}
