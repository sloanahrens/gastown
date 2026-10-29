package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// fakeRegistry serves the registry's v2 API in process from tags and records
// every request path, so a test can assert what a run asked for.
type fakeRegistry struct {
	mu     sync.Mutex
	paths  []string
	repos  map[string][]TagInfo
	status map[string]int    // path prefix -> status code, for a failing read
	bodies map[string]string // digest -> manifest body served in place of the default
}

func newFakeRegistry(repos map[string][]TagInfo) *fakeRegistry {
	return &fakeRegistry{repos: repos, status: map[string]int{}, bodies: map[string]string{}}
}

func (f *fakeRegistry) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

func (f *fakeRegistry) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	for prefix, code := range f.status {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(code)
			return
		}
	}
	switch {
	case r.URL.Path == "/v2/_catalog":
		names := make([]string, 0, len(f.repos))
		for repo := range f.repos {
			names = append(names, repo)
		}
		slices.Sort(names)
		writeJSON(w, map[string][]string{"repositories": names})
	case strings.HasSuffix(r.URL.Path, "/tags/list"):
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
		tags, ok := f.repos[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		names := make([]string, len(tags))
		for i, ti := range tags {
			names[i] = ti.Tag
		}
		writeJSON(w, map[string]any{"name": repo, "tags": names})
	case strings.Contains(r.URL.Path, "/manifests/"):
		repo, ref, ok := splitManifestPath(r.URL.Path)
		ti, found := f.find(repo, ref)
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		if body, custom := f.bodies[ti.Digest]; custom {
			served(w, ti.Digest, body)
			return
		}
		served(w, ti.Digest, imageBody(configOf(ti.Digest), ti.Bytes))
	case strings.Contains(r.URL.Path, "/blobs/"):
		digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		ti, ok := f.findByConfig(digest)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(blobBody(ti.Created.Format(time.RFC3339Nano))))
	default:
		http.NotFound(w, r)
	}
}

// find returns the tag repo serves under ref, by tag or by digest.
func (f *fakeRegistry) find(repo, ref string) (TagInfo, bool) {
	for _, ti := range f.repos[repo] {
		if ti.Tag == ref || ti.Digest == ref {
			return ti, true
		}
	}
	return TagInfo{}, false
}

