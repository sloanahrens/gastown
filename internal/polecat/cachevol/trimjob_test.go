package cachevol

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// trimInput is an input that builds: one build-cache file over the cap, the tests vary one field.
func trimInput() TrimJobInput {
	return TrimJobInput{
		Namespace:        "gt",
		Image:            "gastown-polecat:dev",
		ClaimName:        "gt-cache",
		Listing:          []Entry{file("gocache/ab/cdef", BuildCache, 100, 8, 8)},
		CapBytes:         50,
		SandboxesRunning: 0,
	}
}

// buildTrim builds in and returns the manifest, failing the test when the builder refuses it.
func buildTrim(t *testing.T, in TrimJobInput) *TrimManifest {
	t.Helper()
	m, err := BuildTrimManifest(in)
	if err != nil {
		t.Fatalf("BuildTrimManifest: %v", err)
	}
	return m
}

// shardPaths decodes the removal list one shard carries.
func shardPaths(t *testing.T, shard TrimShard) []string {
	t.Helper()
	var paths []string
	if err := json.Unmarshal([]byte(shard.Data[TrimShardKey]), &paths); err != nil {
		t.Fatalf("the shard %s is not a JSON array of paths: %v", shard.Metadata.Name, err)
	}
	return paths
}

// manifestPaths is every path the manifest's shards list, in the order the container reads them.
func manifestPaths(t *testing.T, m *TrimManifest) []string {
	t.Helper()
	var out []string
	for _, shard := range m.Shards() {
		out = append(out, shardPaths(t, shard)...)
	}
	return out
}

// TestBuildTrimManifestRunsAsTheCacheUserWithTheClaimAtCache pins the Job's identity and its mount:
// the pod runs as SeedUserID, the user the seed handed the claim to, so the Job can remove only what
// a sandbox could write, and the claim is mounted at SeedCacheRoot beside the list.
func TestBuildTrimManifestRunsAsTheCacheUserWithTheClaimAtCache(t *testing.T) {
	t.Parallel()
	job := buildTrim(t, trimInput()).Job()
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Errorf("apiVersion/kind = %q/%q, want batch/v1 and Job", job.APIVersion, job.Kind)
	}
	if job.Metadata.Name != DefaultTrimJobName || job.Metadata.Namespace != "gt" {
		t.Errorf("metadata = %+v, want %s in gt", job.Metadata, DefaultTrimJobName)
	}
	if job.Metadata.Labels["app.kubernetes.io/name"] != "polecat-cache-trim" {
		t.Errorf("labels = %v, want the trim Job's name label", job.Metadata.Labels)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed trim reports itself once", job.Spec.BackoffLimit)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never", got)
	}
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != SeedUserID || sc.RunAsGroup == nil || *sc.RunAsGroup != SeedUserID {
		t.Fatalf("pod securityContext = %+v, want runAsUser and runAsGroup %d", sc, SeedUserID)
	}
	// The rendered document is where the absence matters: a container that names its own identity
	// would override the pod's.
	if n := strings.Count(string(job.JSON()), `"securityContext"`); n != 1 {
		t.Errorf("the document carries %d securityContext blocks, want the pod's alone", n)
	}

	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want the trim container alone", len(pod.Containers))
	}
	if len(pod.Volumes) < 1 || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "gt-cache" {
		t.Fatalf("volumes = %+v, want the gt-cache claim alone", pod.Volumes)
	}
	trim := pod.Containers[0]
	if trim.Image != "gastown-polecat:dev" {
		t.Errorf("image = %q, want the polecat image", trim.Image)
	}
	if len(trim.VolumeMounts) < 1 || trim.VolumeMounts[0].MountPath != SeedCacheRoot ||
		trim.VolumeMounts[0].Name != "cache" {
		t.Errorf("mounts = %+v, want the cache volume at %s", trim.VolumeMounts, SeedCacheRoot)
	}
	if !strings.Contains(string(job.JSON()), `"runAsUser": 10001`) {
		t.Errorf("the document does not carry the cache user: %s", job.JSON())
	}
}

