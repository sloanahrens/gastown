package cachevol

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	// DefaultTrimJobName is the trim Job's name when the input names none.
	DefaultTrimJobName = "gt-cache-trim"
	// TrimOverflowAnnotation carries TrimPlan.OverBytes onto the Job, so a Job read after the run
	// says why the claim is still over its cap.
	TrimOverflowAnnotation = "gastown.dev/cache-overflow-bytes"
	// TrimShardKey is the key each shard ConfigMap holds its part of the removal list under, and so
	// the file name the container reads in each mounted shard.
	TrimShardKey = "paths"
	// TrimShardBytes is the most encoded list one shard holds, a quarter of the 1 MiB a ConfigMap's
	// data is capped at.
	TrimShardBytes = 256 << 10
	// TrimMaxShards is the most shards one trim Job mounts, so one set of shards carries at most
	// TrimMaxShards*TrimShardBytes of encoded list (gt-284u5). A longer list is carried in the claim
	// itself, so no pod is given more mounts than the route was sized for, and the Kubernetes
	// objects a trim applies stop growing with its list (gt-2rq36). A list under that ceiling that
	// still needs more shards than one Job mounts is split over several Jobs.
	TrimMaxShards = 64
	// TrimListRoot is where the trim container mounts the shards: shard i is the directory
	// <TrimListRoot>/<i>, holding the file TrimShardKey.
	TrimListRoot = "/trim"
	// TrimClaimListDir is the claim-relative directory a claim-carried trim keeps its removal list
	// in. It sits outside GOCACHE and GOMODCACHE, so a listing that walks the two cache trees never
	// names it, and a trim removes only the paths its list names.
	TrimClaimListDir = ".gt-trim"
	// TrimClaimListName is the file under TrimClaimListDir that holds the encoded removal list.
	TrimClaimListName = "paths.json"
	// TrimClaimListPath is where a claim-carried trim writes and reads its list, relative to the
	// claim root.
	TrimClaimListPath = TrimClaimListDir + "/" + TrimClaimListName
	// trimCacheVolume is the volume the claim is mounted under, and trimShardVolume the prefix of
	// each shard's volume; a volume name is a DNS-1123 label, so it carries no shard ConfigMap's
	// full name.
	trimCacheVolume = "cache"
	trimShardVolume = "trim-list-"
)

// TrimJobInput is what the trim Job is built from: the polecat image its container runs, the claim
// it trims, and the listing PlanTrim decides on.
type TrimJobInput struct {
	// Name is the Job's name; DefaultTrimJobName when empty.
	Name string
	// Namespace is the Job's namespace; empty leaves the choice to the applying command.
	Namespace string
	// Image is the polecat image tag, which carries the trim-paths helper.
	Image string
	// ClaimName is the shared cache claim the Job trims.
	ClaimName string
	// Listing is the claim's files as the caller read them, each path relative to the claim root,
	// the form PlanTrim's entries take.
	Listing []Entry
	// CapBytes is the size the claim is brought under, passed in rather than read from a config.
	CapBytes int64
	// SandboxesRunning is the caller's count of running sandboxes: PlanTrim leaves the module cache
	// alone while any runs, and nothing here asks the cluster how many there are.
	SandboxesRunning int
}

// TrimJob is the trim Job as the values kubectl applies: a Job document, so it carries SeedJob's
// shape and encodes through SeedJob.JSON.
type TrimJob = SeedJob

// TrimRoute says how a trim's removal list reaches its container, and so what bounds its size.
type TrimRoute int

const (
	// TrimRouteShards carries the list in ConfigMap shards the Jobs mount. A list that needs more
	// than one Job's mounts is split over several Jobs, each mounting at most TrimMaxShards.
	TrimRouteShards TrimRoute = iota
	// TrimRouteClaim carries the list in one file in the claim, and the Job mounts the claim alone,
	// so its ceiling is the claim's free space rather than a count of Kubernetes objects. The caller
	// writes ClaimList into the claim before it applies the documents.
	TrimRouteClaim
)

// TrimClaimList is the list a claim-carried trim reads: where in the claim to write it, and the
// encoded list to write.
type TrimClaimList struct {
	// Path is the claim-relative file the trim container reads.
	Path string
	// Data is the encoded JSON array of PlanTrim's claim-relative paths.
	Data []byte
}

