package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// podTargetCommit is the commit the target pair's polecat image is labeled with.
	podTargetCommit = "1111111111111111111111111111111111111111"
	// podOldCommit is the commit the previous pair's polecat image carries; the
	// town copy of the values file still names it.
	podOldCommit = "2222222222222222222222222222222222222222"
	// podOtherCommit is a commit no pair in these tests names.
	podOtherCommit = "3333333333333333333333333333333333333333"
)

// podLabelRegistry is an in-process registry that answers the polecat image's
// manifests and config blobs by digest, so one repository can hold two pairs'
// images at once. It records every path it is asked for.
type podLabelRegistry struct {
	mu        sync.Mutex
	manifests map[string]podLabelManifest
	blobs     map[string][]byte
	asked     []string
}

// podLabelManifest is one manifest the registry serves. Its bytes are body; the
// digest it answers with is answer, or the digest of body when answer is "".
type podLabelManifest struct {
	body   []byte
	answer string
}

// newPodLabelRegistry is an empty registry for the polecat image.
func newPodLabelRegistry() *podLabelRegistry {
	return &podLabelRegistry{manifests: map[string]podLabelManifest{}, blobs: map[string][]byte{}}
}

// addImage holds a polecat image labeled commit and returns its manifest digest.
func (r *podLabelRegistry) addImage(commit string) string {
	return r.addRebuild(commit, "")
}

// addRebuild holds a polecat image labeled commit whose manifest carries marker,
// so an image with the same label and another digest can be held beside it.
func (r *podLabelRegistry) addRebuild(commit, marker string) string {
	cfg := fakeConfigBytes(commit)
	cfgDigest := testDigestOf(cfg)
	annotation := ""
	if marker != "" {
		annotation = `,"annotations":{"org.example.rebuild":"` + marker + `"}`
	}
	body := []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cfgDigest + `"}` + annotation + `}`)
	digest := testDigestOf(body)
	r.mu.Lock()
	r.manifests[digest] = podLabelManifest{body: body}
	r.blobs[cfgDigest] = cfg
	r.mu.Unlock()
	return digest
}

// manifest and blob read what the registry holds under its lock, so a request
// served on the server's goroutine never races an image added by the test.
func (r *podLabelRegistry) manifest(ref string) (podLabelManifest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.manifests[ref]
	return m, ok
}

func (r *podLabelRegistry) blob(ref string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blobs[ref]
	return b, ok
}

// askedRefs is every path the registry was asked for, in order.
func (r *podLabelRegistry) askedRefs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// serveHTTP answers the polecat image's manifest and blob endpoints by digest.
func (r *podLabelRegistry) serveHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.asked = append(r.asked, req.URL.Path)
	r.mu.Unlock()
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/"+testUpdateImagesOwner+"/"+imagevalues.PolecatImage+"/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	endpoint, ref, _ := strings.Cut(rest, "/")
	switch endpoint {
	case "manifests":
		m, held := r.manifest(ref)
		if !held {
			http.NotFound(w, req)
			return
		}
		answer := m.answer
		if answer == "" {
			answer = testDigestOf(m.body)
		}
		w.Header().Set("Docker-Content-Digest", answer)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(m.body)
	case "blobs":
		b, held := r.blob(ref)
		if !held {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, req)
	}
}

// startPodLabelRegistry serves r until the test ends and returns the registry's
// seams: the town's registry is the server's own, owned by testUpdateImagesOwner.
func startPodLabelRegistry(t *testing.T, r *podLabelRegistry) updateImagesSeams {
	t.Helper()
	//testpolicy:allow no-network — the registry is an in-process httptest server on loopback, the only client the test wires
	server := httptest.NewServer(http.HandlerFunc(r.serveHTTP))
	t.Cleanup(server.Close)
	registry := testRegistryFor(t, server)
	return updateImagesSeams{
		Registry:    func(string) (imagevalues.Registry, error) { return registry, nil },
		Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
		HTTPClient:  server.Client(),
	}
}

// podLabelValues is the pair a values file records for polecat at commit: the
// polecat ref names the town's registry, and the daemon is a digest of its own.
func podLabelValues(seams updateImagesSeams, polecatDigest, commit string) imagevalues.Values {
	registry, _ := seams.Registry("")
	return imagevalues.Values{
		Daemon:  registry.Ref(imagevalues.DaemonImage, testDigestOf([]byte("daemon"))),
		Polecat: registry.Ref(imagevalues.PolecatImage, polecatDigest),
		Commit:  commit,
	}
}

