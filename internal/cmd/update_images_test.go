package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

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

// testUpdateImagesOwner is the account the fixture registry owns its images
// under.
const testUpdateImagesOwner = "town"

// testRegistryFor is the registry a test's server answers for: the server's own
// scheme and host, so the command builds its base URL from the registry the way
// it does in production.
func testRegistryFor(t *testing.T, server *httptest.Server) imagevalues.Registry {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing %s: %v", server.URL, err)
	}
	return imagevalues.Registry{Scheme: u.Scheme, Host: u.Host, Owner: testUpdateImagesOwner}
}

// testUpdateImagesRebuiltDigest is a well-formed digest the fixture bytes do
// not hash to.
var testUpdateImagesRebuiltDigest = "sha256:" + strings.Repeat("4d", 32)

// fakeRegistryImage is one image the fake registry serves. A zero Digest
// reports the digest of the bytes served; a set one reports that instead, so a
// manifest whose bytes do not hash to it is driven. The same holds for
// ConfigDigest, and Marker changes the manifest bytes without changing what
// they name, the way a rebuild under one tag answers another digest.
type fakeRegistryImage struct {
	Digest       string
	OmitDigest   bool
	ConfigDigest string
	Commit       string
	Marker       string
	ManifestBody string
	Status       int
	BlobStatus   int
	MissingTag   bool
	// Children, when set, makes the tag an index over these platform images.
	Children []fakeIndexChild
}

// fakeIndexChild is one platform entry an index names. A zero Digest names the
// digest of the child's bytes; a set one names that instead, so the bytes served
// for it do not hash to what the index names.
type fakeIndexChild struct {
	OS     string
	Arch   string
	Digest string
	Image  *fakeRegistryImage
}

// named is the digest the index names for this entry.
func (c fakeIndexChild) named() string {
	if c.Digest != "" {
		return c.Digest
	}
	return testDigestOf(fakeManifestBytes(c.Image))
}

// childNamed is the child an index names at digest, or nil.
func (img *fakeRegistryImage) childNamed(digest string) *fakeRegistryImage {
	for _, c := range img.Children {
		if c.named() == digest {
			return c.Image
		}
	}
	return nil
}

// childWithConfig is the child whose config blob hashes to digest, or nil.
func (img *fakeRegistryImage) childWithConfig(digest string) *fakeRegistryImage {
	for _, c := range img.Children {
		if fakeConfigDigest(c.Image) == digest {
			return c.Image
		}
	}
	return nil
}

// fakeIndexBytes is the index body that names children, one entry each.
func fakeIndexBytes(children []fakeIndexChild) []byte {
	entries := make([]string, 0, len(children))
	for _, c := range children {
		entries = append(entries, `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"`+c.named()+`","platform":{"os":"`+c.OS+`","architecture":"`+c.Arch+`"}}`)
	}
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(entries, ",") + `]}`)
}

// linuxChild is an index entry for linux/arm64 whose bytes are an image at commit.
func linuxChild(commit string) fakeIndexChild {
	return fakeIndexChild{OS: "linux", Arch: "arm64", Image: matchingContents(commit)}
}

// matchingImage is an image with digest and commit label set.
func matchingImage(digest, commit string) *fakeRegistryImage {
	return &fakeRegistryImage{Digest: digest, Commit: commit}
}

// matchingContents is an image whose digest is the hash of the bytes it
// serves: the image the command records without a rebuild finding.
func matchingContents(commit string) *fakeRegistryImage {
	return &fakeRegistryImage{Commit: commit}
}

// fakeConfigBytes is the config blob an image at commit serves: the label the
// command reads is the only part of it under test.
func fakeConfigBytes(commit string) []byte {
	return []byte(`{"architecture":"arm64","config":{"Labels":{"` + imagevalues.CommitLabel + `":"` + commit + `"}}}`)
}

// fakeConfigDigest is the config digest the manifest names.
func fakeConfigDigest(img *fakeRegistryImage) string {
	if img.ConfigDigest != "" {
		return img.ConfigDigest
	}
	return testDigestOf(fakeConfigBytes(img.Commit))
}

// fakeManifestBytes is the manifest the registry serves for img.
func fakeManifestBytes(img *fakeRegistryImage) []byte {
	if img.ManifestBody != "" {
		return []byte(img.ManifestBody)
	}
	if len(img.Children) > 0 {
		return fakeIndexBytes(img.Children)
	}
	annotation := ""
	if img.Marker != "" {
		annotation = `,"annotations":{"org.example.rebuild":"` + img.Marker + `"}`
	}
	return []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + fakeConfigDigest(img) + `"}` + annotation + `}`)
}