// TrimManifest is what one trim needs applied: the removal list in whichever route carries it, and
// the Job or Jobs whose pods read it.
//
// The list leaves argv for a file mounted or written into the claim because the kernel caps one
// exec's arguments and environment at about a quarter of the stack, 2 MB at the default 8 MB, and a
// first trim of a badly overfilled claim names tens of thousands of paths (gt-cahkg).
type TrimManifest struct {
	route TrimRoute
	// runs is the shard route: one Job per run of at most TrimMaxShards shards, with the shards its
	// pod mounts, in mount order.
	runs []trimRun
	// list and job are the claim route: the list to write into the claim, and the one Job whose pod
	// reads it from the claim alone.
	list TrimClaimList
	job  *TrimJob
}

// trimRun is one Job and the shards its pod mounts, in mount order.
type trimRun struct {
	shards []TrimShard
	job    *TrimJob
}

// Route returns the route that carries this trim's removal list.
func (m *TrimManifest) Route() TrimRoute { return m.route }

// Shards returns every shard ConfigMap in the order the Jobs mount them; a claim-carried trim
// applies none.
func (m *TrimManifest) Shards() []TrimShard {
	var all []TrimShard
	for _, run := range m.runs {
		all = append(all, run.shards...)
	}
	return all
}

// jobs returns the trim Jobs in the order they are applied. It is unexported so Documents stays the
// manifest's only route out: an exported accessor would hand a caller a Job before the shards or
// list it reads.
func (m *TrimManifest) jobs() []*TrimJob {
	if m.route == TrimRouteClaim {
		return []*TrimJob{m.job}
	}
	jobs := make([]*TrimJob, 0, len(m.runs))
	for _, run := range m.runs {
		jobs = append(jobs, run.job)
	}
	return jobs
}

// ClaimList returns the list a caller must write into the claim before it applies Documents, for a
// claim-carried trim; a shard trim carries its list in the documents themselves and returns nil.
func (m *TrimManifest) ClaimList() *TrimClaimList {
	if m.route != TrimRouteClaim {
		return nil
	}
	list := m.list
	return &list
}

// Documents returns the manifest encoded for kubectl apply: for a shard trim, each run's shards
// first and then its Job; for a claim-carried trim, the one Job, which the caller applies after it
// writes ClaimList into the claim. A Job applied before its shards leaves a pod waiting on a volume
// it cannot mount, and an omitted shard trims part of the list. Documents is the manifest's only
// route to the Jobs, so no caller can apply a Job without what it reads.
func (m *TrimManifest) Documents() [][]byte {
	if m.route == TrimRouteClaim {
		return [][]byte{m.job.JSON()}
	}
	docs := make([][]byte, 0, len(m.Shards())+len(m.runs))
	for _, run := range m.runs {
		for i := range run.shards {
			docs = append(docs, run.shards[i].JSON())
		}
		docs = append(docs, run.job.JSON())
	}
	return docs
}

// TrimShard is one ConfigMap carrying part of the removal list under TrimShardKey, as the encoded
// JSON array of PlanTrim's claim-relative paths.
type TrimShard struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Data       map[string]string `json:"data"`
}

// JSON returns the shard as the document kubectl applies.
func (s *TrimShard) JSON() []byte { return encodeDocument(s) }

// TrimShardName is the shard ConfigMap's name for jobName and index: derived from the Job's name so
// a rerun of the same trim overwrites the same shards instead of adding a set per run.
func TrimShardName(jobName string, index int) string {
	return jobName + "-" + strconv.Itoa(index)
}