// TestImageLabelPodNoExec holds the pod target's label read to the registry: the
// label comes back with no command run, so a test that sees one run fails.
func TestImageLabelPodNoExec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newPodLabelRegistry()
	seams := startPodLabelRegistry(t, reg)
	digest := reg.addImage(podTargetCommit)
	imagesPath := filepath.Join(t.TempDir(), "images.yaml")
	if err := imagevalues.Write(imagesPath, podLabelValues(seams, digest, podTargetCommit)); err != nil {
		t.Fatalf("writing the images file: %v", err)
	}
	// The command seam answers with another commit, as docker would for an image
	// the target pair does not name. A read that ran docker would record a call
	// and return this commit, failing both checks below.
	run := &verifyProbeRun{output: podOtherCommit + "\n"}
	probes := podImageLabelProbes{
		VerifyProbes: newVerifyProbes(verifyProbeEnv{Run: run.run}),
		label:        podTargetImageLabel(seams, t.TempDir(), imagesPath),
	}

	got, err := probes.ImageLabel(ctx)
	if err != nil {
		t.Fatalf("pod image label: %v", err)
	}
	if got != podTargetCommit {
		t.Errorf("label = %q, want the target commit %q", got, podTargetCommit)
	}
	if len(run.calls) != 0 {
		t.Errorf("the pod target ran %+v, want no command: the registry is read in process", run.calls)
	}
}

// TestImageLabelUsesTargetImages holds the label to the images file the Job was
// given. The town copy names the previous pair, whose polecat image is served
// too, so reading it would answer the old commit and ask for the old digest.
func TestImageLabelUsesTargetImages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newPodLabelRegistry()
	seams := startPodLabelRegistry(t, reg)
	oldDigest := reg.addImage(podOldCommit)
	targetDigest := reg.addImage(podTargetCommit)

	townRoot := t.TempDir()
	if err := imagevalues.Write(imagevalues.PathIn(filepath.Join(townRoot, "gastown")), podLabelValues(seams, oldDigest, podOldCommit)); err != nil {
		t.Fatalf("writing the town copy: %v", err)
	}
	imagesPath := filepath.Join(t.TempDir(), "images.yaml")
	if err := imagevalues.Write(imagesPath, podLabelValues(seams, targetDigest, podTargetCommit)); err != nil {
		t.Fatalf("writing the images file: %v", err)
	}

	got, err := podTargetImageLabel(seams, townRoot, imagesPath)(ctx)
	if err != nil {
		t.Fatalf("pod image label: %v", err)
	}
	if got != podTargetCommit {
		t.Errorf("label = %q, want the target pair's commit %q", got, podTargetCommit)
	}
	asked := reg.askedRefs()
	if !slices.ContainsFunc(asked, func(p string) bool { return strings.HasSuffix(p, "/"+targetDigest) }) {
		t.Errorf("asked %v, want the target pair's digest %s", asked, targetDigest)
	}
	if slices.ContainsFunc(asked, func(p string) bool { return strings.Contains(p, oldDigest) }) {
		t.Errorf("asked %v, which reads the previous pair's digest %s from the town copy", asked, oldDigest)
	}
}