// TestBuildTrimManifestCarriesTheRemovalsInTheMountedShards pins the route the removal list takes:
// the shards hold PlanTrim's paths and nothing else, the container's argv names no path at all, and
// the helper reads the list from the mount the Job declares.
func TestBuildTrimManifestCarriesTheRemovalsInTheMountedShards(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.Listing = []Entry{
		file("gocache/oldest", BuildCache, 100, 1, 1),
		file("gocache/older", BuildCache, 100, 2, 2),
		file("gocache/newest", BuildCache, 100, 9, 9),
		file("gomodcache/mod", ModuleCache, 100, 0, 0),
	}
	in.CapBytes = 250
	in.SandboxesRunning = 2
	m := buildTrim(t, in)

	// 400 bytes over a 250-byte cap, so the two least recently used build-cache files go and the
	// module cache stays, since a sandbox is running.
	want := removedPaths(PlanTrim(in.Listing, in.CapBytes, true))
	if got := manifestPaths(t, m); !reflect.DeepEqual(got, want) {
		t.Errorf("the shards list %v, want PlanTrim's removals %v", got, want)
	}

	trim := m.Job().Spec.Template.Spec.Containers[0]
	if want := []string{HelperPath, "trim-paths"}; !reflect.DeepEqual(trim.Command, want) {
		t.Errorf("command = %v, want the image's trim-paths helper", trim.Command)
	}
	shards := strconv.Itoa(len(m.Shards()))
	if want := []string{"--root", SeedCacheRoot, "--list", TrimListRoot, "--shards", shards}; !reflect.DeepEqual(trim.Args, want) {
		t.Errorf("args = %v, want %v", trim.Args, want)
	}
	for _, arg := range trim.Args {
		if strings.Contains(arg, "gocache/") {
			t.Errorf("argv names a planned path: %v", trim.Args)
		}
	}
	for i, shard := range m.Shards() {
		mount := trim.VolumeMounts[i+1]
		if want := TrimListRoot + "/" + strconv.Itoa(i); mount.MountPath != want || !mount.ReadOnly {
			t.Errorf("mount %d = %+v, want the shard read-only at %s", i, mount, want)
		}
		if got := m.Job().Spec.Template.Spec.Volumes[i+1].ConfigMap; got == nil || got.Name != shard.Metadata.Name {
			t.Errorf("volume %d mounts %+v, want the shard %s", i, got, shard.Metadata.Name)
		}
	}
}

// TestBuildTrimManifestCarriesAListTooLongForOneArgv is the bead's goal: a plan of tens of thousands
// of paths, past the kernel's limit on one exec, reaches the container whole through the mounts.
func TestBuildTrimManifestCarriesAListTooLongForOneArgv(t *testing.T) {
	t.Parallel()
	const count = 60000
	listing := make([]Entry, 0, count)
	for i := 0; i < count; i++ {
		listing = append(listing, file(fmt.Sprintf("gocache/%02d/%059d", i%97, i), BuildCache, 1, 0, i))
	}
	in := trimInput()
	in.Listing = listing
	in.CapBytes = 0
	m := buildTrim(t, in)

	plan := PlanTrim(listing, 0, false)
	if len(plan.Removals) != count {
		t.Fatalf("the plan removes %d of %d files, want every one", len(plan.Removals), count)
	}
	if got := manifestPaths(t, m); !reflect.DeepEqual(got, removedPaths(plan)) {
		t.Errorf("the shards list %d paths, want the plan's %d in order", len(got), len(plan.Removals))
	}
	if len(m.Shards()) < 2 {
		t.Errorf("the manifest carries %d shard, want the list split over several", len(m.Shards()))
	}
	for _, shard := range m.Shards() {
		if n := len(shard.Data[TrimShardKey]); n > TrimShardBytes {
			t.Errorf("the shard %s holds %d bytes, want at most %d", shard.Metadata.Name, n, TrimShardBytes)
		}
	}

	// The Job document carries the list nowhere: not a path, not an rm argv.
	doc := string(m.Job().JSON())
	if strings.Contains(doc, "gocache/") {
		t.Errorf("the Job document names a planned path")
	}
	argv := 0
	for _, arg := range m.Job().Spec.Template.Spec.Containers[0].Command {
		argv += len(arg)
	}
	for _, arg := range m.Job().Spec.Template.Spec.Containers[0].Args {
		argv += len(arg)
	}
	if argv > 4096 {
		t.Errorf("the argv is %d bytes, want a fixed one clear of the kernel's limit", argv)
	}
}

// TestTrimShardsBoundTheEncodedList pins the size a shard is measured on: a path character the
// encoder escapes costs six bytes, so a shard bounded on raw path text would cross the ConfigMap
// cap it is there to stay under.
func TestTrimShardsBoundTheEncodedList(t *testing.T) {
	t.Parallel()
	// Each path is 104 characters but 626 bytes of encoded list, so 436 of them are 45 KiB of path
	// text and 273 KiB of encoded list: under the bound by the text, over it by the list.
	const count = 436
	paths := make([]string, count)
	for i := range paths {
		paths[i] = fmt.Sprintf("gocache/%03d%s", i, strings.Repeat("<", 100))
	}
	shards := trimShards(DefaultTrimJobName, "gt", paths)
	if len(shards) < 2 {
		t.Errorf("the list is encoded over %d bytes and took %d shard, want it split", TrimShardBytes, len(shards))
	}
	for _, shard := range shards {
		if n := len(shard.Data[TrimShardKey]); n > TrimShardBytes {
			t.Errorf("the shard %s holds %d bytes, want at most %d", shard.Metadata.Name, n, TrimShardBytes)
		}
	}
}

