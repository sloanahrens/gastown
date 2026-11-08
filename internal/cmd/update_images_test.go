package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// testUpdateImagesSHA is the green commit both images carry; its first
	// twelve characters are the tag the fake registry serves.
	testUpdateImagesSHA = "0123456789abcdef0123456789abcdef01234567"
	// testUpdateImagesOtherSHA is a commit neither image may carry.
	testUpdateImagesOtherSHA = "fedcba9876543210fedcba9876543210fedcba98"
	// testUpdateImagesTag is testUpdateImagesSHA's tag.
	testUpdateImagesTag = "0123456789ab"
	// testUpdateImagesUser and testUpdateImagesToken are the viewer credential
	// every test wires; the token must never appear in the command's output.
	testUpdateImagesUser  = "bot-viewer"
	testUpdateImagesToken = "viewer-token-2f9a4c"
)

var (
	testUpdateImagesDaemonDigest  = "sha256:" + strings.Repeat("1a", 32)
	testUpdateImagesPolecatDigest = "sha256:" + strings.Repeat("2b", 32)
	testUpdateImagesConfigDigest  = "sha256:" + strings.Repeat("3c", 32)
	testUpdateImagesRebuiltDigest = "sha256:" + strings.Repeat("4d", 32)
)

// fakeRegistryImage is one image the fake registry serves. A zero Digest with
// OmitDigest false still serves an empty header, which is a refusal.
type fakeRegistryImage struct {
	Digest       string
	OmitDigest   bool
	ConfigDigest string
	Commit       string
	ManifestBody string
	Status       int
	BlobStatus   int
	MissingTag   bool
}

// matchingImage is an image with digest and commit label set.
func matchingImage(digest, commit string) *fakeRegistryImage {
	return &fakeRegistryImage{Digest: digest, Commit: commit}
}

// fakeRegistry serves the registry's v2 manifest and blob endpoints from a map
// of image name to answer, and records the credentials each request carried.
type fakeRegistry struct {
	mu     sync.Mutex
	images map[string]*fakeRegistryImage
	auth   []fakeCredential
}

// fakeCredential is one request's basic-auth pair.
type fakeCredential struct{ user, pass string }

// set changes one served image between runs.
func (f *fakeRegistry) set(name string, mutate func(*fakeRegistryImage)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img := f.images[name]; img != nil {
		mutate(img)
	}
}

// credentials is every basic-auth pair the registry saw.
func (f *fakeRegistry) credentials() []fakeCredential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCredential(nil), f.auth...)
}

