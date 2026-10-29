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
	m := buildTrim(t, trimInput())
	job := m.jobs()[0]
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
	// One input shard: the claim and that shard are the whole of the pod's volumes and mounts.
	if len(pod.Volumes) != 1+len(m.Shards()) || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "gt-cache" {
		t.Fatalf("volumes = %+v, want the gt-cache claim and %d shard", pod.Volumes, len(m.Shards()))
	}
	trim := pod.Containers[0]
	if trim.Image != "gastown-polecat:dev" {
		t.Errorf("image = %q, want the polecat image", trim.Image)
	}
	if len(trim.VolumeMounts) != 1+len(m.Shards()) || trim.VolumeMounts[0].MountPath != SeedCacheRoot ||
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

	trim := m.jobs()[0].Spec.Template.Spec.Containers[0]
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
		if got := m.jobs()[0].Spec.Template.Spec.Volumes[i+1].ConfigMap; got == nil || got.Name != shard.Metadata.Name {
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
	doc := string(m.jobs()[0].JSON())
	if strings.Contains(doc, "gocache/") {
		t.Errorf("the Job document names a planned path")
	}
	argv := 0
	for _, arg := range m.jobs()[0].Spec.Template.Spec.Containers[0].Command {
		argv += len(arg)
	}
	for _, arg := range m.jobs()[0].Spec.Template.Spec.Containers[0].Args {
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
	// Each path is 111 characters but 613 bytes once encoded, since each '<' escapes to six bytes. So
	// 436 of them are 48,396 bytes of path text, under the bound, and 267,705 bytes of encoded list,
	// over it. The setup is checked below, so it cannot drift to a case that does not exercise the bound.
	const count = 436
	paths := make([]string, count)
	for i := range paths {
		paths[i] = fmt.Sprintf("gocache/%03d%s", i, strings.Repeat("<", 100))
	}
	if raw := len(strings.Join(paths, "")); raw > TrimShardBytes {
		t.Fatalf("the paths are %d bytes of text, want under the %d-byte bound", raw, TrimShardBytes)
	}
	if enc, err := json.Marshal(paths); err != nil || len(enc) <= TrimShardBytes {
		t.Fatalf("the paths encode to %d bytes (%v), want over the %d-byte bound", len(enc), err, TrimShardBytes)
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

// shardPerPathListing is n build-cache files each encoding to just over half a shard, so every shard
// holds one path and the shard count is the path count.
func shardPerPathListing(n int) []Entry {
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("gocache/%03d", i) + strings.Repeat("a", TrimShardBytes/2-len("gocache/000"))
		out = append(out, file(p, BuildCache, 1, 0, i))
	}
	return out
}

// TestBuildTrimManifestMountsAListAtTheShardCeilingInOneJob pins the boundary below the split: a list
// that fills exactly TrimMaxShards shards is one Job mounting all of them.
func TestBuildTrimManifestMountsAListAtTheShardCeilingInOneJob(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.Listing = shardPerPathListing(TrimMaxShards)

	m := buildTrim(t, in)
	if len(m.Shards()) != TrimMaxShards || len(m.jobs()) != 1 {
		t.Fatalf("manifest = %d shards and %d Jobs, want %d shards in one Job", len(m.Shards()), len(m.jobs()), TrimMaxShards)
	}
	if got := m.jobs()[0].Metadata.Name; got != DefaultTrimJobName {
		t.Errorf("the Job is named %q, want the name a one-Job trim always had, %q", got, DefaultTrimJobName)
	}
}

// TestBuildTrimManifestSplitsAListPastTheShardCeiling pins the route past one Job's mounts: a list one
// shard longer than TrimMaxShards renders as one Job per run of at most TrimMaxShards shards. Every
// shard is mounted by exactly one Job, the Job's --shards count is its own mount count, and the paths
// reach the containers once each, in the order PlanTrim gave them.
func TestBuildTrimManifestSplitsAListPastTheShardCeiling(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.Listing = shardPerPathListing(TrimMaxShards + 1)
	m := buildTrim(t, in)

	jobs := m.jobs()
	if len(jobs) != 2 {
		t.Fatalf("the manifest has %d Jobs, want 2 for %d shards past one Job's %d mounts", len(jobs), len(m.Shards()), TrimMaxShards)
	}
	var mounted []string
	for i, job := range jobs {
		if want := DefaultTrimJobName + "-" + strconv.Itoa(i); job.Metadata.Name != want {
			t.Errorf("Job %d is named %q, want %q", i, job.Metadata.Name, want)
		}
		pod := job.Spec.Template.Spec
		trim := pod.Containers[0]
		var shardMounts int
		for _, v := range pod.Volumes {
			if v.ConfigMap != nil {
				mounted = append(mounted, v.ConfigMap.Name)
				shardMounts++
			}
		}
		if shardMounts == 0 || shardMounts > TrimMaxShards {
			t.Errorf("Job %d mounts %d shards, want 1 to %d", i, shardMounts, TrimMaxShards)
		}
		if got := trim.Args[len(trim.Args)-1]; got != strconv.Itoa(shardMounts) {
			t.Errorf("Job %d names --shards %s, want its mount count %d", i, got, shardMounts)
		}
		// The helper reads shard k at <TrimListRoot>/k, so each Job's own shards must sit there from 0.
		var listMounts []string
		for _, mt := range trim.VolumeMounts {
			if strings.HasPrefix(mt.MountPath, TrimListRoot+"/") {
				listMounts = append(listMounts, mt.MountPath)
			}
		}
		if len(listMounts) != shardMounts {
			t.Errorf("Job %d has %d list mounts for %d shard volumes, want one mount per shard", i, len(listMounts), shardMounts)
		}
		for k := 0; k < shardMounts; k++ {
			if k >= len(listMounts) || listMounts[k] != TrimListRoot+"/"+strconv.Itoa(k) {
				t.Errorf("Job %d mounts its shards at %v, want %s/0 to %s/%d in order", i, listMounts, TrimListRoot, TrimListRoot, shardMounts-1)
				break
			}
		}
	}

	var shardNames []string
	for _, shard := range m.Shards() {
		shardNames = append(shardNames, shard.Metadata.Name)
	}
	if !reflect.DeepEqual(mounted, shardNames) {
		t.Errorf("the Jobs mount %v, want each of the manifest's shards %v exactly once, in order", mounted, shardNames)
	}

	var want []string
	for _, r := range PlanTrim(in.Listing, in.CapBytes, false).Removals {
		want = append(want, r.Path)
	}
	if got := manifestPaths(t, m); !reflect.DeepEqual(got, want) {
		t.Errorf("the shards list %d paths, want PlanTrim's %d in order", len(got), len(want))
	}

	if docs := m.Documents(); len(docs) != len(m.Shards())+len(jobs) {
		t.Errorf("documents = %d, want one per shard and per Job", len(docs))
	}
}

// TestBuildTrimManifestRefusesASplitJobNamePastTheLabelLimit pins the refusal on the name a split takes:
// a Job's name is a DNS-1123 label, so a name that would pass 63 characters once its run index is added
// is refused rather than rendered.
func TestBuildTrimManifestRefusesASplitJobNamePastTheLabelLimit(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.Listing = shardPerPathListing(TrimMaxShards + 1)

	// "-0" is two characters, so 61 characters is the longest name whose first run still fits.
	in.Name = strings.Repeat("a", 62)
	if m, err := BuildTrimManifest(in); err == nil {
		t.Errorf("BuildTrimManifest with a 62-character name split over two Jobs = %+v, want a refusal", m)
	}

	in.Name = strings.Repeat("a", 61)
	if _, err := BuildTrimManifest(in); err != nil {
		t.Errorf("BuildTrimManifest with a 61-character name split over two Jobs: %v, want the suffixed name to fit", err)
	}
}

// TestTrimRunsRefusesASplitJobNameWithATwoDigitRunIndex pins the name limit at the first run index
// that takes two digits. The runs are rendered from the shard list, so synthetic shards reach run 10
// without building the 80 MB of real list that many shards would take.
func TestTrimRunsRefusesASplitJobNameWithATwoDigitRunIndex(t *testing.T) {
	t.Parallel()
	shards := make([]TrimShard, 10*TrimMaxShards+1)

	// Run 10 takes the suffix "-10", three characters, so 60 characters is the longest name it can take.
	if _, err := trimRuns(trimInput(), strings.Repeat("a", 60), nil, shards); err != nil {
		t.Errorf("trimRuns with a 60-character name over 11 runs: %v, want the suffixed name to fit", err)
	}
	if _, err := trimRuns(trimInput(), strings.Repeat("a", 61), nil, shards); err == nil {
		t.Errorf("trimRuns with a 61-character name over 11 runs, want a refusal at run 10")
	}
}

// TestTrimRunsRefusesAListWithNoShard pins the refusal of a manifest with no Job: a list with no shard
// would apply as a success that trims nothing.
func TestTrimRunsRefusesAListWithNoShard(t *testing.T) {
	t.Parallel()
	if runs, err := trimRuns(trimInput(), DefaultTrimJobName, nil, nil); err == nil {
		t.Errorf("trimRuns with no shards = %d runs, want a refusal", len(runs))
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
			_, annotated := m.jobs()[0].Metadata.Annotations[TrimOverflowAnnotation]
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
	if got := m.jobs()[0].Metadata.Annotations[TrimOverflowAnnotation]; got != "50" {
		t.Errorf("%s = %q, want 50 bytes over the cap", TrimOverflowAnnotation, got)
	}
	if doc := string(m.jobs()[0].JSON()); !strings.Contains(doc, `"`+TrimOverflowAnnotation+`": "50"`) {
		t.Errorf("the document does not carry the overflow annotation: %s", doc)
	}

	// The same listing with no sandbox running loses the module cache too, so nothing overflows.
	in.SandboxesRunning = 0
	m = buildTrim(t, in)
	if _, ok := m.jobs()[0].Metadata.Annotations[TrimOverflowAnnotation]; ok {
		t.Errorf("annotations = %v, want none when the trim reaches the cap", m.jobs()[0].Metadata.Annotations)
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

// TestBuildTrimManifestRefusesAListingUnderAnotherCache pins the check that keeps the plan and the
// argv on one cache: PlanTrim picks removals by Entry.Cache while the container deletes by path, so
// a listing that misnames the directory would have the rules admit one cache's file and the argv
// delete the other's.
func TestBuildTrimManifestRefusesAListingUnderAnotherCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listing []Entry
	}{
		{"a build-cache path labelled as the module cache", []Entry{
			file("gocache/ab/cdef", ModuleCache, 100, 1, 1),
		}},
		{"a module-cache path labelled as the build cache", []Entry{
			file("gomodcache/mod/x", BuildCache, 100, 1, 1),
		}},
		{"a cache the package does not define", []Entry{
			file("gocache/ab/cdef", BuildCache, 100, 8, 8),
			file("gocache/zz", Cache(7), 10, 1, 1),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			in.Listing = tc.listing
			if m, err := BuildTrimManifest(in); err == nil {
				t.Errorf("BuildTrimManifest(%+v) = %+v, want a refusal", in, m)
			}
		})
	}
}

// TestBuildTrimManifestRefusesAClaimThatIsUnderItsCap pins the refusal at the builder, where a caller
// meets it: a claim already under its cap plans no removals, and no manifest (and so no Job) comes
// back for it (gt-284u5).
func TestBuildTrimManifestRefusesAClaimThatIsUnderItsCap(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 1 << 40
	m, err := BuildTrimManifest(in)
	if err == nil {
		t.Fatalf("BuildTrimManifest returned %d Jobs for a claim under its cap, want a refusal", len(m.jobs()))
	}
	if !strings.Contains(err.Error(), "would remove nothing") {
		t.Errorf("error = %q, want it to say the trim would remove nothing", err)
	}
}

// TestTrimManifestExportsNoJobAccessor pins the claim Documents makes: it is the manifest's only
// route to the Jobs, so no caller can apply a Job without its shards. An exported method that returns
// the Jobs would break it (gt-284u5).
func TestTrimManifestExportsNoJobAccessor(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(&TrimManifest{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	if want := []string{"Documents", "Shards"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TrimManifest exports %v, want %v: a Job accessor lets a caller apply a Job without its shards", got, want)
	}
}

// labelSelectorMatches reports whether sel, a comma-joined list of key=value terms that must all
// hold, selects an object carrying labels. An empty selector matches nothing.
func labelSelectorMatches(sel string, labels map[string]string) bool {
	if sel == "" {
		return false
	}
	for _, term := range strings.Split(sel, ",") {
		key, value, ok := strings.Cut(term, "=")
		if !ok || labels[key] != value {
			return false
		}
	}
	return true
}

// TestTrimSelectorMatchesEveryShardAndTheJobs pins the cleanup's selector across a split trim: it
// finds every shard ConfigMap and each Job and its pod template, so deleting by it after the Jobs
// finish leaves no shard behind, whichever run mounted it.
func TestTrimSelectorMatchesEveryShardAndTheJobs(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.Listing = shardPerPathListing(TrimMaxShards + 1)
	m := buildTrim(t, in)

	sel := TrimSelector()
	if len(m.jobs()) < 2 {
		t.Fatalf("the manifest has %d Jobs, want a split trim with at least 2", len(m.jobs()))
	}
	for _, shard := range m.Shards() {
		if !labelSelectorMatches(sel, shard.Metadata.Labels) {
			t.Errorf("selector %q misses shard %s, labelled %v", sel, shard.Metadata.Name, shard.Metadata.Labels)
		}
	}
	for _, job := range m.jobs() {
		if !labelSelectorMatches(sel, job.Metadata.Labels) {
			t.Errorf("selector %q misses Job %s, labelled %v", sel, job.Metadata.Name, job.Metadata.Labels)
		}
		if !labelSelectorMatches(sel, job.Spec.Template.Metadata.Labels) {
			t.Errorf("selector %q misses the pods of Job %s, labelled %v", sel, job.Metadata.Name, job.Spec.Template.Metadata.Labels)
		}
	}
}

// TestTrimSelectorLeavesTheSeedJobAlone pins that the cleanup's selector does not reach the seed
// Job or its pods: a trim's cleanup deletes only what a trim applied.
func TestTrimSelectorLeavesTheSeedJobAlone(t *testing.T) {
	t.Parallel()
	seed, err := BuildSeedJob(SeedJobInput{
		Image:     "gastown-polecat:dev",
		ClaimName: "gt-cache",
		Pins:      []Pin{{Rig: "devops", Commit: sha, URL: "http://forgejo:3000/town/devops.git"}},
	})
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	if labelSelectorMatches(TrimSelector(), seed.Metadata.Labels) {
		t.Errorf("selector %q selects the seed Job, labelled %v", TrimSelector(), seed.Metadata.Labels)
	}
	if labelSelectorMatches(TrimSelector(), seed.Spec.Template.Metadata.Labels) {
		t.Errorf("selector %q selects the seed Job's pods, labelled %v", TrimSelector(), seed.Spec.Template.Metadata.Labels)
	}
}