// TestTrimShardsGiveAnOversizedEntryItsOwnShard pins the one case where a shard is over the bound: an
// entry that alone encodes past it cannot be split, and the run still carries it.
func TestTrimShardsGiveAnOversizedEntryItsOwnShard(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", TrimShardBytes+1)
	shards := trimShards(DefaultTrimJobName, "gt", []string{"gocache/a", long, "gocache/b"})
	if len(shards) != 3 {
		t.Fatalf("shards = %d, want the oversized entry alone in one of three", len(shards))
	}
	if got := shardPaths(t, shards[1]); !reflect.DeepEqual(got, []string{long}) {
		t.Errorf("the middle shard lists %d paths, want the oversized entry alone", len(got))
	}
}

// TestBuildTrimManifestAppliesTheShardsBeforeTheJob pins the manifest's apply order and that the two
// halves agree: every shard it returns is mounted by the Job it returns, so a caller applying the
// documents in order can never leave a shard behind.
func TestBuildTrimManifestAppliesTheShardsBeforeTheJob(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.Listing = []Entry{
		file("gocache/a", BuildCache, 100, 1, 1),
		file("gocache/b", BuildCache, 100, 2, 2),
	}
	in.CapBytes = 50
	m := buildTrim(t, in)

	docs := m.Documents()
	if len(docs) != len(m.Shards())+1 {
		t.Fatalf("documents = %d, want one per shard and the Job", len(docs))
	}
	for i, doc := range docs[:len(docs)-1] {
		var shard TrimShard
		if err := json.Unmarshal(doc, &shard); err != nil {
			t.Fatalf("document %d is not a ConfigMap: %v", i, err)
		}
		if shard.Kind != "ConfigMap" || shard.Metadata.Name != TrimShardName(DefaultTrimJobName, i) {
			t.Errorf("document %d = %s %s, want shard %d of the Job", i, shard.Kind, shard.Metadata.Name, i)
		}
		if shard.Metadata.Labels["app.kubernetes.io/name"] != "polecat-cache-trim" {
			t.Errorf("document %d labels = %v, want the trim labels a cleanup selects", i, shard.Metadata.Labels)
		}
	}
	var job TrimJob
	if err := json.Unmarshal(docs[len(docs)-1], &job); err != nil {
		t.Fatalf("the last document is not the Job: %v", err)
	}
	if job.Kind != "Job" {
		t.Errorf("the last document is %s, want the Job: the pod cannot mount a shard applied after it", job.Kind)
	}

	mounted := make(map[string]bool)
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.ConfigMap != nil {
			mounted[volume.ConfigMap.Name] = true
		}
	}
	for _, shard := range m.Shards() {
		if !mounted[shard.Metadata.Name] {
			t.Errorf("the shard %s is in the manifest but no volume mounts it", shard.Metadata.Name)
		}
	}
	if len(mounted) != len(m.Shards()) {
		t.Errorf("the Job mounts %d shards, want the %d the manifest returns", len(mounted), len(m.Shards()))
	}
}

// TestBuildTrimManifestTakesTheSandboxCountFromItsCaller pins the wiring of PlanTrim's idle rule: the
// count the caller passed decides whether the module cache may go, and the Job itself asks nothing.
func TestBuildTrimManifestTakesTheSandboxCountFromItsCaller(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		running  int
		wantMod  bool
		wantOver bool
	}{
		// 200 bytes of build cache and 100 of module cache under a 50-byte cap: the build cache
		// alone cannot reach the cap, so the module cache goes only when no sandbox runs.
		{name: "idle", running: 0, wantMod: true},
		{name: "one running", running: 1, wantOver: true},
		{name: "three running", running: 3, wantOver: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			in.Listing = []Entry{
				file("gocache/a", BuildCache, 100, 1, 1),
				file("gocache/b", BuildCache, 100, 2, 2),
				file("gomodcache/m", ModuleCache, 100, 3, 3),
			}
			in.CapBytes = 50
			in.SandboxesRunning = tc.running
			m := buildTrim(t, in)
			want := removedPaths(PlanTrim(in.Listing, in.CapBytes, tc.running > 0))
			if got := manifestPaths(t, m); !reflect.DeepEqual(got, want) {
				t.Errorf("the shards list %v, want PlanTrim's removals %v", got, want)
			}
			if got := hasModulePath(manifestPaths(t, m)); got != tc.wantMod {
				t.Errorf("the list names the module cache = %v, want %v", got, tc.wantMod)
			}
			_, annotated := m.Job().Metadata.Annotations[TrimOverflowAnnotation]
			if annotated != tc.wantOver {
				t.Errorf("overflow annotation present = %v, want %v", annotated, tc.wantOver)
			}
		})
	}
}