// serveHTTP answers the registry's two endpoints for the images it holds.
func (f *fakeRegistry) serveHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	f.mu.Lock()
	f.auth = append(f.auth, fakeCredential{user: user, pass: pass})
	name, endpoint, ref, ok := splitRegistryPath(r.URL.Path)
	var img *fakeRegistryImage
	if ok {
		if held := f.images[name]; held != nil {
			copied := *held
			img = &copied
		}
	}
	f.mu.Unlock()
	if !ok || img == nil {
		http.NotFound(w, r)
		return
	}
	switch endpoint {
	case "manifests":
		if ref != testUpdateImagesTag || img.MissingTag {
			http.NotFound(w, r)
			return
		}
		if img.Status != 0 {
			w.WriteHeader(img.Status)
			return
		}
		if !img.OmitDigest {
			w.Header().Set("Docker-Content-Digest", img.Digest)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		writeFakeManifest(w, img)
	case "blobs":
		if img.BlobStatus != 0 {
			w.WriteHeader(img.BlobStatus)
			return
		}
		body, err := json.Marshal(map[string]any{
			"architecture": "arm64",
			"config": map[string]any{
				"Labels": map[string]string{imagevalues.CommitLabel: img.Commit},
			},
		})
		if err != nil {
			http.Error(w, "building the fake config", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

// splitRegistryPath reads /v2/<owner>/<name>/<endpoint>/<ref> from path.
func splitRegistryPath(path string) (name, endpoint, ref string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] != imagevalues.RegistryOwner {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

// writeFakeManifest writes the manifest body: the image's override, or a
// single image manifest naming its config blob.
func writeFakeManifest(w http.ResponseWriter, img *fakeRegistryImage) {
	if img.ManifestBody != "" {
		_, _ = w.Write([]byte(img.ManifestBody))
		return
	}
	digest := img.ConfigDigest
	if digest == "" {
		digest = testUpdateImagesConfigDigest
	}
	body, err := json.Marshal(map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config":    map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest},
	})
	if err != nil {
		http.Error(w, "building the fake manifest", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// testUpdateImagesSeams wires the command to server and counts the values
// writes, so a no-op run is visible as an unchanged count.
func testUpdateImagesSeams(server *httptest.Server, writes *int) updateImagesSeams {
	return updateImagesSeams{
		RegistryBase: server.URL,
		Credentials:  func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
		HTTPClient:   server.Client(),
		WriteValues: func(path string, v imagevalues.Values) error {
			*writes++
			return imagevalues.Write(path, v)
		},
	}
}

// startFakeRegistry serves images until the test ends.
func startFakeRegistry(t *testing.T, images map[string]*fakeRegistryImage) (*fakeRegistry, *httptest.Server) {
	t.Helper()
	registry := &fakeRegistry{images: images}
	//testpolicy:allow no-network — the bead requires a fake registry on an httptest server: it answers in-process on loopback, and the command's only client is the one this test wires to it
	server := httptest.NewServer(http.HandlerFunc(registry.serveHTTP))
	t.Cleanup(server.Close)
	return registry, server
}

// runUpdateImagesCommand runs a fresh gt update images over seams.
func runUpdateImagesCommand(t *testing.T, seams updateImagesSeams, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := newUpdateImagesCmd(seams)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	err = c.Execute()
	return out.String(), errOut.String(), err
}

// TestUpdateImagesWritesTheValuesForAMatchingPair holds the happy path: two
// images whose commit label is --sha give a values file naming both digests
// and the commit, written once.
func TestUpdateImagesWritesTheValuesForAMatchingPair(t *testing.T) {
	t.Parallel()
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})
	writes := 0
	out := filepath.Join(t.TempDir(), "deploy", "values", "images.yaml")

	stdout, stderr, err := runUpdateImagesCommand(t, testUpdateImagesSeams(server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
	if err != nil {
		t.Fatalf("gt update images: %v (stderr %q)", err, stderr)
	}
	want := imagevalues.Values{
		Daemon:  imagevalues.Ref(imagevalues.DaemonImage, testUpdateImagesDaemonDigest),
		Polecat: imagevalues.Ref(imagevalues.PolecatImage, testUpdateImagesPolecatDigest),
		Commit:  testUpdateImagesSHA,
	}
	got, err := imagevalues.Load(out)
	if err != nil {
		t.Fatalf("reading the values file: %v", err)
	}
	if got != want {
		t.Errorf("the values file = %+v, want %+v", got, want)
	}
	if writes != 1 {
		t.Errorf("the values file was written %d times, want once", writes)
	}
	if !strings.Contains(stdout, out) {
		t.Errorf("the command's output %q does not name %s", stdout, out)
	}
}

// TestUpdateImagesRefusesAPairThatDoesNotBelongTogether holds every refusal
// the registry read can make: the error names the image and no values file is
// written.
func TestUpdateImagesRefusesAPairThatDoesNotBelongTogether(t *testing.T) {
	t.Parallel()
	indexBody := `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + "sha256:" + strings.Repeat("5e", 32) + `"}]}`
	rows := []struct {
		name    string
		daemon  *fakeRegistryImage
		polecat *fakeRegistryImage
		want    []string
		notWant string
	}{
		{
			name:    "a tag the registry does not have",
			daemon:  &fakeRegistryImage{MissingTag: true},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "no tag " + testUpdateImagesTag},
		},
		{
			name:    "a tag the polecat image does not have",
			daemon:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
			polecat: &fakeRegistryImage{MissingTag: true},
			want:    []string{"gastown-polecat", "no tag " + testUpdateImagesTag},
		},
		{
			name:    "a config blob the registry refuses",
			daemon:  &fakeRegistryImage{Digest: testUpdateImagesDaemonDigest, Commit: testUpdateImagesSHA, BlobStatus: http.StatusInternalServerError},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon config", "HTTP 500"},
		},
		{
			name:    "labels that differ from each other",
			daemon:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesOtherSHA),
			want:    []string{"gastown-daemon", "gastown-polecat", testUpdateImagesOtherSHA},
		},
		{
			name:    "a label that is not the commit",
			daemon:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesOtherSHA),
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesOtherSHA),
			want:    []string{"gastown-daemon", testUpdateImagesOtherSHA},
		},
		{
			name:    "a malformed digest",
			daemon:  &fakeRegistryImage{Digest: "sha256:not-hex", Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "not sha256"},
		},
		{
			name:    "a registry error",
			daemon:  &fakeRegistryImage{Status: http.StatusInternalServerError, Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "HTTP 500"},
			notWant: "no tag",
		},
		{
			name:    "an answer that does not parse",
			daemon:  &fakeRegistryImage{ManifestBody: "{not json", Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "does not parse"},
		},
		{
			name:    "an index rather than an image",
			daemon:  &fakeRegistryImage{Digest: testUpdateImagesDaemonDigest, ManifestBody: indexBody, Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "is an index"},
		},
		{
			name:    "no digest header",
			daemon:  &fakeRegistryImage{OmitDigest: true, Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "not sha256"},
		},
		{
			name:    "a manifest with no config digest",
			daemon:  &fakeRegistryImage{Digest: testUpdateImagesDaemonDigest, ManifestBody: `{"mediaType":"application/vnd.oci.image.manifest.v1+json"}`, Commit: testUpdateImagesSHA},
			polecat: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "config digest"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
				imagevalues.DaemonImage:  row.daemon,
				imagevalues.PolecatImage: row.polecat,
			})
			writes := 0
			out := filepath.Join(t.TempDir(), "images.yaml")

			_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
			if err == nil {
				t.Fatalf("gt update images succeeded, want a refusal")
			}
			for _, want := range row.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error %q does not name %q", err, want)
				}
			}
			if row.notWant != "" && strings.Contains(err.Error(), row.notWant) {
				t.Errorf("the error %q reads a registry error as %q", err, row.notWant)
			}
			if _, statErr := os.Stat(out); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a refused run left %s behind (%v)", out, statErr)
			}
			if writes != 0 {
				t.Errorf("a refused run wrote the values file %d times", writes)
			}
		})
	}

	t.Run("a commit that is not a full sha", func(t *testing.T) {
		t.Parallel()
		registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
			imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
			imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
		})
		writes := 0
		out := filepath.Join(t.TempDir(), "images.yaml")

		_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(server, &writes), "--sha", testUpdateImagesTag, "--out", out)
		if err == nil || !strings.Contains(err.Error(), "--sha") {
			t.Fatalf("gt update images with a short sha = %v, want an error naming --sha", err)
		}
		if _, statErr := os.Stat(out); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a refused run left %s behind (%v)", out, statErr)
		}
		if got := len(registry.credentials()); got != 0 {
			t.Errorf("a refused sha sent %d registry requests, want none", got)
		}
	})
}

