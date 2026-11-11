package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/buildstamp"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
)

const (
	// devTagRunningCommit is the full commit the running gt was built from; its
	// first buildstamp.Len characters are the build stamp make polecat-image tags
	// the source image with.
	devTagRunningCommit = "0123456789abcdef0123456789abcdef01234567"
	// devTagOldCommit is the commit the dev tag carried before the roll.
	devTagOldCommit = "ffffffffffffffffffffffffffffffffffffffff"
	// devTagOtherCommit is a commit no test expects dev or the source to carry.
	devTagOtherCommit = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// devTagRevision is the build stamp of the running commit, the tag make
// polecat-image gives the source image.
var devTagRevision = devTagRunningCommit[:buildstamp.Len]

// devTagRegistry is an in-memory registry for the polecat image repository. It
// holds each tag's manifest bytes and each blob, serves them the way the
// registry API does, and records every request the step makes.
type devTagRegistry struct {
	mu     sync.Mutex
	tags   map[string][]byte
	blobs  map[string][]byte
	gets   []string
	puts   []string
	status int // when non-zero, every PUT answers it and stores nothing
	// onPut, when set, decides the bytes a PUT stores under its tag.
	onPut func(tag string, body []byte) []byte
}

// devTagHost is the registry's host:port, the form image_registry carries. The
// fake answers in process, so nothing listens on it.
const devTagHost = "registry.test:5001"

// newDevTagRegistry returns an empty fake registry.
func newDevTagRegistry() *devTagRegistry {
	return &devTagRegistry{tags: map[string][]byte{}, blobs: map[string][]byte{}}
}

// devTagTransport serves requests from handler in process: no listener, no
// network.
type devTagTransport http.HandlerFunc

// RoundTrip answers r with the handler's response.
func (h devTagTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	http.HandlerFunc(h).ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// addImage stores an image labeled with commit under tag, with its config blob,
// and returns the manifest bytes it stored.
func (r *devTagRegistry) addImage(tag, commit string) []byte {
	manifest, config := devTagImageBytes(commit)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[devTagDigest(config)] = config
	r.tags[tag] = manifest
	return append([]byte(nil), manifest...)
}

// tagBytes is the manifest bytes a tag holds.
func (r *devTagRegistry) tagBytes(tag string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.tags[tag]...)
}

// requests returns the GETs and PUTs the registry answered, in order.
func (r *devTagRegistry) requests() (gets, puts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.gets...), append([]string(nil), r.puts...)
}

// serve answers the manifest and blob reads and the manifest PUTs the step uses.
func (r *devTagRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	const prefix = "/v2/" + imagevalues.PolecatImage + "/"
	path, ok := strings.CutPrefix(req.URL.Path, prefix)
	if !ok {
		http.NotFound(w, req)
		return
	}
	switch {
	case req.Method == http.MethodGet && strings.HasPrefix(path, "manifests/"):
		r.gets = append(r.gets, req.URL.Path)
		body, found := r.tags[strings.TrimPrefix(path, "manifests/")]
		if !found {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", devTagDigest(body))
		_, _ = w.Write(body)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "blobs/"):
		r.gets = append(r.gets, req.URL.Path)
		body, found := r.blobs[strings.TrimPrefix(path, "blobs/")]
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	case req.Method == http.MethodPut && strings.HasPrefix(path, "manifests/"):
		tag := strings.TrimPrefix(path, "manifests/")
		r.puts = append(r.puts, tag)
		if r.status != 0 {
			w.WriteHeader(r.status)
			return
		}
		body, _ := io.ReadAll(req.Body)
		if r.onPut != nil {
			body = r.onPut(tag, body)
		}
		r.tags[tag] = body
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unexpected request", http.StatusTeapot)
	}
}

// devTagImageBytes builds an image manifest whose config is labeled with commit,
// and returns the manifest bytes and the config blob it names.
func devTagImageBytes(commit string) (manifest, config []byte) {
	config = []byte(`{"config":{"Labels":{"` + imagevalues.CommitLabel + `":"` + commit + `"}}}`)
	manifest, _ = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": devTagDigest(config), "size": len(config)},
		"layers":        []any{},
	})
	return manifest, config
}

// devTagDigest is the sha256 digest of body in the registry's form.
func devTagDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// devTagNotifier records the escalations the step files; the other notifier
// calls are never made by the step and so panic if one is.
type devTagNotifier struct {
	notify.Notifier
	mu          sync.Mutex
	escalations []notify.Escalation
}

// Escalate records the escalation.
func (n *devTagNotifier) Escalate(_ context.Context, e notify.Escalation) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.escalations = append(n.escalations, e)
	return nil
}

// got returns the escalations filed so far.
func (n *devTagNotifier) got() []notify.Escalation {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Escalation(nil), n.escalations...)
}