// BuildTrimManifest returns the trim for in, or the reason it cannot be rendered.
//
// The removal list is PlanTrim's, so the Job deletes what the rules allow and nothing else: the
// container runs as SeedUserID with the claim at SeedCacheRoot, so it can remove only what a
// sandbox could write.
func BuildTrimManifest(in TrimJobInput) (*TrimManifest, error) {
	name := in.Name
	if name == "" {
		name = DefaultTrimJobName
	}
	if !validDNS1123Label(name) {
		return nil, fmt.Errorf("the trim Job name %q: want a DNS-1123 label of at most 63 characters, since Kubernetes names the pod's job-name label after it", name)
	}
	if in.Namespace != "" && !validDNS1123Label(in.Namespace) {
		return nil, fmt.Errorf("the trim Job namespace %q: want a DNS-1123 label of at most 63 characters", in.Namespace)
	}
	if !validImage(in.Image) {
		return nil, fmt.Errorf("the trim Job image %q: want an image reference such as gastown-polecat:dev", in.Image)
	}
	if !validDNS1123Label(in.ClaimName) {
		return nil, fmt.Errorf("the trim Job claim %q: want a DNS-1123 label of at most 63 characters", in.ClaimName)
	}
	if in.CapBytes < 0 {
		return nil, fmt.Errorf("the trim Job cap %d bytes: want the size the claim is trimmed under", in.CapBytes)
	}
	if in.SandboxesRunning < 0 {
		return nil, fmt.Errorf("the trim Job sandbox count %d: want the caller's count of running sandboxes, 0 when none runs", in.SandboxesRunning)
	}

	if err := checkListing(in.Listing); err != nil {
		return nil, err
	}

	plan := PlanTrim(in.Listing, in.CapBytes, in.SandboxesRunning > 0)
	if len(plan.Removals) == 0 {
		return nil, fmt.Errorf("the trim Job on %s would remove nothing: the listing is %d bytes, and the cap is %d", in.ClaimName, plan.Total, in.CapBytes)
	}
	paths, err := removalPaths(plan.Removals)
	if err != nil {
		return nil, err
	}

	// The annotation carries PlanTrim's own verdict onto the object: an operator reading the Job
	// after the run finds from it why the claim is still over its cap, since the plan is not applied
	// with it.
	var annotations map[string]string
	if plan.Overflow {
		annotations = map[string]string{TrimOverflowAnnotation: strconv.FormatInt(plan.OverBytes, 10)}
	}

	if trimListBytes(paths) > TrimMaxShards*TrimShardBytes {
		return claimTrimManifest(in, name, annotations, paths), nil
	}

	shards := trimShards(name, in.Namespace, paths)
	runs, err := trimRuns(in, name, annotations, shards)
	if err != nil {
		return nil, err
	}
	return &TrimManifest{route: TrimRouteShards, runs: runs}, nil
}

// claimTrimManifest is the manifest for a list past one set of ConfigMap shards: one Job whose pod
// mounts the claim alone, reading the list from the file the caller writes into it first.
func claimTrimManifest(in TrimJobInput, name string, annotations map[string]string, paths []string) *TrimManifest {
	// Encoding a slice of strings cannot fail, so the error is dropped.
	data, _ := json.Marshal(paths)
	return &TrimManifest{
		route: TrimRouteClaim,
		list:  TrimClaimList{Path: TrimClaimListPath, Data: data},
		job:   claimTrimJob(in, name, annotations),
	}
}

// trimRuns splits shards into runs of at most TrimMaxShards, in mount order, and renders one Job per
// run. A list that fits one run keeps the Job's name; a longer one names each Job by its run index,
// so every Job is its own object and no pod mounts more shards than the route was sized for.
func trimRuns(in TrimJobInput, name string, annotations map[string]string, shards []TrimShard) ([]trimRun, error) {
	// A Job with no shard mounts nothing and trims nothing, yet would apply as a success, so an empty
	// list is refused here rather than returned as a manifest with no Job.
	if len(shards) == 0 {
		return nil, fmt.Errorf("the trim Job on %s has no shard to mount: want the removal list's shards, so a Job never applies with nothing to trim", in.ClaimName)
	}
	var runs []trimRun
	for start := 0; start < len(shards); start += TrimMaxShards {
		end := start + TrimMaxShards
		if end > len(shards) {
			end = len(shards)
		}
		jobName := name
		if len(shards) > TrimMaxShards {
			jobName = name + "-" + strconv.Itoa(start/TrimMaxShards)
			if !validDNS1123Label(jobName) {
				return nil, fmt.Errorf("the split trim Job name %q: want a DNS-1123 label of at most 63 characters, since each run's Job takes the name with its index", jobName)
			}
		}
		runs = append(runs, trimRun{shards: shards[start:end], job: trimJob(in, jobName, annotations, shards[start:end])})
	}
	return runs, nil
}