// testDigestOf is body's sha256 in the form a registry reports.
func testDigestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeRegistry serves the registry's v2 manifest and blob endpoints from a map
// of image name to answer, records the credentials each request carried, and
// remembers the manifest digest it reported for each image.
type fakeRegistry struct {
	mu      sync.Mutex
	images  map[string]*fakeRegistryImage
	auth    []fakeCredential
	digests map[string]string
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

// reported is the manifest digest the registry last reported for name.
func (f *fakeRegistry) reported(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digests[name]
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
		if child := img.childNamed(ref); child != nil {
			if child.Status != 0 {
				w.WriteHeader(child.Status)
				return
			}
			body := fakeManifestBytes(child)
			w.Header().Set("Docker-Content-Digest", testDigestOf(body))
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(body)
			return
		}
		if ref != testUpdateImagesTag || img.MissingTag {
			http.NotFound(w, r)
			return
		}
		if img.Status != 0 {
			w.WriteHeader(img.Status)
			return
		}
		body := fakeManifestBytes(img)
		digest := img.Digest
		if digest == "" {
			digest = testDigestOf(body)
		}
		f.mu.Lock()
		f.digests[name] = digest
		f.mu.Unlock()
		if !img.OmitDigest {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(body)
	case "blobs":
		if img.BlobStatus != 0 {
			w.WriteHeader(img.BlobStatus)
			return
		}
		commit := img.Commit
		if child := img.childWithConfig(ref); child != nil {
			commit = child.Commit
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(fakeConfigBytes(commit))
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
	if len(parts) != 4 || parts[0] != testUpdateImagesOwner {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

// testUpdateImagesSeams wires the command to server and counts the values
// writes, so a no-op run is visible as an unchanged count.
func testUpdateImagesSeams(t *testing.T, server *httptest.Server, writes *int) updateImagesSeams {
	t.Helper()
	registry := testRegistryFor(t, server)
	return updateImagesSeams{
		FindTownRoot: func() (string, error) { return "/town", nil },
		Registry:     func(string) (imagevalues.Registry, error) { return registry, nil },
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
	registry := &fakeRegistry{images: images, digests: map[string]string{}}
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
	registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	out := filepath.Join(t.TempDir(), "deploy", "values", "images.yaml")

	stdout, stderr, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
	if err != nil {
		t.Fatalf("gt update images: %v (stderr %q)", err, stderr)
	}
	address := testRegistryFor(t, server)
	want := imagevalues.Values{
		Daemon:  address.Ref(imagevalues.DaemonImage, registry.reported(imagevalues.DaemonImage)),
		Polecat: address.Ref(imagevalues.PolecatImage, registry.reported(imagevalues.PolecatImage)),
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

// TestUpdateImagesResolvesAnIndexToItsLinuxChild holds the shape the CI
// publishes: each tag is an index over a linux/amd64 and a linux/arm64 child.
// The values file records each index's own digest, and the commit comes from
// the linux/arm64 child's config, not the amd64 child's.
func TestUpdateImagesResolvesAnIndexToItsLinuxChild(t *testing.T) {
	t.Parallel()
	amd64 := fakeIndexChild{OS: "linux", Arch: "amd64", Image: matchingContents(testUpdateImagesOtherSHA)}
	daemon := &fakeRegistryImage{Children: []fakeIndexChild{amd64, linuxChild(testUpdateImagesSHA)}}
	polecat := &fakeRegistryImage{Children: []fakeIndexChild{amd64, linuxChild(testUpdateImagesSHA)}}
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  daemon,
		imagevalues.PolecatImage: polecat,
	})
	writes := 0
	out := filepath.Join(t.TempDir(), "deploy", "values", "images.yaml")

	_, stderr, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
	if err != nil {
		t.Fatalf("gt update images: %v (stderr %q)", err, stderr)
	}
	address := testRegistryFor(t, server)
	want := imagevalues.Values{
		Daemon:  address.Ref(imagevalues.DaemonImage, testDigestOf(fakeManifestBytes(daemon))),
		Polecat: address.Ref(imagevalues.PolecatImage, testDigestOf(fakeManifestBytes(polecat))),
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
}

// TestUpdateImagesRefusesAPairThatDoesNotBelongTogether holds every refusal
// the registry read can make: the error names the image and no values file is
// written.
func TestUpdateImagesRefusesAPairThatDoesNotBelongTogether(t *testing.T) {
	t.Parallel()
	indexBody := `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + "sha256:" + strings.Repeat("5e", 32) + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	amd64Child := fakeIndexChild{OS: "linux", Arch: "amd64", Image: matchingContents(testUpdateImagesSHA)}
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
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "no tag " + testUpdateImagesTag},
		},
		{
			name:    "a tag the polecat image does not have",
			daemon:  matchingContents(testUpdateImagesSHA),
			polecat: &fakeRegistryImage{MissingTag: true},
			want:    []string{"gastown-polecat", "no tag " + testUpdateImagesTag},
		},
		{
			name:    "a config blob the registry refuses",
			daemon:  &fakeRegistryImage{Commit: testUpdateImagesSHA, BlobStatus: http.StatusInternalServerError},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon config", "HTTP 500"},
		},
		{
			name:    "labels that differ from each other",
			daemon:  matchingContents(testUpdateImagesSHA),
			polecat: matchingContents(testUpdateImagesOtherSHA),
			want:    []string{"gastown-daemon", "gastown-polecat", testUpdateImagesOtherSHA},
		},
		{
			name:    "a label that is not the commit",
			daemon:  matchingContents(testUpdateImagesOtherSHA),
			polecat: matchingContents(testUpdateImagesOtherSHA),
			want:    []string{"gastown-daemon", testUpdateImagesOtherSHA},
		},
		{
			name:    "a malformed digest",
			daemon:  &fakeRegistryImage{Digest: "sha256:not-hex", Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "not sha256"},
		},
		{
			name:    "a registry error",
			daemon:  &fakeRegistryImage{Status: http.StatusInternalServerError, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "HTTP 500"},
			notWant: "no tag",
		},
		{
			name:    "an answer that does not parse",
			daemon:  &fakeRegistryImage{ManifestBody: "{not json", Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "does not parse"},
		},
		{
			name:    "an index with no linux/arm64 child",
			daemon:  &fakeRegistryImage{ManifestBody: indexBody, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", testUpdateImagesTag, "linux/arm64", "linux/amd64"},
		},
		{
			name:    "an index with no linux/arm64 child among others",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{amd64Child}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", testUpdateImagesTag, "linux/arm64", "linux/amd64"},
		},
		{
			name:    "an index with two linux/arm64 children",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{linuxChild(testUpdateImagesSHA), linuxChild(testUpdateImagesSHA)}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", testUpdateImagesTag, "2 linux/arm64 entries", "linux/arm64"},
		},
		{
			name:    "an index child whose bytes do not hash to the digest the index names",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{{OS: "linux", Arch: "arm64", Digest: testUpdateImagesRebuiltDigest, Image: matchingContents(testUpdateImagesSHA)}}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "manifest bytes that hash to"},
		},
		{
			name:    "an index child the registry does not serve",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{{OS: "linux", Arch: "arm64", Image: &fakeRegistryImage{Commit: testUpdateImagesSHA, Status: http.StatusNotFound}}}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "has no child manifest"},
		},
		{
			name:    "an index child the registry refuses",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{{OS: "linux", Arch: "arm64", Image: &fakeRegistryImage{Commit: testUpdateImagesSHA, Status: http.StatusInternalServerError}}}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "child", "HTTP 500"},
		},
		{
			name:    "an index whose child is itself an index",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{{OS: "linux", Arch: "arm64", Image: &fakeRegistryImage{Children: []fakeIndexChild{linuxChild(testUpdateImagesSHA)}}}}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "is an index, not an image"},
		},
		{
			name:    "an index whose linux/arm64 child carries another commit",
			daemon:  &fakeRegistryImage{Children: []fakeIndexChild{linuxChild(testUpdateImagesOtherSHA)}},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", testUpdateImagesOtherSHA},
		},
		{
			name:    "no digest header",
			daemon:  &fakeRegistryImage{OmitDigest: true, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "not sha256"},
		},
		{
			name:    "a manifest with no config digest",
			daemon:  &fakeRegistryImage{ManifestBody: `{"mediaType":"application/vnd.oci.image.manifest.v1+json"}`, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "config digest"},
		},
		{
			name:    "manifest bytes that do not hash to the reported digest",
			daemon:  &fakeRegistryImage{Digest: testUpdateImagesRebuiltDigest, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "manifest bytes hash to"},
		},
		{
			name:    "a config blob that does not hash to the manifest's config digest",
			daemon:  &fakeRegistryImage{ConfigDigest: testUpdateImagesRebuiltDigest, Commit: testUpdateImagesSHA},
			polecat: matchingContents(testUpdateImagesSHA),
			want:    []string{"gastown-daemon", "config bytes hash to"},
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

			_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
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
			imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
			imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
		})
		writes := 0
		out := filepath.Join(t.TempDir(), "images.yaml")

		_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesTag, "--out", out)
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
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(t, server, &writes)
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

	registry.set(imagevalues.DaemonImage, func(img *fakeRegistryImage) { img.Marker = "rebuilt" })
	_, _, err = runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", out)
	if err == nil {
		t.Fatal("a rebuilt image under a recorded tag succeeded, want a refusal")
	}
	for _, want := range []string{"finding", testUpdateImagesSHA, registry.reported(imagevalues.DaemonImage)} {
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
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(t, server, &writes)

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

// TestUpdateImagesCredentialTransport holds the transport rule: the viewer
// credential goes over https to any registry, over http only to a loopback
// address or the town's own Forgejo, and it never follows a redirect that
// changes the scheme or the host.
func TestUpdateImagesCredentialTransport(t *testing.T) {
	t.Parallel()

	t.Run("https sends the credential", func(t *testing.T) {
		t.Parallel()
		registry := &fakeRegistry{
			images: map[string]*fakeRegistryImage{
				imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
				imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
			},
			digests: map[string]string{},
		}
		//testpolicy:allow no-network — the fake registry answers in-process on a loopback TLS server, and the rule under test is that https carries the credential
		server := httptest.NewTLSServer(http.HandlerFunc(registry.serveHTTP))
		t.Cleanup(server.Close)
		writes := 0
		seams := testUpdateImagesSeams(t, server, &writes)

		if _, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml")); err != nil {
			t.Fatalf("gt update images over https: %v", err)
		}
		credentials := registry.credentials()
		if len(credentials) == 0 {
			t.Fatal("the https registry saw no request")
		}
		for _, got := range credentials {
			if got.pass != testUpdateImagesToken {
				t.Errorf("an https request carried no viewer credential")
			}
		}
	})

	// The rule's exact table: the one named service and port, case-insensitive
	// on the host, and a loopback host on any port pass; every look-alike is
	// refused. The command-level subtests above and below assert that a refusal
	// makes no request.
	t.Run("the exact host and port table", func(t *testing.T) {
		t.Parallel()
		path := "/v2/town/gastown-daemon/manifests/" + testUpdateImagesTag
		rows := []struct {
			url  string
			want bool
		}{
			{"http://forgejo:3001" + path, false},
			{"http://evil:3000" + path, false},
			{"http://forgejo.evil.com:3000" + path, false},
			{"http://forgejo" + path, false},
			{"http://localhost.evil.com:3000" + path, false},
			{"http://localhostx:3000" + path, false},
			{"http://127.0.0.1.evil.com:3000" + path, false},
			{"http://127.0.0.2:5000" + path, false},
			{"http://FORGEJO:3000" + path, true},
			{"http://forgejo:3000" + path, true},
			{"http://127.0.0.1:45678" + path, true},
			{"http://[::1]:45678" + path, true},
			{"http://localhost:45678" + path, true},
		}
		for _, row := range rows {
			u, err := url.Parse(row.url)
			if err != nil {
				t.Fatalf("parsing %s: %v", row.url, err)
			}
			err = checkCredentialTransport(u, plaintextHostOf(imagevalues.Registry{Scheme: "http", Host: u.Host}))
			switch {
			case row.want && err != nil:
				t.Errorf("checkCredentialTransport(%s) = %v, want the credential allowed", row.url, err)
			case !row.want && err == nil:
				t.Errorf("checkCredentialTransport(%s) = nil, want a refusal", row.url)
			}
		}
	})

	// The refusal is what a user reads when gt update images stops, so its text
	// is pinned: a change that rewords it has to say so here (gt-4nlpw8).
	t.Run("the refusal names the rule", func(t *testing.T) {
		t.Parallel()
		u, err := url.Parse("http://evil:3000/v2/town/gastown-daemon/manifests/" + testUpdateImagesTag)
		if err != nil {
			t.Fatalf("parsing the refused URL: %v", err)
		}
		err = checkCredentialTransport(u, plaintextHostOf(imagevalues.Registry{Scheme: "http", Host: u.Host}))
		if err == nil {
			t.Fatal("checkCredentialTransport(http://evil:3000) = nil, want a refusal")
		}
		const want = "refusing to send the registry credential over http to evil:3000: http is allowed only to a loopback address or the town's own Forgejo, every other registry is read over https"
		if err.Error() != want {
			t.Errorf("refusal = %q, want %q", err.Error(), want)
		}
	})

	t.Run("http to the town's Forgejo sends the credential", func(t *testing.T) {
		t.Parallel()
		var seen []fakeCredential
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			user, pass, _ := req.BasicAuth()
			seen = append(seen, fakeCredential{user: user, pass: pass})
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
		})}
		seams := updateImagesSeams{
			FindTownRoot: func() (string, error) { return "/town", nil },
			Registry: func(string) (imagevalues.Registry, error) {
				return imagevalues.Registry{Scheme: "http", Host: "forgejo:3000", Owner: testUpdateImagesOwner}, nil
			},
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  client,
			WriteValues: func(string, imagevalues.Values) error { return nil },
		}

		// The 404 refuses the run; what is under test is that the request to
		// the town's own Forgejo was made at all, with the credential.
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil {
			t.Fatal("a 404 registry succeeded, want a refusal")
		}
		if len(seen) == 0 {
			t.Fatal("no request was made to http://forgejo:3000")
		}
		if seen[0].pass != testUpdateImagesToken {
			t.Errorf("the request to the town's Forgejo carried no viewer credential")
		}
	})

	t.Run("loopback sends the credential", func(t *testing.T) {
		t.Parallel()
		registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
			imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
			imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
		})
		writes := 0
		if _, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml")); err != nil {
			t.Fatalf("gt update images over loopback http: %v", err)
		}
		if len(registry.credentials()) == 0 {
			t.Fatal("the loopback registry saw no request")
		}
	})

	t.Run("http to another host is refused before a request", func(t *testing.T) {
		t.Parallel()
		requests := 0
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		})}
		// A dotted host is a public name, which has an https endpoint: the
		// command builds http://registry.example.com from this registry and
		// must refuse it before the request.
		seams := updateImagesSeams{
			FindTownRoot: func() (string, error) { return "/town", nil },
			Registry: func(string) (imagevalues.Registry, error) {
				return imagevalues.Registry{Scheme: "http", Host: "registry.example.com", Owner: testUpdateImagesOwner}, nil
			},
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  client,
			WriteValues: func(string, imagevalues.Values) error { return nil },
		}

		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil {
			t.Fatal("http to another host succeeded, want a refusal")
		}
		for _, want := range []string{"registry.example.com", "http"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error %q does not name %q", err, want)
			}
		}
		if requests != 0 {
			t.Errorf("the refused read made %d requests, want none", requests)
		}
	})

	// The plaintext exception is the exact host and port, not any dotless or
	// look-alike name: each of these is read over https like every other
	// non-loopback host, and no request carrying the credential is made.
	for _, host := range []string{
		"forgejo:3001",
		"evil:3000",
		"forgejo.evil.com:3000",
		"forgejo",
		"localhost.evil.com",
		"localhostx",
		"127.0.0.1.evil.com",
		"127.0.0.2",
	} {
		t.Run("http to "+host+" is refused before a request", func(t *testing.T) {
			t.Parallel()
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
			})}
			seams := updateImagesSeams{
				FindTownRoot: func() (string, error) { return "/town", nil },
				Registry: func(string) (imagevalues.Registry, error) {
					return imagevalues.Registry{Scheme: "http", Host: host, Owner: testUpdateImagesOwner}, nil
				},
				Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
				HTTPClient:  client,
				WriteValues: func(string, imagevalues.Values) error { return nil },
			}

			_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
			if err == nil {
				t.Fatalf("http to %s succeeded, want a refusal", host)
			}
			for _, want := range []string{host, "http"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error %q does not name %q", err, want)
				}
			}
			if requests != 0 {
				t.Errorf("the refused read made %d requests, want none", requests)
			}
		})
	}

	t.Run("a redirect to another host is refused", func(t *testing.T) {
		t.Parallel()
		var targetRequests atomic.Int32
		//testpolicy:allow no-network — the redirect target is an in-process httptest server; the test asserts it is never reached
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) }))
		t.Cleanup(target.Close)
		//testpolicy:allow no-network — the redirecting registry is an in-process httptest server on loopback
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(source.Close)
		writes := 0
		seams := testUpdateImagesSeams(t, source, &writes)

		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil {
			t.Fatal("a redirect to another host succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("the error %q does not name the redirect", err)
		}
		if got := targetRequests.Load(); got != 0 {
			t.Errorf("the redirect target saw %d requests, want none", got)
		}
		if writes != 0 {
			t.Errorf("the refused run wrote the values file %d times", writes)
		}
	})

	t.Run("a redirect that changes the scheme is refused", func(t *testing.T) {
		t.Parallel()
		requests := 0
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			// Only the scheme differs from the first request's URL: the host and
			// port stay the same, so this drives the scheme half of the rule on
			// its own.
			if req.URL.Scheme == "https" {
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": []string{"http://registry.test:5000/v2/town/gastown-daemon/manifests/" + testUpdateImagesTag}},
					Body:       http.NoBody,
				}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		})}
		seams := updateImagesSeams{
			FindTownRoot: func() (string, error) { return "/town", nil },
			Registry: func(string) (imagevalues.Registry, error) {
				return imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: testUpdateImagesOwner}, nil
			},
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  client,
			WriteValues: func(string, imagevalues.Values) error { return nil },
		}

		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil {
			t.Fatal("a redirect from https to http succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("the error %q does not name the redirect", err)
		}
		if requests != 1 {
			t.Errorf("the downgrading redirect was followed: %d requests, want the one that answered", requests)
		}
	})
}

// TestUpdateImagesRefusesACommandWithNoSeams holds the wiring rule: a command
// built without its reader or writer refuses instead of reporting success.
func TestUpdateImagesRefusesACommandWithNoSeams(t *testing.T) {
	t.Parallel()
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	valid := testUpdateImagesSeams(t, server, &writes)
	rows := []struct {
		name   string
		mutate func(*updateImagesSeams)
		want   string
	}{
		{"no town lookup", func(s *updateImagesSeams) { s.FindTownRoot = nil }, "no town lookup is wired"},
		{"no registry", func(s *updateImagesSeams) { s.Registry = nil }, "no registry is wired"},
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

	t.Run("a registry that does not validate", func(t *testing.T) {
		t.Parallel()
		seams := valid
		seams.Registry = func(string) (imagevalues.Registry, error) {
			return imagevalues.Registry{Scheme: "https", Host: "registry.test/path", Owner: "town"}, nil
		}
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil || !strings.Contains(err.Error(), "host") {
			t.Fatalf("gt update images with an unusable registry = %v, want a refusal naming the host", err)
		}
	})

	t.Run("a registry that cannot be resolved", func(t *testing.T) {
		t.Parallel()
		seams := valid
		seams.Registry = func(string) (imagevalues.Registry, error) {
			return imagevalues.Registry{}, errors.New("the town names no image rig")
		}
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil || !strings.Contains(err.Error(), "no image rig") {
			t.Fatalf("gt update images with no registry source = %v, want the resolver's refusal", err)
		}
	})

	t.Run("a town root that cannot be found", func(t *testing.T) {
		t.Parallel()
		seams := valid
		seams.FindTownRoot = func() (string, error) { return "", errors.New("not in a town") }
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil || !strings.Contains(err.Error(), "town root") {
			t.Fatalf("gt update images outside a town = %v, want a town-root refusal", err)
		}
	})
}

// TestUpdateImagesRefusesAnUnreadableCredential holds the credential rule: a
// viewer credential that cannot be read refuses before any registry request.
func TestUpdateImagesRefusesAnUnreadableCredential(t *testing.T) {
	t.Parallel()
	registry, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	seams := testUpdateImagesSeams(t, server, &writes)
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
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})

	t.Run("an existing values file that does not parse", func(t *testing.T) {
		t.Parallel()
		writes := 0
		out := filepath.Join(t.TempDir(), "images.yaml")
		if err := os.WriteFile(out, []byte("daemon: ["), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		_, _, err := runUpdateImagesCommand(t, testUpdateImagesSeams(t, server, &writes), "--sha", testUpdateImagesSHA, "--out", out)
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
		seams := testUpdateImagesSeams(t, server, &writes)
		seams.WriteValues = func(string, imagevalues.Values) error { return errors.New("the disk is full") }
		_, _, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA, "--out", filepath.Join(t.TempDir(), "images.yaml"))
		if err == nil || !strings.Contains(err.Error(), "the disk is full") {
			t.Fatalf("gt update images = %v, want the writer's error", err)
		}
	})
}

// errReader fails every read, so a response body that breaks mid-read is
// driven without a network.
type errReader struct{}

// Read always fails.
func (errReader) Read([]byte) (int, error) { return 0, errors.New("the connection broke") }

// TestUpdateImagesDrivesEveryRegistryFailure holds the branches a broken
// registry address or answer reaches: the request cannot be built, the server
// cannot be reached, the body breaks, or the body is too large.
func TestUpdateImagesDrivesEveryRegistryFailure(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-network — the server is closed before use: it exists only to give a loopback address nothing listens on, and the branch under test is the failed dial
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable.Close()

	// scripted is a registry whose answer these rows script, so no address on
	// this host is dialled.
	scripted := imagevalues.Registry{Scheme: "https", Host: "127.0.0.1:1", Owner: testUpdateImagesOwner}
	rows := []struct {
		name     string
		registry imagevalues.Registry
		http     *http.Client
		want     string
	}{
		{
			name:     "a host Validate refuses",
			registry: imagevalues.Registry{Scheme: "https", Host: "[::::]", Owner: testUpdateImagesOwner},
			http:     http.DefaultClient,
			want:     "host",
		},
		{
			name: "a server that cannot be reached",
			registry: imagevalues.Registry{
				Scheme: "http",
				Host:   strings.TrimPrefix(unreachable.URL, "http://"),
				Owner:  testUpdateImagesOwner,
			},
			http: unreachable.Client(),
			want: "connect",
		},
		{
			name:     "a body that breaks mid-read",
			registry: scripted,
			http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(errReader{})}, nil
			})},
			want: "the connection broke",
		},
		{
			name:     "a body larger than the bound",
			registry: scripted,
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
				FindTownRoot: func() (string, error) { return "/town", nil },
				Registry:     func(string) (imagevalues.Registry, error) { return row.registry, nil },
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

// TestUpdateImagesRefusesAMalformedRequestURL holds the guard below Validate: a
// caller that hands the reader an address it cannot build a request from is
// refused before a dial, rather than reading an error as an answer.
func TestUpdateImagesRefusesAMalformedRequestURL(t *testing.T) {
	t.Parallel()
	reader := registryReader{http: http.DefaultClient}
	_, _, err := reader.getJSON(context.Background(), "https://[::::]/v2/town/gastown-daemon/manifests/"+testUpdateImagesTag, updateImagesMaxManifest, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "not a URL") {
		t.Fatalf("getJSON = %v, want a refusal naming the address", err)
	}
}

// TestTownImageRegistryReadsTheTownsForgejo holds the source rule: the registry
// host and owner come from the town's own configuration — the rig whose green
// sweep publishes the image build's ref, and that rig's
// merge_queue.forgejo.remote_url — never from a constant in the shared package.
func TestTownImageRegistryReadsTheTownsForgejo(t *testing.T) {
	t.Parallel()

	const (
		rigsJSON = `{"version":1,"rigs":{"gastown":{"git_url":"https://github.com/example/gastown.git"}}}`
		daemon   = `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"image_rigs":["gastown"]}}}`
	)
	rigConfig := func(remote string) string {
		return `{"type":"rig","version":1,"name":"gastown","git_url":"https://github.com/example/gastown.git","merge_queue":{"forgejo":{"remote_url":"` + remote + `"}}}`
	}

	t.Run("a plaintext town Forgejo", func(t *testing.T) {
		t.Parallel()
		townRoot := writeTownFixture(t, map[string]string{
			"mayor/rigs.json":     rigsJSON,
			"mayor/daemon.json":   daemon,
			"gastown/config.json": rigConfig("http://forgejo:3000/sloan/gastown"),
		})
		got, err := townImageRegistry(townRoot)
		if err != nil {
			t.Fatalf("townImageRegistry: %v", err)
		}
		want := imagevalues.Registry{Scheme: "http", Host: "forgejo:3000", Owner: "sloan"}
		if got != want {
			t.Errorf("townImageRegistry = %+v, want %+v", got, want)
		}
	})

	t.Run("an https Forgejo", func(t *testing.T) {
		t.Parallel()
		townRoot := writeTownFixture(t, map[string]string{
			"mayor/rigs.json":     rigsJSON,
			"mayor/daemon.json":   daemon,
			"gastown/config.json": rigConfig("https://forgejo.example.com/acme/gastown"),
		})
		got, err := townImageRegistry(townRoot)
		if err != nil {
			t.Fatalf("townImageRegistry: %v", err)
		}
		want := imagevalues.Registry{Scheme: "https", Host: "forgejo.example.com", Owner: "acme"}
		if got != want {
			t.Errorf("townImageRegistry = %+v, want %+v", got, want)
		}
	})

	rows := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "no image rig named",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true}}}`,
				"gastown/config.json": rigConfig("http://forgejo:3000/sloan/gastown"),
			},
			want: "image_rigs",
		},
		{
			name: "an image rig the registry does not know",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"image_rigs":["gastwon"]}}}`,
				"gastown/config.json": rigConfig("http://forgejo:3000/sloan/gastown"),
			},
			want: "gastwon",
		},
		{
			name: "a rig with no Forgejo block",
			files: map[string]string{
				"mayor/rigs.json":   rigsJSON,
				"mayor/daemon.json": daemon,
			},
			want: "remote_url",
		},
		{
			name: "a rig whose Forgejo remote names no host",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   daemon,
				"gastown/config.json": rigConfig("gastown"),
			},
			want: "remote",
		},
		{
			name: "a daemon settings file that does not parse",
			files: map[string]string{
				"mayor/rigs.json":   rigsJSON,
				"mayor/daemon.json": "{not json",
			},
			want: "daemon settings",
		},
		{
			name: "a rig registry that cannot be read",
			files: map[string]string{
				"mayor/daemon.json": daemon,
			},
			want: "rig registry",
		},
		{
			name: "a rig whose settings do not parse",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   daemon,
				"gastown/config.json": "{not json",
			},
			want: "Forgejo settings",
		},
		{
			name: "a Forgejo remote that names no repository",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   daemon,
				"gastown/config.json": rigConfig("https://forgejo.example.com"),
			},
			want: "owner and repository",
		},
		{
			name: "a Forgejo remote over another scheme",
			files: map[string]string{
				"mayor/rigs.json":     rigsJSON,
				"mayor/daemon.json":   daemon,
				"gastown/config.json": rigConfig("ftp://forgejo.example.com/acme/gastown"),
			},
			want: "scheme",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := townImageRegistry(writeTownFixture(t, row.files))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("townImageRegistry = %v, want a refusal naming %q", err, row.want)
			}
		})
	}
}

// TestTownImageRegistryReadsTheRegistryOwner: a rig names the registry account
// separately from its remote's owner (gt-69fgg3). Set, it replaces the remote's
// owner; empty or whitespace-only, it is the remote's owner as before; an
// invalid value is refused, and the refusal names the setting.
func TestTownImageRegistryReadsTheRegistryOwner(t *testing.T) {
	t.Parallel()

	const (
		rigsJSON = `{"version":1,"rigs":{"gastown":{"git_url":"https://github.com/example/gastown.git"}}}`
		daemon   = `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"image_rigs":["gastown"]}}}`
		remote   = "http://forgejo:3000/sloan/gastown"
	)
	rigConfig := func(registryOwner string) string {
		return `{"type":"rig","version":1,"name":"gastown","git_url":"https://github.com/example/gastown.git","merge_queue":{"forgejo":{"remote_url":"` + remote + `","registry_owner":"` + registryOwner + `"}}}`
	}
	files := func(registryOwner string) map[string]string {
		return map[string]string{
			"mayor/rigs.json":     rigsJSON,
			"mayor/daemon.json":   daemon,
			"gastown/config.json": rigConfig(registryOwner),
		}
	}

	ok := []struct {
		name          string
		registryOwner string
		wantOwner     string
	}{
		{name: "a set owner replaces the remote's owner", registryOwner: "bot-registry", wantOwner: "bot-registry"},
		{name: "a set owner is trimmed", registryOwner: "  bot-registry  ", wantOwner: "bot-registry"},
		{name: "an empty owner is the remote's owner", registryOwner: "", wantOwner: "sloan"},
		{name: "a whitespace-only owner is the remote's owner", registryOwner: ` \t `, wantOwner: "sloan"},
	}
	for _, row := range ok {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := townImageRegistry(writeTownFixture(t, files(row.registryOwner)))
			if err != nil {
				t.Fatalf("townImageRegistry: %v", err)
			}
			want := imagevalues.Registry{Scheme: "http", Host: "forgejo:3000", Owner: row.wantOwner}
			if got != want {
				t.Errorf("townImageRegistry = %+v, want %+v", got, want)
			}
		})
	}

	refused := []struct {
		name          string
		registryOwner string
	}{
		{name: "an owner with a space", registryOwner: "bot registry"},
		{name: "an owner with an uppercase letter", registryOwner: "BotRegistry"},
		{name: "an owner with a path separator", registryOwner: "bot/registry"},
	}
	for _, row := range refused {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := townImageRegistry(writeTownFixture(t, files(row.registryOwner)))
			if err == nil || !strings.Contains(err.Error(), "merge_queue.forgejo.registry_owner") {
				t.Fatalf("townImageRegistry = %v, want a refusal naming merge_queue.forgejo.registry_owner", err)
			}
		})
	}
}

// TestTownImageRegistryRescuesANoOwnerRemoteWithTheRegistryOwner: a rig's remote
// that names no owner (http://forgejo:3000) is still readable when the rig names
// its registry account, because the account, not the remote's owner, is what the
// registry needs (gt-5m3ai1). With no account named, the remote's missing owner
// is still refused, and a named account is validated like any other.
func TestTownImageRegistryRescuesANoOwnerRemoteWithTheRegistryOwner(t *testing.T) {
	t.Parallel()

	const (
		rigsJSON = `{"version":1,"rigs":{"gastown":{"git_url":"https://github.com/example/gastown.git"}}}`
		daemon   = `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"image_rigs":["gastown"]}}}`
		remote   = "http://forgejo:3000"
	)
	rigConfig := func(registryOwner string) string {
		return `{"type":"rig","version":1,"name":"gastown","git_url":"https://github.com/example/gastown.git","merge_queue":{"forgejo":{"remote_url":"` + remote + `","registry_owner":"` + registryOwner + `"}}}`
	}
	files := func(registryOwner string) map[string]string {
		return map[string]string{
			"mayor/rigs.json":     rigsJSON,
			"mayor/daemon.json":   daemon,
			"gastown/config.json": rigConfig(registryOwner),
		}
	}

	t.Run("a set owner stands in for the remote's missing owner", func(t *testing.T) {
		t.Parallel()
		got, err := townImageRegistry(writeTownFixture(t, files("bot-registry")))
		if err != nil {
			t.Fatalf("townImageRegistry: %v", err)
		}
		want := imagevalues.Registry{Scheme: "http", Host: "forgejo:3000", Owner: "bot-registry"}
		if got != want {
			t.Errorf("townImageRegistry = %+v, want %+v", got, want)
		}
	})

	refused := []struct {
		name          string
		registryOwner string
		want          string
	}{
		{name: "no owner set and the remote names none", registryOwner: "", want: "owner and repository"},
		{name: "an owner with a space stands in and is refused", registryOwner: "bot registry", want: "merge_queue.forgejo.registry_owner"},
	}
	for _, row := range refused {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := townImageRegistry(writeTownFixture(t, files(row.registryOwner)))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("townImageRegistry = %v, want a refusal naming %q", err, row.want)
			}
		})
	}
}

// TestUpdateImagesWorkflowNamesTheTownsRegistry holds the one-spelling rule
// across the build and the command: the workflow's FORGEJO_HOST and
// REGISTRY_OWNER are the host and owner gt update images reads from the town's
// rig settings. A drift between the registry the image build pushes to and the
// one the command reads is a failure, not two silent values.
func TestUpdateImagesWorkflowNamesTheTownsRegistry(t *testing.T) {
	t.Parallel()
	// The fixture is the same town config TestImagesWorkflowNamesTheSharedImageConstants
	// in internal/ci holds the workflow's env to.
	townRoot := filepath.FromSlash("../../deploy/ci/testdata/images-town")
	registry, err := townImageRegistry(townRoot)
	if err != nil {
		t.Fatalf("townImageRegistry: %v", err)
	}

	raw, err := os.ReadFile(filepath.FromSlash("../../deploy/ci/images.workflow.yml"))
	if err != nil {
		t.Fatalf("reading the images workflow: %v", err)
	}
	var doc struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the images workflow is not valid YAML: %v", err)
	}
	for _, want := range []struct{ key, value string }{
		{"FORGEJO_HOST", registry.Host},
		{"REGISTRY_OWNER", registry.Owner},
	} {
		if got := doc.Env[want.key]; got != want.value {
			t.Errorf("the workflow's %s is %q, but gt update images reads %q from the town's rig settings", want.key, got, want.value)
		}
	}
}

// writeTownFixture writes a town's config files under a temporary root.
func writeTownFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return root
}

// TestUpdateImagesUsesTheSharedPath holds the one place the values file's path
// lives: with no --out the command writes the file at imagevalues.PathIn under
// the town's gastown directory whatever directory it runs in, the verify probe
// reads that same file back, and no other source in this package names the
// file, so moving it changes one constant and every consumer follows
// (gt-mwjtgg).
//
//testpolicy:allow parallel — moves the working directory, which another parallel test must not change mid-run
func TestUpdateImagesUsesTheSharedPath(t *testing.T) {
	t.Run("the default output is the shared path, read back by the probe", func(t *testing.T) {
		//testpolicy:allow no-chdir — the working directory is the input under test: the default path must not resolve against it
		t.Chdir(t.TempDir())
		townRoot := t.TempDir()
		_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
			imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
			imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
		})
		writes := 0
		seams := testUpdateImagesSeams(t, server, &writes)
		seams.FindTownRoot = func() (string, error) { return townRoot, nil }

		stdout, stderr, err := runUpdateImagesCommand(t, seams, "--sha", testUpdateImagesSHA)
		if err != nil {
			t.Fatalf("gt update images with no --out: %v (stderr %q)", err, stderr)
		}
		path := imagevalues.PathIn(filepath.Join(townRoot, "gastown"))
		if writes != 1 {
			t.Fatalf("the values file was written %d times, want once at %s", writes, path)
		}
		if !strings.Contains(stdout, path) {
			t.Errorf("the command's output %q does not name %s", stdout, path)
		}
		if _, err := os.Stat(imagevalues.DefaultRelPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the command wrote a values file relative to its working directory: %v", err)
		}
		values, err := imagevalues.Load(path)
		if err != nil {
			t.Fatalf("reading the file the command wrote at the shared path: %v", err)
		}

		// The verify probe reads the same file through the directory its
		// production wiring passes, so a writer/reader path mismatch fails here.
		run := &verifyProbeRun{output: values.Commit + "\n"}
		probes := newVerifyProbes(verifyProbeEnv{
			Run:       run.run,
			ValuesDir: func() string { return filepath.Join(townRoot, "gastown") },
		})
		label, err := probes.ImageLabel(context.Background())
		if err != nil {
			t.Fatalf("the verify probe reading the file the command wrote: %v", err)
		}
		if label != values.Commit {
			t.Errorf("the probe read label %q, want the file's commit %q", label, values.Commit)
		}
		if len(run.calls) != 1 || !slices.Contains(run.calls[0].args, values.Polecat) {
			t.Errorf("the probe ran %+v, want an inspect of the file's polecat image %s", run.calls, values.Polecat)
		}
	})

	t.Run("no other package source names the file", func(t *testing.T) {
		t.Parallel()
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatalf("reading the package sources: %v", err)
		}
		scanned := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			scanned++
			if strings.Contains(string(data), imagevalues.DefaultRelPath) {
				t.Errorf("%s names %s; the command reads it from imagevalues.DefaultRelPath", name, imagevalues.DefaultRelPath)
			}
		}
		if scanned == 0 {
			t.Fatal("no package source was scanned, so this test proves nothing")
		}
	})
}
