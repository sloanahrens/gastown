package cmd

import (
	"context"
	"errors"
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
	"github.com/steveyegge/gastown/internal/update"
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

// podVerifyCheck is the check update.Verify reported under name.
func podVerifyCheck(t *testing.T, checks []update.Check, name string) update.Check {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("verify reported no %s check: %+v", name, checks)
	return update.Check{}
}

// TestPodImageLabelProbeRefusesAnUnwiredRead: a probe with no registry read
// wired fails its image-label check instead of passing or crashing.
func TestPodImageLabelProbeRefusesAnUnwiredRead(t *testing.T) {
	t.Parallel()
	probes := podImageLabelProbes{}

	if _, err := probes.ImageLabel(context.Background()); err == nil {
		t.Fatal("an unwired image label read reported no error")
	}
}

// TestPodPairProbesReportThePair holds the pair probes to their readers: the
// wanted polecat digest is the run's, and each check reports the digest its
// reader answers without asking for it another way.
func TestPodPairProbesReportThePair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := testDigestOf(podPairManifest)
	probes := podImageLabelProbes{
		VerifyProbes: newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: (&verifyProbeRun{}).run}),
		wantPolecat:  want,
		annotation:   func(context.Context) (string, error) { return want, nil },
		devTag:       func(context.Context) (string, error) { return want, nil },
	}

	if got := probes.WantPolecat(); got != want {
		t.Errorf("WantPolecat = %q, want %q", got, want)
	}
	if got, err := probes.PolecatAnnotation(ctx); err != nil || got != want {
		t.Errorf("PolecatAnnotation = %q, %v; want the reader's digest %q", got, err, want)
	}
	if got, err := probes.DevTagDigest(ctx); err != nil || got != want {
		t.Errorf("DevTagDigest = %q, %v; want the reader's digest %q", got, err, want)
	}
	checks := update.Verify(ctx, probes, rollbackNewCommit)
	for _, name := range []string{"polecat-annotation", "dev-tag"} {
		if check := podVerifyCheck(t, checks, name); !check.OK || check.Detail != want {
			t.Errorf("%s = %+v, want ok with the digest %q", name, check, want)
		}
	}
}

// TestPodPairProbesFailOnAFailedRead drives each way one of the pair checks can
// fail: the check that read the failed answer fails, and the other is judged on
// its own read.
func TestPodPairProbesFailOnAFailedRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := testDigestOf(podPairManifest)
	boom := errors.New("the read failed")
	good := func(context.Context) (string, error) { return want, nil }
	fail := func(context.Context) (string, error) { return "", boom }

	tests := []struct {
		name   string
		probes podImageLabelProbes
		failed string
		detail string
	}{
		{
			name:   "the annotation read fails",
			probes: podImageLabelProbes{annotation: fail, devTag: good},
			failed: "polecat-annotation",
			detail: boom.Error(),
		},
		{
			name:   "the dev-tag read fails",
			probes: podImageLabelProbes{annotation: good, devTag: fail},
			failed: "dev-tag",
			detail: boom.Error(),
		},
		{
			name:   "no annotation reader is wired",
			probes: podImageLabelProbes{devTag: good},
			failed: "polecat-annotation",
			detail: "the run has no Deployment annotation reader",
		},
		{
			name:   "no dev-tag reader is wired",
			probes: podImageLabelProbes{annotation: good},
			failed: "dev-tag",
			detail: "the run has no dev tag reader",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probes := tt.probes
			probes.VerifyProbes = newVerifyProbes(verifyProbeEnv{Exe: verifyProbeExe, Run: (&verifyProbeRun{}).run})
			probes.wantPolecat = want

			checks := update.Verify(ctx, probes, rollbackNewCommit)

			failed := podVerifyCheck(t, checks, tt.failed)
			if failed.OK {
				t.Fatalf("%s passed on a failed read: %+v", tt.failed, failed)
			}
			if failed.Detail != tt.detail {
				t.Errorf("%s detail = %q, want %q", tt.failed, failed.Detail, tt.detail)
			}
			other := "dev-tag"
			if tt.failed == other {
				other = "polecat-annotation"
			}
			if check := podVerifyCheck(t, checks, other); !check.OK {
				t.Errorf("%s failed when only %s did: %+v", other, tt.failed, check)
			}
		})
	}
}

