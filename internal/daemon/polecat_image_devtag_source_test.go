package daemon

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// devTagCIHost and devTagCIRepo are the CI registry the source is read from in
// these tests: a second fake, apart from the sandbox registry the copy lands in.
const (
	devTagCIHost = "forgejo.test:3000"
	devTagCIRepo = "bot-registry/" + imagevalues.PolecatImage
)

// devTagRoute sends a request to the CI fake when its host names the CI registry,
// and to the sandbox fake otherwise.
func devTagRoute(ci, sandbox *devTagRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == devTagCIHost {
			ci.serve(w, r)
			return
		}
		sandbox.serve(w, r)
	}
}

// devTagSplitDaemon is devTagDaemon with the source read from the CI fake: the
// setting names devTagCIHost and devTagCIRepo, and the requests route by host.
func devTagSplitDaemon(t *testing.T, ci, sandbox *devTagRegistry) (*Daemon, *devTagNotifier, *bytes.Buffer) {
	t.Helper()
	d, n, logs := devTagDaemon(t, sandbox)
	d.patrolConfig.PolecatImageSourceSetting = devTagCIHost + "/" + devTagCIRepo
	d.polecatDevTagSeams.http = &http.Client{Transport: devTagTransport(devTagRoute(ci, sandbox))}
	return d, n, logs
}

// TestPolecatImageDevTagCopiesFromCIRegistry: the running commit's image is read
// from the CI registry by its build stamp, and its manifest and config blob are
// copied into the sandbox registry, where dev then carries the running commit.
// Nothing is written to the CI registry.
func TestPolecatImageDevTagCopiesFromCIRegistry(t *testing.T) {
	t.Parallel()
	ci := newDevTagRegistryAt(devTagCIRepo)
	source := ci.addImage(devTagRevision, devTagRunningCommit)
	_, config := devTagImageBytes(devTagRunningCommit)
	sandbox := newDevTagRegistry()
	sandbox.addImage("dev", devTagOldCommit)
	d, n, logs := devTagSplitDaemon(t, ci, sandbox)

	d.publishPolecatDevTag()

	if got := sandbox.tagBytes("dev"); !bytes.Equal(got, source) {
		t.Fatalf("sandbox dev tag was not moved to the CI image at the running commit")
	}
	if want := "copied from " + devTagCIHost + "/" + devTagCIRepo + ":" + devTagRevision; !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want the move to name %q", logs.String(), want)
	}
	if !sandbox.hasBlob(devTagDigest(config)) {
		t.Fatal("the config blob of the running commit's image is not in the sandbox registry")
	}
	if sandbox.uploads != 1 {
		t.Fatalf("sandbox uploads = %d, want the one config blob", sandbox.uploads)
	}
	if _, puts := ci.requests(); len(puts) != 0 {
		t.Fatalf("CI registry PUTs = %v, want none", puts)
	}
	gets, _ := ci.requests()
	if !containsString(gets, "/v2/"+devTagCIRepo+"/manifests/"+devTagRevision) {
		t.Fatalf("CI GETs = %v, want the source read by its build stamp", gets)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none", got)
	}
}

// TestPolecatImageDevTagSourceFallsBackToValuesDigest: when the CI registry has no
// tag at the build stamp, the digest deploy/values/images.yaml names is read
// instead, but only when that file pairs the running commit. Any other answer
// keeps the 404 and files one escalation naming the source, the tag and the 404.
func TestPolecatImageDevTagSourceFallsBackToValuesDigest(t *testing.T) {
	t.Parallel()
	manifest, _ := devTagImageBytes(devTagRunningCommit)
	digest := devTagDigest(manifest)
	for _, tc := range []struct {
		name string
		// values is the commit the values file pairs; empty writes no file, and
		// garbage writes a file that does not parse.
		values  string
		garbage bool
		wantGet bool
	}{
		{name: "values file pairs the running commit", values: devTagRunningCommit, wantGet: true},
		{name: "values file pairs another commit", values: devTagOtherCommit},
		{name: "no values file"},
		{name: "values file does not parse", garbage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ci := newDevTagRegistryAt(devTagCIRepo)
			ci.addImage(digest, devTagRunningCommit)
			sandbox := newDevTagRegistry()
			sandbox.addImage("dev", devTagOldCommit)
			before := sandbox.tagBytes("dev")
			d, n, logs := devTagSplitDaemon(t, ci, sandbox)
			writeDevTagValues(t, d, tc.values, tc.garbage, digest)

			d.publishPolecatDevTag()

			gets, _ := ci.requests()
			if !containsString(gets, "/v2/"+devTagCIRepo+"/manifests/"+devTagRevision) {
				t.Fatalf("CI GETs = %v, want the build stamp asked first", gets)
			}
			got := n.got()
			if tc.wantGet {
				if !bytes.Equal(sandbox.tagBytes("dev"), manifest) {
					t.Fatalf("sandbox dev tag was not moved to the digest the values file names")
				}
				// A digest is a reference of its own: the log names it with an at sign,
				// never as a tag after a colon.
				if want := "copied from " + devTagCIHost + "/" + devTagCIRepo + "@" + digest; !strings.Contains(logs.String(), want) {
					t.Fatalf("log = %q, want the move to name the digest reference %q", logs.String(), want)
				}
				if len(got) != 0 {
					t.Fatalf("escalations = %+v, want none", got)
				}
				return
			}
			if !bytes.Equal(sandbox.tagBytes("dev"), before) {
				t.Fatalf("sandbox dev tag moved although the source was not read")
			}
			if len(got) != 1 {
				t.Fatalf("escalations = %+v, want one", got)
			}
			for _, want := range []string{devTagCIHost + "/" + devTagCIRepo, devTagRevision, "HTTP 404"} {
				if !strings.Contains(got[0].Reason, want) {
					t.Fatalf("escalation %q does not name %q", got[0].Reason, want)
				}
			}
		})
	}
}