func (f *fakeRegistry) findByConfig(config string) (TagInfo, bool) {
	for _, tags := range f.repos {
		for _, ti := range tags {
			if configOf(ti.Digest) == config {
				return ti, true
			}
		}
	}
	return TagInfo{}, false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// splitManifestPath splits /v2/<repo>/manifests/<ref> into its repo and ref.
func splitManifestPath(path string) (repo, ref string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/manifests/")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len("/manifests/"):], true
}

// configOf is the config digest the fake serves for a manifest digest, so each
// tag's blob carries that tag's own created time.
func configOf(manifestDigest string) string {
	sum := sha256.Sum256([]byte(manifestDigest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeTag is one tag the fake registry holds.
func fakeTag(repo, tag, digest string, created time.Time, bytes int64) TagInfo {
	return TagInfo{Repo: repo, Tag: tag, Digest: digest, Created: created, Bytes: bytes}
}

// testConfig wires a target to reg, an empty pod list and a records directory
// holding records.
func testConfig(t *testing.T, reg *fakeRegistry, records ...string) Config {
	t.Helper()
	dir := t.TempDir()
	for i, body := range records {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("sandbox-%d.json", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		Base:       "http://registry.test",
		HTTP:       &http.Client{Transport: roundTrip(reg.handler)},
		Kubeconfig: "/kubeconfig",
		RecordsDir: dir,
		Run:        podRunner(`{"items":[]}`),
		Installed:  func() (string, error) { return commit("c000099"), nil },
	}
}

// podRunner answers the kubectl read with pods, the way a running cluster does.
func podRunner(pods string) diskmaint.Runner {
	return func(context.Context, []string, string, ...string) ([]byte, error) {
		return []byte(pods), nil
	}
}

// podUsing names one container image and its imageID in a kubectl reply.
func podUsing(image, imageID string) string {
	return fmt.Sprintf(`{"items":[{"spec":{"containers":[{"image":%q}]},"status":{"containerStatuses":[{"image":%q,"imageID":%q}]}}]}`,
		image, image, imageID)
}

// podsUsing names each image as a container of one pod in a kubectl reply.
func podsUsing(images ...string) string {
	containers := make([]string, len(images))
	for i, image := range images {
		containers[i] = fmt.Sprintf(`{"image":%q}`, image)
	}
	return fmt.Sprintf(`{"items":[{"spec":{"containers":[%s]}}]}`, strings.Join(containers, ","))
}

// clusterImages are images the kind cluster's own pods run. None lives in the
// local registry, and the target cannot delete from their repos.
var clusterImages = []string{
	"registry.k8s.io/etcd:3.7.0-0",
	"docker.io/kindest/kindnetd:v20260820-69b56db7",
	"ghcr.io/nvidia/openshell/gateway:0.1.4",
}

// planTime is the created time of the nth fixture manifest.
func planTime(n int) time.Time {
	return time.Date(2026, 10, 20+n, 9, 0, 0, 0, time.UTC)
}

func TestTargetNameIsRegistry(t *testing.T) {
	t.Parallel()
	var target diskmaint.Target = NewTarget(Config{Base: "http://registry.test"})
	if got := target.Name(); got != "registry" {
		t.Fatalf("Name() = %q, want %q", got, "registry")
	}
}

func TestPlanAssemblesInputAndBuildsPlan(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3, c4 := dg("d0"), dg("a1"), dg("a2"), dg("a3"), dg("a4")
	stray := dg("b1")
	mainTags := []TagInfo{
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
		fakeTag(RepoMain, "c000004", c4, planTime(4), 500),
	}
	strayTags := []TagInfo{fakeTag("seedtest/polecat", "x1", stray, planTime(0), 60)}
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: mainTags, "seedtest/polecat": strayTags})
	cfg := testConfig(t, reg, `{"image":"localhost:5001/seedtest/polecat:x1"}`)
	installed := commit("c000004")
	cfg.Installed = func() (string, error) { return installed, nil }

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := BuildPlan(PlanInput{
		Repos:           map[string][]TagInfo{RepoMain: mainTags, "seedtest/polecat": strayTags},
		Keep:            2,
		InstalledCommit: installed,
		InUseDigests:    map[string]bool{stray: true},
		InUseRepos:      map[string]bool{"seedtest/polecat": true},
	})
	if !plansEqual(plan, want) {
		t.Fatalf("plan = %+v, want BuildPlan of the same data = %+v", plan, want)
	}
}

func TestPlanResolvesInUseTagsToDigests(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}})
	cfg := testConfig(t, reg)
	cfg.Run = podRunner(podUsing(
		"localhost:5001/gastown-polecat:c000001",
		"docker-pullable://localhost:5001/gastown-polecat@"+dg("f0"),
	))

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := actionRefs(plan), []string{mainRef(c2)}; !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want only %v: the tag a pod names keeps its digest", got, want)
	}
	if got := keptRule(plan, RepoMain+":c000001"); got != ruleInUse {
		t.Fatalf("c000001 kept by %q, want %q", got, ruleInUse)
	}
}

func TestPlanRefusesWhenAnInUseTagCannotBeResolved(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}})
	cfg := testConfig(t, reg)
	cfg.Run = podRunner(podUsing("localhost:5001/gastown-polecat:c999999", ""))

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "c999999") {
		t.Fatalf("Refused = %q, want it to name the unresolvable tag", plan.Refused)
	}
}

func TestPlanIgnoresForeignImageRunningOnTheCluster(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}})
	cfg := testConfig(t, reg)
	cfg.Run = podRunner(podsUsing(append([]string{"localhost:5001/gastown-polecat:dev"}, clusterImages...)...))

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("plan refused on the cluster's own images: %s", plan.Refused)
	}
	if keptRule(plan, RepoMain+":"+TagDev) == "" {
		t.Fatalf("dev is not kept; the plan must keep the tag")
	}
	if slices.Contains(actionRefs(plan), mainRef(dev)) {
		t.Fatalf("plan deletes dev %s; the tag must stay", mainRef(dev))
	}
	for _, path := range reg.requested() {
		if path != "/v2/_catalog" && !strings.HasPrefix(path, "/v2/"+RepoMain+"/") {
			t.Fatalf("registry asked about %s; a foreign image must not be read", path)
		}
	}
}