// TestPodTargetPairReaders holds the two readers to the reads they reuse: the
// annotation comes from the swap's running pair command, and the dev tag from
// the command the pair check runs in the daemon pod.
func TestPodTargetPairReaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := testDigestOf(podPairManifest)

	t.Run("the annotation digest is the Deployment's polecat image", func(t *testing.T) {
		t.Parallel()
		kube := &podPairKubectl{image: podSwapDigestRef, polecat: podPairPolecatRef}

		got, err := podTargetAnnotation(podPairTarget(), kube.run)(ctx)
		if err != nil {
			t.Fatalf("annotation digest: %v", err)
		}
		if got != want {
			t.Errorf("annotation digest = %q, want the annotation's digest %q", got, want)
		}
		read := `jsonpath=` + runningPairJSONPath
		if !slices.ContainsFunc(kube.calls, func(c podPairCall) bool { return slices.Contains(c.args, read) }) {
			t.Errorf("ran %+v, want the swap's running pair read", kube.calls)
		}
	})

	t.Run("a Deployment with no polecat annotation is an error", func(t *testing.T) {
		t.Parallel()
		kube := &podPairKubectl{image: podSwapDigestRef}

		if _, err := podTargetAnnotation(podPairTarget(), kube.run)(ctx); err == nil {
			t.Fatal("a Deployment with no polecat annotation reported no error")
		}
	})

	t.Run("a failed pair read is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("kubectl refused")
		kube := &podPairKubectl{runErr: boom}

		if _, err := podTargetAnnotation(podPairTarget(), kube.run)(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the run error", err)
		}
	})

	t.Run("the dev-tag digest is what the daemon pod reads", func(t *testing.T) {
		t.Parallel()
		kube := &podPairKubectl{}

		got, err := podTargetDevTag(podPairTarget(), kube.run)(ctx)
		if err != nil {
			t.Fatalf("dev-tag digest: %v", err)
		}
		if got != want {
			t.Errorf("dev-tag digest = %q, want the tag's digest %q", got, want)
		}
		if !slices.ContainsFunc(kube.calls, func(c podPairCall) bool { return slices.Contains(c.args, "polecat-devtag") }) {
			t.Errorf("ran %+v, want the daemon's polecat-devtag read", kube.calls)
		}
	})

	t.Run("an absent dev tag is an error", func(t *testing.T) {
		t.Parallel()
		kube := &podPairKubectl{devTag: podPairDevTagJSON(false, "")}

		if _, err := podTargetDevTag(podPairTarget(), kube.run)(ctx); err == nil {
			t.Fatal("an absent dev tag reported no error")
		}
	})

	t.Run("a failed dev-tag read is an error carrying no output", func(t *testing.T) {
		t.Parallel()
		kube := &podPairKubectl{devTagExit: 1}

		_, err := podTargetDevTag(podPairTarget(), kube.run)(ctx)
		if err == nil {
			t.Fatal("a failed dev-tag read reported no error")
		}
		if strings.Contains(err.Error(), podSwapOutput) {
			t.Errorf("error %q carries the command's output", err)
		}
	})
}

// TestPodRunVerifyListsThePairChecks drives the probes the command wires for a
// pod target: a pod run's verify result lists the pair's two checks, and they
// pass against the pair the install rolled, read through the fake Deployment and
// daemon pod.
func TestPodRunVerifyListsThePairChecks(t *testing.T) {
	t.Parallel()
	got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
	// The run's Run seam is stubbed, so no patch moved the fake Deployment: put it
	// on the pair the values file names, the state a verify after the install reads.
	got.kube.polecat = podPairPolecatRef

	probes, ok := got.deps.Verify.(podImageLabelProbes)
	if !ok {
		t.Fatalf("verify probe = %T, want the pod runs' probes", got.deps.Verify)
	}
	want := testDigestOf(podPairManifest)
	if digest := probes.WantPolecat(); digest != want {
		t.Errorf("WantPolecat = %q, want the --images file's polecat digest %q", digest, want)
	}
	checks := update.Verify(context.Background(), got.deps.Verify, rollbackNewCommit)
	for _, name := range []string{"polecat-annotation", "dev-tag"} {
		if check := podVerifyCheck(t, checks, name); !check.OK {
			t.Errorf("%s failed for the pair the install rolled: %+v", name, check)
		}
	}
}

// TestPodRunRefusesAMissingImagesFile: a pod target's values file that does not
// read stops the run before any step, so the pair probes never read a pair the
// file does not name.
func TestPodRunRefusesAMissingImagesFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ran := false
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: updateClock{}}, nil
		},
		Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
			ran = true
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		PodRun: (&podDaemonKubectl{}).run,
	}

	_, err := runPodCommand(t, seams, "--images", filepath.Join(townRoot, "missing.yaml"),
		"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
	if err == nil || !strings.Contains(err.Error(), "the --images values file") {
		t.Fatalf("gt update --images error = %v, want the values file refusal", err)
	}
	if ran {
		t.Error("the run started, want the values file refusal before any step")
	}
}

// TestNonPodRunVerifyListsNoPairChecks holds the probes a run without a pod
// target wires, as realUpdateDeps builds them: the pair extension is absent, so
// the verify result is the four checks the town's own reads answer.
func TestNonPodRunVerifyListsNoPairChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	run := &verifyProbeRun{output: "gt version " + rollbackNewCommit + " (dev: main)\n"}
	probes := newVerifyProbes(verifyProbeEnv{
		Exe:          verifyProbeExe,
		Run:          run.run,
		DaemonCommit: func() (string, error) { return rollbackNewCommit, nil },
	})

	if _, ok := probes.(update.PairProbes); ok {
		t.Fatal("the probes a run without a pod target wires implement the pair extension")
	}
	checks := update.Verify(ctx, probes, rollbackNewCommit)
	if len(checks) != 4 {
		t.Fatalf("got %d checks, want the four a non-pod run makes: %+v", len(checks), checks)
	}
	for _, name := range []string{"polecat-annotation", "dev-tag"} {
		for _, check := range checks {
			if check.Name == name {
				t.Errorf("a run without a pod target reported %s", name)
			}
		}
	}
}