// TestBuildTrimManifestAnnotatesTheOverflow pins the byte count the rules hand the caller: the Job says
// how far over the cap the claim stays, so a Job read without the caller's log still explains
// itself, and a trim that reaches the cap carries no annotation.
func TestBuildTrimManifestAnnotatesTheOverflow(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.Listing = []Entry{
		file("gocache/a", BuildCache, 100, 1, 1),
		file("gomodcache/m", ModuleCache, 100, 2, 2),
	}
	in.CapBytes = 50
	in.SandboxesRunning = 1
	m := buildTrim(t, in)
	if got := m.Job().Metadata.Annotations[TrimOverflowAnnotation]; got != "50" {
		t.Errorf("%s = %q, want 50 bytes over the cap", TrimOverflowAnnotation, got)
	}
	if doc := string(m.Job().JSON()); !strings.Contains(doc, `"`+TrimOverflowAnnotation+`": "50"`) {
		t.Errorf("the document does not carry the overflow annotation: %s", doc)
	}

	// The same listing with no sandbox running loses the module cache too, so nothing overflows.
	in.SandboxesRunning = 0
	m = buildTrim(t, in)
	if _, ok := m.Job().Metadata.Annotations[TrimOverflowAnnotation]; ok {
		t.Errorf("annotations = %v, want none when the trim reaches the cap", m.Job().Metadata.Annotations)
	}
}

// hasModulePath reports whether a removal list names a module-cache file.
func hasModulePath(paths []string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, SeedModuleCacheDir+"/") {
			return true
		}
	}
	return false
}

// TestBuildTrimManifestRefusesAnInputItCannotRender pins each refusal: a name, namespace, image or
// claim Kubernetes would reject, a cap or count that is not a size, a listing with nothing to remove,
// and a path that could name a file outside the claim.
func TestBuildTrimManifestRefusesAnInputItCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*TrimJobInput)
	}{
		{"a name that is not a label", func(in *TrimJobInput) { in.Name = "gt-cache-trim/1" }},
		{"a namespace that is not a label", func(in *TrimJobInput) { in.Namespace = "gt cache" }},
		{"no image", func(in *TrimJobInput) { in.Image = "" }},
		{"an image with a space", func(in *TrimJobInput) { in.Image = "gastown polecat:dev" }},
		{"no claim", func(in *TrimJobInput) { in.ClaimName = "" }},
		{"a negative cap", func(in *TrimJobInput) { in.CapBytes = -1 }},
		{"a negative sandbox count", func(in *TrimJobInput) { in.SandboxesRunning = -1 }},
		{"nothing listed", func(in *TrimJobInput) { in.Listing = nil }},
		{"a listing under the cap", func(in *TrimJobInput) { in.CapBytes = 1000 }},
		{"an absolute path", func(in *TrimJobInput) {
			in.Listing = []Entry{file("/etc/passwd", BuildCache, 100, 1, 1)}
		}},
		{"an absolute path under the mount", func(in *TrimJobInput) {
			in.Listing = []Entry{file(SeedCacheRoot+"/gocache/a", BuildCache, 100, 1, 1)}
		}},
		{"a path above the claim root", func(in *TrimJobInput) {
			in.Listing = []Entry{file("../gocache/a", BuildCache, 100, 1, 1)}
		}},
		{"the claim root itself", func(in *TrimJobInput) {
			in.Listing = []Entry{file(".", BuildCache, 100, 1, 1)}
		}},
		{"an unclean path", func(in *TrimJobInput) {
			in.Listing = []Entry{file("gocache//a", BuildCache, 100, 1, 1)}
		}},
		{"a path with a current-directory element", func(in *TrimJobInput) {
			in.Listing = []Entry{file("gocache/./a", BuildCache, 100, 1, 1)}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			tc.edit(&in)
			if m, err := BuildTrimManifest(in); err == nil {
				t.Errorf("BuildTrimManifest(%+v) = %+v, want a refusal", in, m)
			}
		})
	}
}