// TestImageLabelPodRefusals drives each way the pod target's label read is
// refused. Every refusal is an error, and none asks the registry for anything
// it has no pair to name.
func TestImageLabelPodRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no images file names no target pair", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		reg.addImage(podTargetCommit)
		if _, err := podTargetImageLabel(seams, t.TempDir(), "")(ctx); err == nil {
			t.Fatal("an empty images path reported no error")
		}
		if asked := reg.askedRefs(); len(asked) != 0 {
			t.Errorf("asked %v with no images file, want no request", asked)
		}
	})

	t.Run("an unreadable images file is an error", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		dir := t.TempDir()
		if _, err := podTargetImageLabel(seams, t.TempDir(), dir)(ctx); err == nil {
			t.Fatal("an images path that is a directory reported no error")
		}
		if asked := reg.askedRefs(); len(asked) != 0 {
			t.Errorf("asked %v for an unreadable images file, want no request", asked)
		}
	})

	t.Run("a polecat ref that is not the town's registry is refused", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		digest := reg.addImage(podTargetCommit)
		v := podLabelValues(seams, digest, podTargetCommit)
		v.Polecat = "other.example/town/gastown-polecat@" + digest
		imagesPath := filepath.Join(t.TempDir(), "images.yaml")
		if err := imagevalues.Write(imagesPath, v); err != nil {
			t.Fatalf("writing the images file: %v", err)
		}
		if _, err := podTargetImageLabel(seams, t.TempDir(), imagesPath)(ctx); err == nil {
			t.Fatal("a polecat ref on another registry reported no error")
		}
		if asked := reg.askedRefs(); len(asked) != 0 {
			t.Errorf("asked %v for a ref on another registry, want no request", asked)
		}
	})

	t.Run("a registry that answers another digest for the one asked is refused", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		wanted := reg.addImage(podTargetCommit)
		rebuilt := reg.addRebuild(podTargetCommit, "rebuilt")
		// The bytes held under the asked digest are the rebuilt image's manifest,
		// which the registry answers with its own digest: each answer is
		// self-consistent, and the commit label is the one the file names.
		reg.manifests[wanted] = reg.manifests[rebuilt]
		imagesPath := filepath.Join(t.TempDir(), "images.yaml")
		if err := imagevalues.Write(imagesPath, podLabelValues(seams, wanted, podTargetCommit)); err != nil {
			t.Fatalf("writing the images file: %v", err)
		}
		if _, err := podTargetImageLabel(seams, t.TempDir(), imagesPath)(ctx); err == nil {
			t.Fatal("a registry that answered another digest reported no error")
		}
	})

	t.Run("a label that differs from the file's commit is an error naming both", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		digest := reg.addImage(podOtherCommit)
		imagesPath := filepath.Join(t.TempDir(), "images.yaml")
		if err := imagevalues.Write(imagesPath, podLabelValues(seams, digest, podTargetCommit)); err != nil {
			t.Fatalf("writing the images file: %v", err)
		}
		_, err := podTargetImageLabel(seams, t.TempDir(), imagesPath)(ctx)
		if err == nil {
			t.Fatal("a label that differs from the file's commit reported no error")
		}
		for _, commit := range []string{podOtherCommit, podTargetCommit} {
			if !strings.Contains(err.Error(), commit) {
				t.Errorf("error %q does not name commit %s", err, commit)
			}
		}
	})

	t.Run("a polecat digest that is not sha256 hex is refused before any request", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		v := podLabelValues(seams, reg.addImage(podTargetCommit), podTargetCommit)
		registry, err := seams.Registry("")
		if err != nil {
			t.Fatalf("the test registry: %v", err)
		}
		badRef := registry.Ref(imagevalues.PolecatImage, "sha256:"+strings.Repeat("g", 64))
		imagesPath := filepath.Join(t.TempDir(), "images.yaml")
		raw := "daemon: " + strconv.Quote(v.Daemon) + "\npolecat: " + strconv.Quote(badRef) + "\ncommit: " + v.Commit + "\n"
		if err := os.WriteFile(imagesPath, []byte(raw), 0o600); err != nil {
			t.Fatalf("writing the images file: %v", err)
		}
		if _, err := podTargetImageLabel(seams, t.TempDir(), imagesPath)(ctx); err == nil {
			t.Fatal("a polecat digest that is not sha256 hex reported no error")
		}
		if asked := reg.askedRefs(); len(asked) != 0 {
			t.Errorf("asked %v for a malformed polecat digest, want no request", asked)
		}
	})

	t.Run("a seam that is not wired is refused, not read", func(t *testing.T) {
		t.Parallel()
		reg := newPodLabelRegistry()
		seams := startPodLabelRegistry(t, reg)
		imagesPath := filepath.Join(t.TempDir(), "images.yaml")
		if err := imagevalues.Write(imagesPath, podLabelValues(seams, reg.addImage(podTargetCommit), podTargetCommit)); err != nil {
			t.Fatalf("writing the images file: %v", err)
		}
		seams.Registry = nil
		if _, err := podTargetImageLabel(seams, t.TempDir(), imagesPath)(ctx); err == nil {
			t.Fatal("an unwired registry reported no error")
		}
		if asked := reg.askedRefs(); len(asked) != 0 {
			t.Errorf("asked %v with no registry wired, want no request", asked)
		}
	})
}
