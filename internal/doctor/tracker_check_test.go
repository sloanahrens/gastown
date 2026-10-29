package doctor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// trackerTestURL is the endpoint the tests configure. Nothing listens on it:
// the transport below answers the request in process, so no test opens a
// socket.
const trackerTestURL = "http://127.0.0.1:1"

// handlerTransport serves each request with h in process, so the check and the
// tracker client under it run their real request building and response
// handling without a connection.
type handlerTransport struct{ h http.Handler }

func (tr handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// failingTransport is a tracker that cannot be reached: every request fails
// the way a refused connection does.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// trackerReadyz answers /readyz with the given status code and body; every
// other path is a 404, so a check that asks the wrong route fails loudly.
func trackerReadyz(code int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
}

// newTrackerCheck is a check configured with a tracker endpoint and a token,
// answered by transport.
func newTrackerCheck(transport http.RoundTripper) *TrackerCheck {
	c := NewTrackerCheck()
	c.getenv = func(k string) string {
		return map[string]string{"TRACKER_URL": trackerTestURL, "TRACKER_TOKEN": "trk_test"}[k]
	}
	c.http = &http.Client{Transport: transport}
	return c
}

// trackerTown is a town whose route table puts one rig on the tracker.
func trackerTown(t *testing.T) string {
	t.Helper()
	return writeTownRoutes(t, `{"prefix":"tr-","path":"trackerrig","backend":"tracker"}`+"\n")
}

// A ready tracker is OK, with the schema level the server reported.
func TestTrackerCheckReadyIsOK(t *testing.T) {
	t.Parallel()
	c := newTrackerCheck(handlerTransport{trackerReadyz(http.StatusOK, `{"status":"ready","schema_level":3}`)})

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "schema level 3") {
		t.Errorf("message = %q, want the schema level the server reported", res.Message)
	}
}

// A tracker that answers not ready is ERROR, carrying the server's reason.
func TestTrackerCheckNotReadyIsError(t *testing.T) {
	t.Parallel()
	c := newTrackerCheck(handlerTransport{trackerReadyz(http.StatusServiceUnavailable,
		`{"kind":"unavailable","message":"database does not accept writes"}`)})

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR for a not-ready tracker", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "database does not accept writes") {
		t.Errorf("message = %q, want the server's reason", res.Message)
	}
}

// A 200 that does not say ready is ERROR too: the check reads the answer, not
// just the status code.
func TestTrackerCheckStatusThatIsNotReadyIsError(t *testing.T) {
	t.Parallel()
	c := newTrackerCheck(handlerTransport{trackerReadyz(http.StatusOK, `{"status":"starting","schema_level":3}`)})

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR for an answer that is not ready", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "starting") {
		t.Errorf("message = %q, want the status the server answered", res.Message)
	}
}

// A tracker nothing answers for is ERROR, not a pass.
func TestTrackerCheckUnreachableIsError(t *testing.T) {
	t.Parallel()
	c := newTrackerCheck(failingTransport{})

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR for an unreachable tracker", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "not ready") {
		t.Errorf("message = %q, want it to say the tracker is not ready", res.Message)
	}
}

// A town with no tracker-backed rig is not applicable: skipped, not OK (the
// check proved nothing) and not an error, and the tracker is never probed.
func TestTrackerCheckNotApplicableWithoutTrackerRigs(t *testing.T) {
	t.Parallel()
	town := writeTownRoutes(t, `{"prefix":"dl-","path":"doltrig"}`+"\n")
	c := NewTrackerCheck()
	c.getenv = func(k string) string {
		t.Errorf("read %s with no rig on the tracker; the check must not probe", k)
		return ""
	}
	c.http = &http.Client{Transport: failingTransport{}}

	res := c.Run(&CheckContext{TownRoot: town})
	if res.Status != StatusSkipped {
		t.Fatalf("status = %v (%s), want skipped when no rig is on the tracker", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "not applicable") {
		t.Errorf("message = %q, want it to say the check is not applicable", res.Message)
	}
}

// A tracker rig with no endpoint configured is ERROR: the rig has nowhere to
// read or write, which is what the check exists to surface.
func TestTrackerCheckMissingURLIsError(t *testing.T) {
	t.Parallel()
	c := NewTrackerCheck()
	c.getenv = func(k string) string {
		return map[string]string{"TRACKER_TOKEN": "trk_test"}[k]
	}

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR without TRACKER_URL", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "TRACKER_URL") {
		t.Errorf("message = %q, want it to name the missing setting", res.Message)
	}
}

// A tracker rig with no token is ERROR too: no rig on the tracker could be
// opened without one, so an OK here would hide a broken town.
func TestTrackerCheckMissingTokenIsError(t *testing.T) {
	t.Parallel()
	c := NewTrackerCheck()
	c.getenv = func(k string) string {
		return map[string]string{"TRACKER_URL": trackerTestURL}[k]
	}

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR without a token", res.Status, res.Message)
	}
}

// An unreadable route table leaves the check unable to say whether any rig is
// on the tracker, so it reports the read error rather than reading as clean.
func TestTrackerCheckUnreadableRoutesIsError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the routes file belongs cannot be read as one.
	if err := os.MkdirAll(filepath.Join(town, ".beads", beads.RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewTrackerCheck()
	c.getenv = func(string) string { return "" }

	res := c.Run(&CheckContext{TownRoot: town})
	if res.Status != StatusError {
		t.Fatalf("status = %v (%s), want ERROR with an unreadable routes file", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "cannot read the town routes") {
		t.Errorf("message = %q, want it to say the routes could not be read", res.Message)
	}
	if len(res.Details) == 0 {
		t.Error("details are empty, want the read error")
	}
}

// The endpoint is reported with its credentials stripped, so a doctor run's
// output can be shared without carrying them.
func TestTrackerCheckRedactsCredentialsInEndpoint(t *testing.T) {
	t.Parallel()
	c := NewTrackerCheck()
	c.getenv = func(k string) string {
		return map[string]string{
			"TRACKER_URL":   "https://operator:hunter2@tracker.example",
			"TRACKER_TOKEN": "trk_test",
		}[k]
	}
	c.http = &http.Client{Transport: handlerTransport{trackerReadyz(http.StatusOK, `{"status":"ready","schema_level":3}`)}}

	res := c.Run(&CheckContext{TownRoot: trackerTown(t)})
	if res.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", res.Status, res.Message)
	}
	if strings.Contains(res.Message, "hunter2") {
		t.Errorf("message = %q, want the endpoint's credentials withheld", res.Message)
	}
	if !strings.Contains(res.Message, "tracker.example") {
		t.Errorf("message = %q, want the endpoint named without its credentials", res.Message)
	}
}