// devTagDaemon is a daemon whose runner image registry is reg, whose build is
// the stamp of the running commit, and whose escalations and log are captured.
func devTagDaemon(t *testing.T, reg *devTagRegistry) (*Daemon, *devTagNotifier, *bytes.Buffer) {
	t.Helper()
	d, _, _, logs := newTierSweepDaemon(t, time.Now(), "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = &config.TierSweepRunnerConfig{ImageRegistry: devTagHost}
	d.buildCommitFn = func() string { return devTagRevision }
	d.polecatDevTagSeams.runningCommit = func() string { return devTagRunningCommit }
	d.polecatDevTagSeams.http = &http.Client{Transport: devTagTransport(reg.serve)}
	n := &devTagNotifier{}
	d.notifier = n
	return d, n, logs
}

// TestPolecatImageDevTagCopiesRunningCommit: a dev tag behind the running commit
// is moved to the source tag at that commit, the label reads back as the running
// commit, and the step logs one line naming the move.
func TestPolecatImageDevTagCopiesRunningCommit(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	reg.addImage("dev", devTagOldCommit)
	reg.addImage(devTagRevision, devTagRunningCommit)
	source := reg.tagBytes(devTagRevision)
	d, n, logs := devTagDaemon(t, reg)

	d.publishPolecatDevTag()

	if got := reg.tagBytes("dev"); !bytes.Equal(got, source) {
		t.Fatalf("dev tag was not moved to the running commit's image")
	}
	if _, puts := reg.requests(); len(puts) != 1 || puts[0] != "dev" {
		t.Fatalf("PUTs = %v, want one PUT of dev", puts)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none", got)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var moved []string
	for _, line := range lines {
		if strings.Contains(line, "polecat image:") {
			moved = append(moved, line)
		}
	}
	if len(moved) != 1 || !strings.Contains(moved[0], "gt commit "+devTagRunningCommit) || !strings.Contains(moved[0], "label reads back "+devTagRunningCommit) {
		t.Fatalf("log lines naming the move = %q, want one line naming the running commit and its label read back", moved)
	}
}

// TestPolecatImageDevTagCreatesMissingDevTag: a registry with no dev tag yet gets
// one, at the running commit, and nothing is put back because there was nothing
// before it.
func TestPolecatImageDevTagCreatesMissingDevTag(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	source := reg.addImage(devTagRevision, devTagRunningCommit)
	d, n, _ := devTagDaemon(t, reg)

	d.publishPolecatDevTag()

	if got := reg.tagBytes("dev"); !bytes.Equal(got, source) {
		t.Fatalf("dev tag was not created at the running commit's image")
	}
	if _, puts := reg.requests(); len(puts) != 1 {
		t.Fatalf("PUTs = %v, want the one copy", puts)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none", got)
	}
}

// TestPolecatImageDevTagSkipsWhenLabelAgrees: a dev tag that already carries the
// running commit is left alone: no write, no read of the source, no log line,
// no escalation.
func TestPolecatImageDevTagSkipsWhenLabelAgrees(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	reg.addImage("dev", devTagRunningCommit)
	reg.addImage(devTagRevision, devTagRunningCommit)
	before := reg.tagBytes("dev")
	d, n, logs := devTagDaemon(t, reg)

	d.publishPolecatDevTag()

	if _, puts := reg.requests(); len(puts) != 0 {
		t.Fatalf("PUTs = %v, want none when dev already carries the running commit", puts)
	}
	if got := reg.tagBytes("dev"); !bytes.Equal(got, before) {
		t.Fatalf("dev tag changed although its label already agreed")
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("log = %q, want no line beyond the check", logs.String())
	}
}

// TestPolecatImageDevTagSourceIsRunningCommitTag: the source is the tag named by
// the running commit, never dev. A source whose label is another commit is
// refused: nothing is written and dev is left as it was.
func TestPolecatImageDevTagSourceIsRunningCommitTag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		sourceLbl  string
		wantCopied bool
	}{
		{name: "source at the running commit is copied", sourceLbl: devTagRunningCommit, wantCopied: true},
		{name: "source at another commit is refused", sourceLbl: devTagOtherCommit, wantCopied: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newDevTagRegistry()
			reg.addImage("dev", devTagOldCommit)
			reg.addImage(devTagRevision, tc.sourceLbl)
			old := reg.tagBytes("dev")
			d, n, _ := devTagDaemon(t, reg)

			d.publishPolecatDevTag()

			gets, puts := reg.requests()
			if !containsString(gets, "/v2/"+imagevalues.PolecatImage+"/manifests/"+devTagRevision) {
				t.Fatalf("GETs = %v, want the source read by its commit tag %s", gets, devTagRevision)
			}
			if tc.wantCopied {
				if len(puts) != 1 || !bytes.Equal(reg.tagBytes("dev"), reg.tagBytes(devTagRevision)) {
					t.Fatalf("PUTs = %v, want dev copied from the source", puts)
				}
				return
			}
			if len(puts) != 0 || !bytes.Equal(reg.tagBytes("dev"), old) {
				t.Fatalf("PUTs = %v, want no write and dev untouched when the source carries another commit", puts)
			}
			if got := n.got(); len(got) != 1 || !strings.Contains(got[0].Reason, devTagOtherCommit) {
				t.Fatalf("escalations = %+v, want one naming the source's commit", got)
			}
		})
	}
}