// trimJob is the Job document for the shard route: the cache user trimming the claim, reading the
// list from the shards its pod mounts.
func trimJob(in TrimJobInput, name string, annotations map[string]string, shards []TrimShard) *TrimJob {
	volumes := []Volume{
		{Name: trimCacheVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName}},
	}
	mounts := []VolumeMount{{Name: trimCacheVolume, MountPath: SeedCacheRoot}}
	for i, shard := range shards {
		volume := trimShardVolume + strconv.Itoa(i)
		volumes = append(volumes, Volume{Name: volume, ConfigMap: &ConfigMapVolumeSource{Name: shard.Metadata.Name}})
		mounts = append(mounts, VolumeMount{Name: volume, MountPath: path.Join(TrimListRoot, strconv.Itoa(i)), ReadOnly: true})
	}
	return trimJobDocument(in, name, annotations, removeContainer(in.Image, len(shards), mounts), volumes)
}

// claimTrimJob is the Job document for the claim route: the claim alone is mounted, and the
// container reads the list from the file a caller wrote into it.
func claimTrimJob(in TrimJobInput, name string, annotations map[string]string) *TrimJob {
	volumes := []Volume{
		{Name: trimCacheVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName}},
	}
	mounts := []VolumeMount{{Name: trimCacheVolume, MountPath: SeedCacheRoot}}
	return trimJobDocument(in, name, annotations, claimRemoveContainer(in.Image, mounts), volumes)
}

// trimJobDocument is the Job both routes apply: the cache user's trim container, with the volumes
// and mounts its route gives it.
func trimJobDocument(in TrimJobInput, name string, annotations map[string]string, container Container, volumes []Volume) *TrimJob {
	return &TrimJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: ObjectMeta{
			Name:        name,
			Namespace:   in.Namespace,
			Labels:      trimLabels(),
			Annotations: annotations,
		},
		Spec: JobSpec{
			// A failed trim reports itself: the removal list is the run's whole input, so a retry
			// re-runs the same failure.
			BackoffLimit: int32Ptr(0),
			Template: PodTemplate{
				Metadata: TemplateMeta{Labels: trimLabels()},
				Spec: PodSpec{
					RestartPolicy: "Never",
					SecurityContext: &PodSecurityContext{
						RunAsUser:  int64Ptr(SeedUserID),
						RunAsGroup: int64Ptr(SeedUserID),
					},
					Containers: []Container{container},
					Volumes:    volumes,
				},
			},
		},
	}
}

// trimLabels are the labels on the Job, its pods and its shards, so TrimSelector matches all of them.
func trimLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "polecat-cache-trim",
		"app.kubernetes.io/managed-by": "gt",
	}
}

// TrimSelector is the label selector matching every object a trim applies, its shard ConfigMaps
// included. A cleanup deletes by it once the Jobs finish, not by name: a rerun overwrites the shards
// of the same Job by name, but a longer earlier run leaves shards a rerun does not name.
func TrimSelector() string {
	labels := trimLabels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		terms = append(terms, k+"="+labels[k])
	}
	return strings.Join(terms, ",")
}

// removeContainer deletes the listed files with the image's trim-paths helper. The helper reads the
// paths from the mounted shards, not from argv, so a name that holds a space or a glob character
// stays the one string the shard gave; the helper decides whether a path may be removed.
func removeContainer(image string, shards int, mounts []VolumeMount) Container {
	return Container{
		Name:    "trim",
		Image:   image,
		Command: []string{HelperPath, "trim-paths"},
		Args:    []string{"--root", SeedCacheRoot, "--list", TrimListRoot, "--shards", strconv.Itoa(shards)},
		// The shards are the whole input: a mount a pod cannot make leaves it waiting rather than
		// trimming the part of the list that did arrive.
		VolumeMounts: mounts,
	}
}

// claimRemoveContainer deletes the listed files with the image's trim-paths helper, which reads the
// list from the file the caller wrote into the claim. A list that is not there fails the run rather
// than trimming nothing.
func claimRemoveContainer(image string, mounts []VolumeMount) Container {
	return Container{
		Name:         "trim",
		Image:        image,
		Command:      []string{HelperPath, "trim-paths"},
		Args:         []string{"--root", SeedCacheRoot, "--list-file", TrimClaimListPath},
		VolumeMounts: mounts,
	}
}