func TestPlanRefusesWhenAnInUseImageDoesNotResolve(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	mainTags := []TagInfo{
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}
	strayTags := []TagInfo{fakeTag("seedtest/polecat", "x1", dg("b1"), planTime(0), 60)}
	cases := []struct {
		name, image, tag string
	}{
		{name: "main repo", image: "localhost:5001/gastown-polecat:c999999", tag: "c999999"},
		{name: "stray repo", image: "localhost:5001/seedtest/polecat:x9", tag: "x9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newFakeRegistry(map[string][]TagInfo{RepoMain: mainTags, "seedtest/polecat": strayTags})
			cfg := testConfig(t, reg)
			cfg.Run = podRunner(podsUsing(append([]string{tc.image}, clusterImages...)...))

			plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 1})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			assertRefused(t, plan)
			if !strings.Contains(plan.Refused, tc.tag) {
				t.Fatalf("Refused = %q, want it to name the unresolvable tag %s", plan.Refused, tc.tag)
			}
		})
	}
}

func TestPlanForeignImageRefAddsNothing(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	repos := map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}}
	baseline, err := NewTarget(testConfig(t, newFakeRegistry(repos))).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatalf("baseline Plan: %v", err)
	}

	reg := newFakeRegistry(repos)
	cfg := testConfig(t, reg)
	cfg.Run = podRunner(podUsing("docker.io/library/gastown-polecat:c000002", ""))
	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !plansEqual(plan, baseline) {
		t.Fatalf("plan = %+v, want the plan without the foreign ref = %+v", plan, baseline)
	}
	for _, path := range reg.requested() {
		if strings.Contains(path, "library/") {
			t.Fatalf("registry asked about %s; a foreign-repo ref must not be resolved", path)
		}
	}
}

func TestPlanSkipsManifestFetchWhenTagCountSmall(t *testing.T) {
	t.Parallel()
	dev, c1, c2 := dg("d0"), dg("a1"), dg("a2")
	reg := newFakeRegistry(map[string][]TagInfo{
		RepoMain: {
			fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
			fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
			fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		},
		"other/repo": {fakeTag("other/repo", "v1", dg("e1"), planTime(0), 10)},
	})

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := (diskmaint.Plan{Target: "registry"}); !plansEqual(plan, want) {
		t.Fatalf("plan = %+v, want an empty plan", plan)
	}
	// The catalog names what exists; the main repo's tags are the only listing
	// read. No manifest and no other repo's tags were requested.
	want := []string{"/v2/_catalog", "/v2/gastown-polecat/tags/list"}
	if got := reg.requested(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestPlanRefusesWhenCatalogFails(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {}})
	reg.status["/v2/_catalog"] = http.StatusInternalServerError

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "catalog") {
		t.Fatalf("Refused = %q, want it to name the catalog read", plan.Refused)
	}
}

func TestPlanRefusesWhenOneManifestFetchFails(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}})
	reg.status["/v2/gastown-polecat/manifests/c000002"] = http.StatusInternalServerError

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "c000002") {
		t.Fatalf("Refused = %q, want it to name the failing manifest", plan.Refused)
	}
}

func TestPlanRefusesWhenInstalledCommitUnreadable(t *testing.T) {
	t.Parallel()
	dev, c1, c2, c3 := dg("d0"), dg("a1"), dg("a2"), dg("a3")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dev, planTime(0), 100),
		fakeTag(RepoMain, "c000001", c1, planTime(1), 200),
		fakeTag(RepoMain, "c000002", c2, planTime(2), 300),
		fakeTag(RepoMain, "c000003", c3, planTime(3), 400),
	}})
	cfg := testConfig(t, reg)
	cfg.Installed = func() (string, error) { return "", errors.New("gt version reported nothing") }

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "installed") {
		t.Fatalf("Refused = %q, want it to name the installed commit read", plan.Refused)
	}
}

func TestMeasureReportsTagCountAndDetail(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(map[string][]TagInfo{
		RepoMain: {
			fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
			fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
		},
		"seedtest/polecat": {fakeTag("seedtest/polecat", "x1", dg("b1"), planTime(0), 300)},
	})

	m, err := NewTarget(testConfig(t, reg)).Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if want := int64(600); m.Used != want {
		t.Errorf("Used = %d, want %d bytes of manifests", m.Used, want)
	}
	if m.Capacity != 0 {
		t.Errorf("Capacity = %d, want 0 for a registry that shares the town's disk", m.Capacity)
	}
	for _, want := range []string{"3 tags", "600 bytes"} {
		if !strings.Contains(m.Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", m.Detail, want)
		}
	}
}