// TestPolecatImageDevTagEscalatesFailedCopy: a copy the registry refuses files
// one escalation naming the registry and both commits, and dev keeps the tag it
// had.
func TestPolecatImageDevTagEscalatesFailedCopy(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	reg.addImage("dev", devTagOldCommit)
	reg.addImage(devTagRevision, devTagRunningCommit)
	reg.status = http.StatusInternalServerError
	old := reg.tagBytes("dev")
	d, n, logs := devTagDaemon(t, reg)

	d.publishPolecatDevTag()

	got := n.got()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want exactly one", got)
	}
	for _, want := range []string{devTagHost, devTagRunningCommit, devTagOldCommit} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("escalation reason %q does not name %s", got[0].Reason, want)
		}
	}
	if !bytes.Equal(reg.tagBytes("dev"), old) {
		t.Fatalf("dev tag changed although the copy was refused")
	}
	if strings.Contains(logs.String(), "moved to gt commit") {
		t.Fatalf("log = %q, want no move line for a refused copy", logs.String())
	}
}

// TestPolecatImageDevTagRestoresOnLabelMismatch: when the dev tag does not read
// back as the source after the write, the earlier tag is put back and the step
// escalates once, naming both commits.
func TestPolecatImageDevTagRestoresOnLabelMismatch(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	reg.addImage("dev", devTagOldCommit)
	source := reg.addImage(devTagRevision, devTagRunningCommit)
	old := reg.tagBytes("dev")
	// The registry stores another image than the one sent for the first PUT:
	// the scratch tag holds that image, so its config blob is served too.
	wrong := reg.addImage("scratch", devTagOtherCommit)
	reg.onPut = func(tag string, body []byte) []byte {
		if tag == "dev" && bytes.Equal(body, source) {
			return wrong
		}
		return body
	}
	d, n, _ := devTagDaemon(t, reg)

	d.publishPolecatDevTag()

	got := n.got()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want exactly one", got)
	}
	for _, want := range []string{devTagHost, devTagRunningCommit, devTagOldCommit} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("escalation reason %q does not name %s", got[0].Reason, want)
		}
	}
	if !bytes.Equal(reg.tagBytes("dev"), old) {
		t.Fatalf("dev tag was not put back after the copy did not read back")
	}
	if _, puts := reg.requests(); len(puts) != 2 {
		t.Fatalf("PUTs = %v, want the copy and then the restore", puts)
	}
}

// TestPolecatImageDevTagRefusesUnnamedSource: a build whose revision cannot name
// the running commit's source tag, or whose running commit is not a full commit,
// files one escalation and touches the registry not at all.
func TestPolecatImageDevTagRefusesUnnamedSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		build   string
		running string
	}{
		{name: "no build commit", build: "", running: devTagRunningCommit},
		{name: "unknown build commit", build: "unknown", running: devTagRunningCommit},
		{name: "full-length build commit", build: devTagRunningCommit, running: devTagRunningCommit},
		{name: "build commit not the running commit", build: "fedcba987654", running: devTagRunningCommit},
		{name: "running commit not full", build: devTagRevision, running: devTagRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newDevTagRegistry()
			reg.addImage("dev", devTagOldCommit)
			reg.addImage(devTagRevision, devTagRunningCommit)
			d, n, _ := devTagDaemon(t, reg)
			d.buildCommitFn = func() string { return tc.build }
			d.polecatDevTagSeams.runningCommit = func() string { return tc.running }

			d.publishPolecatDevTag()

			if gets, puts := reg.requests(); len(gets)+len(puts) != 0 {
				t.Fatalf("registry requests gets=%v puts=%v, want none", gets, puts)
			}
			if got := n.got(); len(got) != 1 {
				t.Fatalf("escalations = %+v, want exactly one", got)
			}
		})
	}
}

// TestPolecatImageDevTagOffWithoutRegistry: a daemon with no image_registry has
// no registry to move, so the step makes no request and files nothing.
func TestPolecatImageDevTagOffWithoutRegistry(t *testing.T) {
	t.Parallel()
	reg := newDevTagRegistry()
	reg.addImage("dev", devTagOldCommit)
	d, n, _ := devTagDaemon(t, reg)
	d.patrolConfig.Patrols.TierSweep.Runner.ImageRegistry = ""

	d.publishPolecatDevTag()

	if gets, puts := reg.requests(); len(gets)+len(puts) != 0 {
		t.Fatalf("registry requests gets=%v puts=%v, want none without image_registry", gets, puts)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none", got)
	}
}

// containsString reports whether want is one of values.
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