// TestPolecatImageDevTagEscalatesRefusedBlobCopy: when the sandbox refuses a blob
// the copy sends, the step files one escalation naming the copy and the refusal,
// and neither writes the dev tag nor leaves a manifest PUT behind.
func TestPolecatImageDevTagEscalatesRefusedBlobCopy(t *testing.T) {
	t.Parallel()
	ci := newDevTagRegistryAt(devTagCIRepo)
	ci.addImage(devTagRevision, devTagRunningCommit)
	sandbox := newDevTagRegistry()
	sandbox.addImage("dev", devTagOldCommit)
	sandbox.uploadStatus = http.StatusInternalServerError
	before := sandbox.tagBytes("dev")
	d, n, _ := devTagSplitDaemon(t, ci, sandbox)

	d.publishPolecatDevTag()

	if !bytes.Equal(sandbox.tagBytes("dev"), before) {
		t.Fatalf("sandbox dev tag moved although a blob copy was refused")
	}
	if _, puts := sandbox.requests(); len(puts) != 0 {
		t.Fatalf("sandbox PUTs = %v, want none after the blob copy was refused", puts)
	}
	got := n.got()
	if len(got) != 1 || !strings.Contains(got[0].Reason, "the copy of "+devTagRevision) || !strings.Contains(got[0].Reason, "HTTP 500") {
		t.Fatalf("escalations = %+v, want one naming the refused copy and HTTP 500", got)
	}
}

// TestPolecatImageDevTagRefusesSourceWithoutPath: a source setting that names a
// host and no repository is refused before the registry is asked anything, and
// the sandbox's dev tag is left as it was. The validator refuses such a setting at
// load; the step refuses it too rather than read a repository it cannot name.
func TestPolecatImageDevTagRefusesSourceWithoutPath(t *testing.T) {
	t.Parallel()
	ci := newDevTagRegistryAt(devTagCIRepo)
	ci.addImage(devTagRevision, devTagRunningCommit)
	sandbox := newDevTagRegistry()
	sandbox.addImage("dev", devTagOldCommit)
	before := sandbox.tagBytes("dev")
	d, n, _ := devTagSplitDaemon(t, ci, sandbox)
	d.patrolConfig.PolecatImageSourceSetting = devTagCIHost

	d.publishPolecatDevTag()

	if gets, _ := ci.requests(); len(gets) != 0 {
		t.Fatalf("CI GETs = %v, want none for a source with no repository", gets)
	}
	if !bytes.Equal(sandbox.tagBytes("dev"), before) {
		t.Fatalf("sandbox dev tag moved for a source with no repository")
	}
	got := n.got()
	if len(got) != 1 || !strings.Contains(got[0].Reason, "want a repository path after host:port") {
		t.Fatalf("escalations = %+v, want one naming the missing repository path", got)
	}
}

// writeDevTagValues writes the values file the fallback reads into the town's
// gastown directory: one that pairs values, one that does not parse, or none.
func writeDevTagValues(t *testing.T, d *Daemon, values string, garbage bool, digest string) {
	t.Helper()
	path := imagevalues.PathIn(filepath.Join(d.config.TownRoot, "gastown"))
	switch {
	case garbage:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("daemon: [\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case values != "":
		if err := imagevalues.Write(path, imagevalues.Values{
			Daemon:  devTagCIHost + "/bot-registry/gastown-daemon@" + devTagDigest([]byte("daemon")),
			Polecat: devTagCIHost + "/" + devTagCIRepo + "@" + digest,
			Commit:  values,
		}); err != nil {
			t.Fatal(err)
		}
	}
}