// checkListing refuses an entry whose Cache names a different part of the claim than its path sits
// in. PlanTrim chooses what to remove by Entry.Cache while the container deletes by path, so a
// listing that misnames the directory would have the rules admit a file of one cache and the argv
// delete a file of the other: a module-cache file removed while a sandbox is building.
//
// An entry that is not claim-relative is a path problem, left to removalPaths, which refuses it
// after the plan and still before any shard or Job is built.
func checkListing(listing []Entry) error {
	for _, e := range listing {
		if !claimRelative(e.Path) {
			continue
		}
		dir, ok := cacheDir(e.Cache)
		if !ok {
			return fmt.Errorf("the trim listing names the cache %d for %q: want BuildCache or ModuleCache", int(e.Cache), e.Path)
		}
		if !strings.HasPrefix(e.Path, dir+"/") {
			return fmt.Errorf("the trim listing puts %q under a directory its cache does not name: want a path under %s/", e.Path, dir)
		}
	}
	return nil
}

// cacheDir is the claim-relative directory a cache's entries sit in, and false for a Cache the
// package does not define.
func cacheDir(c Cache) (string, bool) {
	switch c {
	case BuildCache:
		return SeedBuildCacheDir, true
	case ModuleCache:
		return SeedModuleCacheDir, true
	}
	return "", false
}

// removalPaths is PlanTrim's removals as the claim-relative paths the shards carry. A path that is
// absolute, not clean or above the claim root could name a file the listing did not, so it is
// refused rather than handed to the container.
func removalPaths(removals []Removal) ([]string, error) {
	out := make([]string, 0, len(removals))
	for _, r := range removals {
		if !claimRelative(r.Path) {
			return nil, fmt.Errorf("the trim plan names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", r.Path)
		}
		out = append(out, r.Path)
	}
	return out, nil
}

// claimRelative reports whether p is a path a walk of the claim gives: relative to the claim root,
// already clean, and below it rather than at it or above it.
//
// The check is on the string, so a "." or ".." component keeps it inside the claim wherever the
// component sits. Whether a component that stays inside the claim is a directory rather than a
// symlink is decided at removal, by trim-paths on the mounted claim (internal/polecatimage/
// trimpaths.go): it resolves the entry's directory and refuses one that leaves the claim, then
// removes through an os.Root, which refuses a symlink pointing outside its root. This package reads
// no filesystem, so the claim's own shape is the container's to judge.
func claimRelative(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	first, _, _ := strings.Cut(p, "/")
	return first != "." && first != ".."
}

// trimListBytes is the size paths take as the encoded JSON array the shards and the claim list
// carry: the figure the shard route's ceiling is measured on. It measures each path's encoding, as
// trimShards does, so the two agree on what a character the encoder escapes costs.
func trimListBytes(paths []string) int {
	size := 2 // the array's brackets
	for i, p := range paths {
		// A string's JSON encoding cannot fail, so the error is dropped.
		b, _ := json.Marshal(p)
		size += len(b)
		if i > 0 {
			size++ // the separator before this entry
		}
	}
	return size
}

// trimShards splits paths into the ConfigMaps the Job mounts, each holding at most TrimShardBytes
// of encoded list.
//
// The bound counts the encoded JSON, not the path text: a character the encoder escapes costs six
// bytes, so a shard measured on raw text could cross the ConfigMap cap. An entry that alone exceeds
// the bound takes a shard of its own, since splitting it is not an option.
func trimShards(jobName, namespace string, paths []string) []TrimShard {
	encoded := make([]string, len(paths))
	for i, p := range paths {
		// A string's JSON encoding cannot fail, so the error is dropped.
		b, _ := json.Marshal(p)
		encoded[i] = string(b)
	}

	var shards []TrimShard
	start, size := 0, 2 // the array's brackets
	for i := range encoded {
		add := len(encoded[i])
		if i > start {
			add++ // the separator before this entry
		}
		if i > start && size+add > TrimShardBytes {
			shards = append(shards, newTrimShard(jobName, namespace, len(shards), encoded[start:i]))
			start, size, add = i, 2, len(encoded[i])
		}
		size += add
	}
	return append(shards, newTrimShard(jobName, namespace, len(shards), encoded[start:]))
}

// newTrimShard is one shard ConfigMap whose list is entries, already encoded, joined into a JSON
// array.
func newTrimShard(jobName, namespace string, index int, entries []string) TrimShard {
	return TrimShard{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: ObjectMeta{
			Name:      TrimShardName(jobName, index),
			Namespace: namespace,
			Labels:    trimLabels(),
		},
		Data: map[string]string{TrimShardKey: "[" + strings.Join(entries, ",") + "]"},
	}
}
