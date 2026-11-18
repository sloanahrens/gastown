package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// probeGateway stands in for the bd gateway's HTTP surface. It keeps the live tree's
// hash the way the gateway does: a PUT stores the document's manifest and makes it
// the live hash, and every answer carries the live hash. A test moves the live hash,
// or makes the gateway absent or unreachable, the way a restart would.
type probeGateway struct {
	mu     sync.Mutex
	live   string
	absent bool
	down   bool
	puts   int
	putc   chan int
}

// Do answers one request. The reconcile loop and the probe both send through it.
func (g *probeGateway) Do(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return nil, errors.New("connection refused")
	}
	if req.Method == http.MethodPut {
		var doc struct {
			ManifestSHA string `json:"manifest_sha"`
		}
		data, err := io.ReadAll(req.Body)
		if err != nil || json.Unmarshal(data, &doc) != nil {
			return nil, errors.New("probe gateway: the document is not readable")
		}
		g.live, g.absent = doc.ManifestSHA, false
		g.puts++
		g.putc <- g.puts
		return g.answer(http.StatusOK, `{"manifest_sha":"`+doc.ManifestSHA+`"}`), nil
	}
	if g.absent {
		return g.answer(http.StatusBadRequest, `{"code":"bad_request","message":"bdgateway: the workspace is not present: no tree"}`), nil
	}
	return g.answer(http.StatusOK, `{}`), nil
}

// answer is a gateway response carrying the live hash, as every response does.
func (g *probeGateway) answer(status int, body string) *http.Response {
	h := http.Header{}
	h.Set(bdgateway.WorkspaceHashHeader, g.live)
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func (g *probeGateway) setLive(hash string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.live = hash
}

func (g *probeGateway) setAbsent(absent bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.absent = absent
}

func (g *probeGateway) setDown(down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = down
}

func (g *probeGateway) putCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.puts
}

// probeRun is a reconcile whose gateway is the real GatewayPusher over a
// probeGateway, so a probe's answer reaches the drift check through the Observe
// the daemon installs, as it does in production.
type probeRun struct {
	d    *Daemon
	clk  *clockwork.FakeClock
	gw   *probeGateway
	logs *lockedBuffer
}

// startProbeRun starts the reconcile against a town it can push, and waits for the
// start push and the two clock waits (the tick and the probe) it sets.
func startProbeRun(t *testing.T) *probeRun {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	r := &probeRun{
		clk:  newFixedClock(),
		gw:   &probeGateway{putc: make(chan int, 10)},
		logs: &lockedBuffer{},
	}
	env := map[string]string{
		bdgateway.WorkspaceURLEnv:   "http://localhost:7440",
		bdgateway.WorkspaceTokenEnv: token,
	}
	r.d = newWorkspaceDaemon(t, env, r.clk, nil, r.logs, nil)
	r.d.config.TownRoot = writeProbeTown(t)
	r.d.workspaceHTTP = r.gw
	// The loop pushes through the process-wide installed pusher, which the run
	// fills with its gateway pusher. The run clears it again at cleanup.
	t.Cleanup(func() { bdgateway.SetWorkspacePusher(nil) })
	if !r.d.startWorkspaceReconcile() {
		t.Fatal("startWorkspaceReconcile() = false with a gateway named, want true")
	}
	if got := awaitPush(t, r.gw.putc); got != 1 {
		t.Fatalf("start push = PUT %d, want 1", got)
	}
	awaitWaits(t, r.clk, 2)
	return r
}

// writeProbeTown writes the smallest town the push can build a document from.
func writeProbeTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"mayor/town.json":      `{"type":"town","version":1,"name":"fx"}`,
		"mayor/rigs.json":      `{"version":1,"rigs":{}}`,
		".beads/metadata.json": `{"dolt_database":"hq"}` + "\n",
		".beads/config.yaml":   "issue-prefix: hq\n",
		".beads/routes.jsonl":  `{"path":".","prefix":"hq-"}` + "\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return root
}

// assertNoDrift fails the test if the drift check holds a drift that the loop has
// not taken: a probe that should have been quiet recorded one.
func assertNoDrift(t *testing.T, d *Daemon) {
	t.Helper()
	if pending, _ := d.workspaceDrift.take(); pending {
		t.Fatal("the probe recorded a drift, want none")
	}
}