// TestUpdateImagesIsIdempotentAndNeverReplaces holds the write rules: the same
// digests twice write once, and a digest that changed under a recorded commit
// is refused without replacing the file.
func TestUpdateImagesIsIdempotentAndNeverReplaces(t *testing.T) {
	t.Parallel()
	registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(server, &writes)
	out := filepath.Join(t.TempDir(), "images.yaml")

	if _, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", out); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, err := imagevalues.Load(out)
	if err != nil {
		t.Fatalf("reading the values file: %v", err)
	}

	stdout, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", out)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if writes != 1 {
		t.Errorf("the same pair was written %d times, want once", writes)
	}
	if again, err := imagevalues.Load(out); err != nil || again != first {
		t.Errorf("the second run changed the file: %+v (err %v)", again, err)
	}
	if !strings.Contains(stdout, "nothing changed") {
		t.Errorf("the second run's output %q does not report a no-op", stdout)
	}

	registry.set(imagevalues.DaemonImage, func(img *fakeRegistryImage) {
		img.Digest = testUpdateImagesRebuiltDigest
	})
	_, _, err = runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", out)
	if err == nil {
		t.Fatal("a rebuilt image under a recorded tag succeeded, want a refusal")
	}
	for _, want := range []string{"finding", testUpdateImagesSHA, testUpdateImagesRebuiltDigest} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
	if writes != 1 {
		t.Errorf("the refused run wrote the values file %d times, want no write", writes)
	}
	if kept, err := imagevalues.Load(out); err != nil || kept != first {
		t.Errorf("the refused run replaced the pair: %+v (err %v)", kept, err)
	}
}