// threeCandidates is a main repo with one candidate more than the Keep+1 tags
// that would let a run skip its manifest reads.
func threeCandidates() map[string][]TagInfo {
	return map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
		fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
		fakeTag(RepoMain, "c000002", dg("a2"), planTime(2), 300),
		fakeTag(RepoMain, "c000003", dg("a3"), planTime(3), 400),
	}}
}

func TestPlanFetchesManifestsForAStrayRepoWhenMainIsSmall(t *testing.T) {
	t.Parallel()
	stray := dg("b1")
	reg := newFakeRegistry(map[string][]TagInfo{
		RepoMain: {
			fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
			fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
		},
		"seedtest/polecat": {fakeTag("seedtest/polecat", "x1", stray, planTime(0), 60)},
	})

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := actionRefs(plan), []string{"seedtest/polecat@" + stray}; !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v: a stray repo is deleted whatever the main repo holds", got, want)
	}
}

func TestPlanRefusesWhenASmallRepoHasNoAnchor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []TagInfo
		want string
	}{
		{"no tags", nil, "no tags"},
		{"no dev tag", []TagInfo{fakeTag(RepoMain, "c000001", dg("a1"), planTime(0), 200)}, TagDev},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newFakeRegistry(map[string][]TagInfo{RepoMain: tc.tags})
			plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			assertRefused(t, plan)
			if !strings.Contains(plan.Refused, tc.want) {
				t.Fatalf("Refused = %q, want it to name %q", plan.Refused, tc.want)
			}
		})
	}
}

func TestPlanRefusesWhenAStrayListingFails(t *testing.T) {
	t.Parallel()
	repos := threeCandidates()
	repos["seedtest/polecat"] = []TagInfo{fakeTag("seedtest/polecat", "x1", dg("b1"), planTime(0), 60)}
	reg := newFakeRegistry(repos)
	reg.status["/v2/seedtest/polecat/tags/list"] = http.StatusInternalServerError

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "seedtest/polecat") {
		t.Fatalf("Refused = %q, want it to name the repo whose tags failed", plan.Refused)
	}
}

func TestPlanRefusesWhenTheMainRepoListingFails(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(threeCandidates())
	reg.status["/v2/gastown-polecat/tags/list"] = http.StatusInternalServerError

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, RepoMain) {
		t.Fatalf("Refused = %q, want it to name the failed repo read", plan.Refused)
	}
}

func TestMeasureCountsAManifestOnceWhenTagsShareIt(t *testing.T) {
	t.Parallel()
	shared := dg("a1")
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, shared, planTime(0), 100),
		fakeTag(RepoMain, "c000001", shared, planTime(1), 100),
		fakeTag(RepoMain, "c000002", dg("a2"), planTime(2), 200),
	}})

	m, err := NewTarget(testConfig(t, reg)).Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if want := int64(300); m.Used != want {
		t.Errorf("Used = %d, want %d: two tags naming one manifest hold it once", m.Used, want)
	}
	if !strings.Contains(m.Detail, "3 tags") {
		t.Errorf("Detail = %q, want it to count all 3 tags", m.Detail)
	}
}

func TestPlanRefusesWhenASmallRegistrysSafetyReadsFail(t *testing.T) {
	t.Parallel()
	// Two tags are under the Keep+1 window for keep 2, so this run fetches no
	// manifest; the installed and in-use reads still run and still refuse.
	small := map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
		fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
	}}
	cases := []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"installed unreadable", func(cfg *Config) {
			cfg.Installed = func() (string, error) { return "", errors.New("gt version reported nothing") }
		}, "installed"},
		{"installed reader missing", func(cfg *Config) { cfg.Installed = nil }, "installed"},
		{"in-use unreadable", func(cfg *Config) {
			cfg.Run = func(context.Context, []string, string, ...string) ([]byte, error) {
				return nil, errors.New("kubectl: no such cluster")
			}
		}, "kubectl"},
		{"in-use runner missing", func(cfg *Config) { cfg.Run = nil }, "runner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t, newFakeRegistry(small))
			tc.set(&cfg)
			plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 2})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			assertRefused(t, plan)
			if !strings.Contains(plan.Refused, tc.want) {
				t.Fatalf("Refused = %q, want it to name %q", plan.Refused, tc.want)
			}
		})
	}
}

func TestMeasureFailsWhenAReadFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"catalog", "/v2/_catalog"},
		{"tags", "/v2/gastown-polecat/tags/list"},
		{"manifest", "/v2/gastown-polecat/manifests/c000002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newFakeRegistry(threeCandidates())
			reg.status[tc.path] = http.StatusInternalServerError
			if _, err := NewTarget(testConfig(t, reg)).Measure(context.Background()); err == nil {
				t.Fatalf("Measure returned no error for the %s read it could not make", tc.name)
			}
		})
	}
}

func TestPlanRefusesWhenTheInUseReadFails(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, newFakeRegistry(threeCandidates()))
	cfg.Run = func(context.Context, []string, string, ...string) ([]byte, error) {
		return nil, errors.New("kubectl: no such cluster")
	}

	plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "kubectl") {
		t.Fatalf("Refused = %q, want it to name the in-use read", plan.Refused)
	}
}

func TestPlanRefusesWhenAReaderIsNotConfigured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"installed", func(cfg *Config) { cfg.Installed = nil }, "installed"},
		{"runner", func(cfg *Config) { cfg.Run = nil }, "runner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t, newFakeRegistry(threeCandidates()))
			tc.set(&cfg)
			plan, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Keep: 2})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			assertRefused(t, plan)
			if !strings.Contains(plan.Refused, tc.want) {
				t.Fatalf("Refused = %q, want it to name the missing %s reader", plan.Refused, tc.want)
			}
		})
	}
}

func TestPlanKeepFallsBackToTheConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfgKeep int
		policy  diskmaint.Policy
		want    []string
	}{
		{"policy keep wins over an unset config", 0, diskmaint.Policy{Keep: 2}, []string{mainRef(dg("a1"))}},
		{"policy keep wins over a different config", 1, diskmaint.Policy{Keep: 2}, []string{mainRef(dg("a1"))}},
		{"config keep fills in", 2, diskmaint.Policy{}, []string{mainRef(dg("a1"))}},
		{"both unset keep nothing", 0, diskmaint.Policy{}, []string{mainRef(dg("a1")), mainRef(dg("a2")), mainRef(dg("a3"))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t, newFakeRegistry(threeCandidates()))
			cfg.Keep = tc.cfgKeep
			plan, err := NewTarget(cfg).Plan(context.Background(), tc.policy)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if got := actionRefs(plan); !slices.Equal(got, tc.want) {
				t.Fatalf("actions = %v, want %v", got, tc.want)
			}
		})
	}
}

// unreadableBody is a manifest whose config blob the fake does not serve: the
// manifest answers, and GetManifest fails on the blob read.
func unreadableBody() string { return imageBody(digestOf('e'), 60) }

// unreadableTag is a tag whose manifest answers but cannot be read in full. Its
// digest is the sha256 of the body, as a real registry's is, so ManifestDigest
// reads it; reg serves the body under that digest.
func unreadableTag(reg *fakeRegistry, repo, tag string, created time.Time) TagInfo {
	body := unreadableBody()
	ti := fakeTag(repo, tag, bodyDigest(body), created, 0)
	reg.bodies[ti.Digest] = body
	return ti
}

// actionFor returns the action that deletes ref, and whether the plan has one.
func actionFor(p diskmaint.Plan, ref string) (diskmaint.Action, bool) {
	for _, a := range p.Actions {
		if a.Ref == ref {
			return a, true
		}
	}
	return diskmaint.Action{}, false
}

// strayIndexWithFailingChild seeds a stray tag whose index answers and hashes
// to its digest, while its one child fails to read: GetManifest fails on the
// child, and ManifestDigest still names the index.
func strayIndexWithFailingChild(reg *fakeRegistry) TagInfo {
	child := digestOf('7')
	body := indexBody(child)
	ti := fakeTag(straySeed, "x2", bodyDigest(body), planTime(0), 0)
	reg.bodies[ti.Digest] = body
	reg.status["/v2/"+straySeed+"/manifests/"+child] = http.StatusInternalServerError
	return ti
}

