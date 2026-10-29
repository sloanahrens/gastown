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

// halfShardPaths is n paths each encoding to just over half a shard, so every shard holds one path
// and the shard count is the path count.
func halfShardPaths(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("gocache/%03d", i)+strings.Repeat("a", TrimShardBytes/2-len("gocache/000")))
	}
	return out
}

// shardPerPathListing is halfShardPaths as a listing PlanTrim removes whole: the cap is the caller's
// to set to zero.
func shardPerPathListing(n int) []Entry {
	paths := halfShardPaths(n)
	out := make([]Entry, 0, n)
	for i, p := range paths {
		out = append(out, file(p, BuildCache, 1, 0, i))
	}
	return out
}

// TestBuildTrimManifestMountsAListAtTheShardCeilingInOneJob pins the boundary below the claim: a list
// that fills exactly TrimMaxShards shards is one Job mounting all of them.
func TestBuildTrimManifestMountsAListAtTheShardCeilingInOneJob(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.Listing = shardPerPathListing(TrimMaxShards)

	m := buildTrim(t, in)
	if m.Route() != TrimRouteShards || len(m.Shards()) != TrimMaxShards || len(m.jobs()) != 1 {
		t.Fatalf("manifest = route %v, %d shards and %d Jobs, want %d shards in one Job", m.Route(), len(m.Shards()), len(m.jobs()), TrimMaxShards)
	}
	if got := m.jobs()[0].Metadata.Name; got != DefaultTrimJobName {
		t.Errorf("the Job is named %q, want the name a trim always had, %q", got, DefaultTrimJobName)
	}
}

// TestTrimShardCountMatchesTheShardsItCounts pins the count the route is decided on against the
// shards the shard route applies, over the packings the boundary meets: a count that disagreed with
// trimShards would route to the claim a list one set of shards could still carry, or the other way
// round.
func TestTrimShardCountMatchesTheShardsItCounts(t *testing.T) {
	t.Parallel()
	sizes := []int{0, 1, 2, 3, TrimMaxShards - 1, TrimMaxShards, TrimMaxShards + 1, 2 * TrimMaxShards}
	for _, n := range sizes {
		paths := halfShardPaths(n)
		if got, want := trimShardCount(paths), len(trimShards(DefaultTrimJobName, "gt", paths)); got != want {
			t.Errorf("trimShardCount of %d paths = %d, want the %d shards trimShards makes", len(paths), got, want)
		}
	}
	// A path that alone exceeds a shard takes one of its own, and the packing has to count it so.
	oversized := []string{"gocache/a", strings.Repeat("a", TrimShardBytes+1), "gocache/b"}
	if got, want := trimShardCount(oversized), len(trimShards(DefaultTrimJobName, "gt", oversized)); got != want {
		t.Errorf("trimShardCount of an oversized entry = %d, want the %d shards trimShards makes", got, want)
	}
}

// TestBuildTrimManifestRoutesOnTheShardCount pins where the route changes. The route is decided on the
// ConfigMaps the shard route would need, in shards rather than in bytes: a list that packs into
// exactly TrimMaxShards shards stays in ConfigMaps, and one shard more takes the claim. So no trim
// splits over several Jobs, and the Kubernetes objects a trim applies stop growing with its list
// (gt-2rq36).
func TestBuildTrimManifestRoutesOnTheShardCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths int
		want  TrimRoute
	}{
		{name: "a list one shard short of the ceiling", paths: TrimMaxShards - 1, want: TrimRouteShards},
		{name: "a list that fills the ceiling", paths: TrimMaxShards, want: TrimRouteShards},
		{name: "one shard past the ceiling", paths: TrimMaxShards + 1, want: TrimRouteClaim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			in.CapBytes = 0
			in.Listing = shardPerPathListing(tc.paths)
			m := buildTrim(t, in)
			if got := m.Route(); got != tc.want {
				t.Fatalf("a list of %d paths takes route %v, want %v", tc.paths, got, tc.want)
			}
			if got := len(m.Shards()) > 0; got != (tc.want == TrimRouteShards) {
				t.Errorf("the manifest applies shards = %v on route %v, want them exactly under the ceiling", got, tc.want)
			}
			if got := len(m.jobs()); got != 1 {
				t.Errorf("the manifest has %d Jobs, want one on either route", got)
			}
		})
	}
}