// TestUpdateImagesNeverPrintsTheCredential holds the secret rule: the request
// carries the viewer token, and no run's stdout, stderr or error does.
func TestUpdateImagesNeverPrintsTheCredential(t *testing.T) {
	t.Parallel()
	registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(server, &writes)

	stdout, stderr, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "ok", "images.yaml"))
	if err != nil {
		t.Fatalf("gt update images: %v", err)
	}

	registry.set(imagevalues.DaemonImage, func(img *fakeRegistryImage) { img.Status = http.StatusInternalServerError })
	failOut, failErr, failRunErr := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "fail", "images.yaml"))
	if failRunErr == nil {
		t.Fatal("a failing registry succeeded, want a refusal")
	}

	printed := stdout + stderr + failOut + failErr + failRunErr.Error()
	if strings.Contains(printed, testUpdateImagesToken) {
		t.Fatalf("the viewer token appears in the command's output")
	}

	// The token was sent, so the assertion above is not vacuous.
	credentials := registry.credentials()
	if len(credentials) == 0 {
		t.Fatal("the registry saw no request")
	}
	for _, got := range credentials {
		if got.user != testUpdateImagesUser || got.pass != testUpdateImagesToken {
			t.Errorf("a registry request carried another credential")
		}
	}
}

// errReader fails every read, so a response body that breaks mid-read is
// driven without a network.
type errReader struct{}

// Read always fails.
func (errReader) Read([]byte) (int, error) { return 0, errors.New("the connection broke") }

// TestUpdateImagesRefusesACommandWithNoSeams holds the wiring rule: a command
// built without its reader or writer refuses instead of reporting success.
func TestUpdateImagesRefusesACommandWithNoSeams(t *testing.T) {
	t.Parallel()
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})
	writes := 0
	valid := testUpdateImagesSeams(server, &writes)
	rows := []struct {
		name   string
		mutate func(*updateImagesSeams)
		want   string
	}{
		{"no registry", func(s *updateImagesSeams) { s.RegistryBase = "" }, "no registry is wired"},
		{"no credential reader", func(s *updateImagesSeams) { s.Credentials = nil }, "no credential reader is wired"},
		{"no HTTP client", func(s *updateImagesSeams) { s.HTTPClient = nil }, "no HTTP client is wired"},
		{"no values writer", func(s *updateImagesSeams) { s.WriteValues = nil }, "no values writer is wired"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			seams := valid
			row.mutate(&seams)
			_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("gt update images = %v, want a refusal naming %q", err, row.want)
			}
		})
	}

	t.Run("no values file path", func(t *testing.T) {
		t.Parallel()
		_, _, err := runUpdateImagesCommand(t, valid, "--sha", testUpdateImagesSHA, "--out", "")
		if err == nil || !strings.Contains(err.Error(), "--out") {
			t.Fatalf("gt update images with no output path = %v, want a refusal naming --out", err)
		}
	})
}

// TestUpdateImagesRefusesAnUnreadableCredential holds the credential rule: a
// viewer credential that cannot be read refuses before any registry request.
func TestUpdateImagesRefusesAnUnreadableCredential(t *testing.T) {
	t.Parallel()
	registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(server, &writes)
	seams.Credentials = func() (string, string, error) {
		return "", "", errors.New("the viewer token file is missing")
	}
	_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
	if err == nil || !strings.Contains(err.Error(), "viewer credential") {
		t.Fatalf("gt update images = %v, want a credential refusal", err)
	}
	if got := len(registry.credentials()); got != 0 {
		t.Errorf("an unreadable credential sent %d registry requests, want none", got)
	}
}