// TestWorkspaceDriftProbePushesOnDifferingHeader: a gateway whose probe answer
// shows a hash other than the stored tree is pushed to at the probe, not at the next
// tick. The push stores, and the next probe, which matches it, pushes nothing.
//
//testpolicy:allow parallel — installs the process-wide workspace pusher the reconcile loop pushes through, which another test must not see mid-run
func TestWorkspaceDriftProbePushesOnDifferingHeader(t *testing.T) {
	r := startProbeRun(t)
	start := r.clk.Now()
	r.gw.setLive("empty")
	r.clk.Advance(workspaceProbeInterval)
	if got := awaitPush(t, r.gw.putc); got != 2 {
		t.Fatalf("PUT = %d, want the drift push 2", got)
	}
	if got := r.clk.Now().Sub(start); got != workspaceProbeInterval {
		t.Errorf("drift push at %s after the start, want at the probe, %s", got, workspaceProbeInterval)
	}
	if workspaceProbeInterval >= config.DefaultWorkspaceReconcileInterval {
		t.Fatalf("probe interval %s is not before the tick %s", workspaceProbeInterval, config.DefaultWorkspaceReconcileInterval)
	}
	r.clk.Advance(workspaceProbeInterval)
	awaitWaits(t, r.clk, 2)
	if got := r.gw.putCount(); got != 2 {
		t.Errorf("PUTs = %d after a probe that matches the stored tree, want 2", got)
	}
	assertNoDrift(t, r.d)
}

// TestWorkspaceDriftProbeAbsentPushesOnce: a probe answer that refuses because the
// workspace is not present pushes once. The push clears the absent state, so the
// probe after it is quiet.
//
//testpolicy:allow parallel — installs the process-wide workspace pusher the reconcile loop pushes through, which another test must not see mid-run
func TestWorkspaceDriftProbeAbsentPushesOnce(t *testing.T) {
	r := startProbeRun(t)
	r.gw.setAbsent(true)
	r.clk.Advance(workspaceProbeInterval)
	if got := awaitPush(t, r.gw.putc); got != 2 {
		t.Fatalf("PUT = %d, want the absent push 2", got)
	}
	r.clk.Advance(workspaceProbeInterval)
	awaitWaits(t, r.clk, 2)
	if got := r.gw.putCount(); got != 2 {
		t.Errorf("PUTs = %d after the absent state cleared, want 2", got)
	}
	assertNoDrift(t, r.d)
}

// TestWorkspaceDriftProbeMatchingHashIsQuiet: probes that report the stored tree
// record no drift and push nothing, however many there are.
//
//testpolicy:allow parallel — installs the process-wide workspace pusher the reconcile loop pushes through, which another test must not see mid-run
func TestWorkspaceDriftProbeMatchingHashIsQuiet(t *testing.T) {
	r := startProbeRun(t)
	for range 3 {
		r.clk.Advance(workspaceProbeInterval)
		awaitWaits(t, r.clk, 2)
		assertNoDrift(t, r.d)
	}
	if got := r.gw.putCount(); got != 1 {
		t.Errorf("PUTs = %d after probes that match, want only the start push 1", got)
	}
}

// TestWorkspaceDriftProbeUnreachableLogsOnce: a gateway that does not answer its
// probe triggers no push, and is logged once for the failing state, not on every
// probe. Its first answer after that is logged once too.
//
//testpolicy:allow parallel — installs the process-wide workspace pusher the reconcile loop pushes through, which another test must not see mid-run
func TestWorkspaceDriftProbeUnreachableLogsOnce(t *testing.T) {
	r := startProbeRun(t)
	r.gw.setDown(true)
	for range 3 {
		r.clk.Advance(workspaceProbeInterval)
		awaitWaits(t, r.clk, 2)
	}
	if n := strings.Count(r.logs.String(), "did not answer the workspace probe"); n != 1 {
		t.Errorf("unreachable probes logged %d times, want 1 for the failing state", n)
	}
	if got := r.gw.putCount(); got != 1 {
		t.Errorf("PUTs = %d while the gateway is unreachable, want only the start push 1", got)
	}
	assertNoDrift(t, r.d)

	r.gw.setDown(false)
	r.clk.Advance(workspaceProbeInterval)
	awaitWaits(t, r.clk, 2)
	if n := strings.Count(r.logs.String(), "answers the workspace probe again"); n != 1 {
		t.Errorf("recovered probe logged %d times, want 1", n)
	}
	if got := r.gw.putCount(); got != 1 {
		t.Errorf("PUTs = %d after the gateway answers, want only the start push 1", got)
	}
}