// TestBuildTrimManifestCarriesAListPastTheShardCeilingInTheClaim is the bead's goal: a list past what
// one set of ConfigMap shards carries renders one Job that mounts the claim alone, plans the list from
// the claim in an init container and writes it there, so nothing outside the tree has to write it and
// the number of Jobs and ConfigMaps no longer grows with the list.
func TestBuildTrimManifestCarriesAListPastTheShardCeilingInTheClaim(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.CapBytes = 0
	in.SandboxesRunning = 2
	// 65 paths of about half a shard each: one set of 64 shards cannot carry them.
	in.Listing = shardPerPathListing(TrimMaxShards + 1)
	m := buildTrim(t, in)

	if got := m.Route(); got != TrimRouteClaim {
		t.Fatalf("route = %v, want the claim for a list past the shard ceiling", got)
	}
	if got := len(m.Shards()); got != 0 {
		t.Errorf("the manifest applies %d shard, want none: the claim carries the list", got)
	}
	if got := len(m.jobs()); got != 1 {
		t.Fatalf("the manifest has %d Jobs, want one whatever the list size", got)
	}
	job := m.jobs()[0]
	if job.Metadata.Name != DefaultTrimJobName {
		t.Errorf("the Job is named %q, want the name a trim always had, %q", job.Metadata.Name, DefaultTrimJobName)
	}

	pod := job.Spec.Template.Spec
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "gt-cache" {
		t.Fatalf("volumes = %+v, want the gt-cache claim alone", pod.Volumes)
	}
	sc := pod.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != SeedUserID {
		t.Errorf("pod securityContext = %+v, want the trim to run as the cache user %d", sc, SeedUserID)
	}

	// The init container is the writer: it plans PlanTrim's list from the claim under the caller's cap
	// and sandbox count, and the main container cannot start until it has written the list.
	if len(pod.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want the one that writes the list", len(pod.InitContainers))
	}
	plan := pod.InitContainers[0]
	if want := []string{HelperPath, "trim-list-write"}; !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("init command = %v, want the image's trim-list-write helper", plan.Command)
	}
	if want := []string{"--root", SeedCacheRoot, "--cap-bytes", "0", "--sandboxes", "2"}; !reflect.DeepEqual(plan.Args, want) {
		t.Errorf("init args = %v, want the caller's cap and sandbox count", plan.Args)
	}
	if len(plan.VolumeMounts) != 1 || plan.VolumeMounts[0].MountPath != SeedCacheRoot {
		t.Errorf("init mounts = %+v, want the claim at %s", plan.VolumeMounts, SeedCacheRoot)
	}
	if plan.VolumeMounts[0].ReadOnly {
		t.Error("the init container's claim mount is read-only, but it has to write the list into it")
	}

	// The trim container reads the list and the digest written beside it, both from the claim.
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want the trim container alone", len(pod.Containers))
	}
	trim := pod.Containers[0]
	if want := []string{HelperPath, "trim-paths"}; !reflect.DeepEqual(trim.Command, want) {
		t.Errorf("command = %v, want the image's trim-paths helper", trim.Command)
	}
	want := []string{"--root", SeedCacheRoot, "--list-file", TrimClaimListPath, "--list-sha256-file", TrimClaimDigestPath}
	if !reflect.DeepEqual(trim.Args, want) {
		t.Errorf("args = %v, want %v", trim.Args, want)
	}
	if len(trim.VolumeMounts) != 1 || trim.VolumeMounts[0].MountPath != SeedCacheRoot {
		t.Errorf("mounts = %+v, want the claim alone at %s", trim.VolumeMounts, SeedCacheRoot)
	}
	if doc := string(job.JSON()); strings.Contains(doc, "gocache/") || strings.Contains(doc, "ConfigMap") {
		t.Errorf("the Job document carries a path or a ConfigMap: %s", doc)
	}

	// The cleanup selector has to reach the Job a claim-carried trim applies, as it reaches a shard
	// trim's.
	if sel := TrimSelector(); !labelSelectorMatches(sel, job.Metadata.Labels) || !labelSelectorMatches(sel, job.Spec.Template.Metadata.Labels) {
		t.Errorf("selector %q misses the claim-carried Job or its pods", sel)
	}

	// The one document is the Job, and it is the manifest's only route to it.
	docs := m.Documents()
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want the one Job", len(docs))
	}
	var doc TrimJob
	if err := json.Unmarshal(docs[0], &doc); err != nil || doc.Kind != "Job" {
		t.Errorf("the only document is %+v (%v), want the Job", doc, err)
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
// route to the Jobs, so no caller can apply a Job without what it reads. An exported method that
// returns the Jobs would break it (gt-284u5).
func TestTrimManifestExportsNoJobAccessor(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(&TrimManifest{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	if want := []string{"Documents", "Route", "Shards"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TrimManifest exports %v, want %v: a Job accessor lets a caller apply a Job without what it reads", got, want)
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

// TestTrimSelectorMatchesEveryShardAndTheJobs pins the cleanup's selector across both routes: it finds
// every shard ConfigMap and the Job and its pod template, so deleting by it after the Job finishes
// leaves no trim object behind, whichever route carried the list.
func TestTrimSelectorMatchesEveryShardAndTheJobs(t *testing.T) {
	t.Parallel()
	shardTrim := trimInput()
	shardTrim.CapBytes = 0
	shardTrim.Listing = shardPerPathListing(3)

	claimTrim := trimInput()
	claimTrim.CapBytes = 0
	claimTrim.Listing = shardPerPathListing(TrimMaxShards + 1)

	sel := TrimSelector()
	for _, m := range []*TrimManifest{buildTrim(t, shardTrim), buildTrim(t, claimTrim)} {
		if m.Route() == TrimRouteShards && len(m.Shards()) == 0 {
			t.Fatalf("the shard trim applies %d shards, want the several the fixture makes", len(m.Shards()))
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