// TestViewerCredentialReaderDrivesEveryFailure holds both refusals of the
// production credential reader: a token path that cannot be resolved, and a
// file that cannot be read.
func TestViewerCredentialReaderDrivesEveryFailure(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name      string
		tokenPath func(string) (string, error)
		want      string
	}{
		{
			name:      "a config home that cannot be resolved",
			tokenPath: func(string) (string, error) { return "", errors.New("no config home") },
			want:      "no config home",
		},
		{
			name:      "a token file that cannot be read",
			tokenPath: func(string) (string, error) { return filepath.Join(t.TempDir(), "absent.env"), nil },
			want:      "token file",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := viewerCredentialReader(row.tokenPath)(); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("viewerCredentialReader = %v, want a refusal naming %q", err, row.want)
			}
		})
	}
}

// TestUpdateImagesRefusesAFileOrWriterItCannotUse holds the local refusals: an
// existing values file that does not parse, and a writer that fails.
func TestUpdateImagesRefusesAFileOrWriterItCannotUse(t *testing.T) {
	t.Parallel()
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingImage(testUpdateImagesDaemonDigest, testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingImage(testUpdateImagesPolecatDigest, testUpdateImagesSHA),
	})

	t.Run("an existing values file that does not parse", func(t *testing.T) {
		t.Parallel()
		writes := 0
		out := filepath.Join(t.TempDir(), "images.yaml")
		if err := os.WriteFile(out, []byte("daemon: ["), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
		if err == nil || !strings.Contains(err.Error(), "does not parse") {
			t.Fatalf("gt update images = %v, want the malformed file named", err)
		}
		if writes != 0 {
			t.Errorf("a malformed file was overwritten %d times, want no write", writes)
		}
	})

	t.Run("a values writer that fails", func(t *testing.T) {
		t.Parallel()
		writes := 0
		seams := testUpdateImagesSeams(server, &writes)
		seams.WriteValues = func(string, imagevalues.Values) error { return errors.New("the disk is full") }
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil || !strings.Contains(err.Error(), "the disk is full") {
			t.Fatalf("gt update images = %v, want the writer's error", err)
		}
	})
}

// TestUpdateImagesDrivesEveryRegistryFailure holds the branches a broken
// registry address or answer reaches: the request cannot be built, the server
// cannot be reached, the body breaks, or the body is too large.
func TestUpdateImagesDrivesEveryRegistryFailure(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-network — the server is closed before use: it exists only to give a loopback address nothing listens on, and the branch under test is the failed dial
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable.Close()

	rows := []struct {
		name string
		base string
		http *http.Client
		want string
	}{
		{
			name: "an address that is not a URL",
			base: "http://registry.test/%zz",
			http: http.DefaultClient,
			want: "invalid URL escape",
		},
		{
			name: "a server that cannot be reached",
			base: unreachable.URL,
			http: unreachable.Client(),
			want: "connect",
		},
		{
			name: "a body that breaks mid-read",
			base: "http://registry.test",
			http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(errReader{})}, nil
			})},
			want: "the connection broke",
		},
		{
			name: "a body larger than the bound",
			base: "http://registry.test",
			http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(bytes.NewReader(make([]byte, updateImagesMaxManifest+1))),
				}, nil
			})},
			want: "larger than",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			writes := 0
			seams := updateImagesSeams{
				RegistryBase: row.base,
				Credentials:  func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
				HTTPClient:   row.http,
				WriteValues:  func(path string, v imagevalues.Values) error { writes++; return imagevalues.Write(path, v) },
			}
			_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("gt update images = %v, want a refusal naming %q", err, row.want)
			}
			if writes != 0 {
				t.Errorf("a failed registry read wrote the values file %d times", writes)
			}
		})
	}
}