func TestPlanDeletesAnUnreadableStrayManifest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		seed func(reg *fakeRegistry) TagInfo
	}{
		{"config blob missing", func(reg *fakeRegistry) TagInfo {
			return unreadableTag(reg, straySeed, "x1", planTime(0))
		}},
		{"index child fails", strayIndexWithFailingChild},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
				fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
				fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
			}})
			bad := tc.seed(reg)
			reg.repos[straySeed] = []TagInfo{bad}

			plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.Refused != "" {
				t.Fatalf("plan refused: %s: an unreadable stray manifest must not refuse the registry plan", plan.Refused)
			}
			ref := straySeed + "@" + bad.Digest
			a, ok := actionFor(plan, ref)
			if !ok {
				t.Fatalf("actions = %v, want a delete of %s", actionRefs(plan), ref)
			}
			if a.Kind != "delete-manifest" {
				t.Errorf("kind = %q, want delete-manifest", a.Kind)
			}
			if a.Bytes != 0 {
				t.Errorf("bytes = %d, want 0: the size of an unreadable manifest is unknown", a.Bytes)
			}
			if !strings.Contains(a.Why, "GetManifest failed") {
				t.Errorf("why = %q, want it to say GetManifest failed", a.Why)
			}
		})
	}
}

func TestPlanRefusesAnUnreadableMainManifest(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(map[string][]TagInfo{})
	reg.repos[RepoMain] = []TagInfo{
		fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
		fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
		fakeTag(RepoMain, "c000002", dg("a2"), planTime(2), 300),
		unreadableTag(reg, RepoMain, "c000003", planTime(3)),
	}

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	if !strings.Contains(plan.Refused, "c000003") {
		t.Fatalf("Refused = %q, want it to name the unreadable main manifest", plan.Refused)
	}
}

func TestPlanKeepsAnUnreadableStrayNamedByARecord(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
		fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
	}})
	// A record names seedtest/polecat:p1, which reads fine, so the in-use resolve
	// succeeds and the repo is kept whole, unreadable x1 included.
	reg.repos[straySeed] = []TagInfo{
		fakeTag(straySeed, "p1", dg("bbbb0001"), planTime(1), 60),
		unreadableTag(reg, straySeed, "x1", planTime(0)),
	}
	record := `{"image":"localhost:5001/seedtest/polecat:p1"}`

	plan, err := NewTarget(testConfig(t, reg, record)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("plan refused: %s", plan.Refused)
	}
	for _, ref := range actionRefs(plan) {
		if strings.HasPrefix(ref, straySeed+"@") {
			t.Fatalf("actions = %v: a stray repo a sandbox record names must not be deleted", actionRefs(plan))
		}
	}
	if rule := keptRule(plan, straySeed+":x1"); rule != ruleSandboxRecord {
		t.Fatalf("x1 kept by %q, want %q: %v", rule, ruleSandboxRecord, plan.Kept)
	}
}

func TestPlanRefusesWhenStrayDigestAlsoFails(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(map[string][]TagInfo{RepoMain: {
		fakeTag(RepoMain, TagDev, dg("d0"), planTime(0), 100),
		fakeTag(RepoMain, "c000001", dg("a1"), planTime(1), 200),
	}})
	// The header names dg("b1"), which is not the sha256 of the body, so
	// ManifestDigest refuses too: the plan must name both failures.
	bad := fakeTag(straySeed, "x1", dg("b1"), planTime(0), 0)
	reg.repos[straySeed] = []TagInfo{bad}
	reg.bodies[bad.Digest] = unreadableBody()

	plan, err := NewTarget(testConfig(t, reg)).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertRefused(t, plan)
	for _, want := range []string{straySeed + ":x1", "digest fallback"} {
		if !strings.Contains(plan.Refused, want) {
			t.Fatalf("Refused = %q, want it to name %q", plan.Refused, want)
		}
	}
}

// plansEqual compares two plans field by field; a plain DeepEqual on the
// nil-against-empty slices a refusal produces would fail on nothing.
func plansEqual(a, b diskmaint.Plan) bool {
	return a.Target == b.Target && a.Refused == b.Refused &&
		slices.Equal(a.Actions, b.Actions) && slices.Equal(a.Kept, b.Kept)
}
