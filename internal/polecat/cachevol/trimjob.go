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
	// TrimMaxShards is the most shards one trim Job mounts: one set of shards (gt-284u5). A removal
	// list that packs into more shards than that is carried in the claim itself, so the number of
	// Jobs and ConfigMaps a trim applies stops growing with its list, and no pod is given more mounts
	// than the route was sized for (gt-2rq36). The route is decided on this object count, not on the
	// list's bytes: a shard holds at most TrimShardBytes, so a byte ceiling would still let a list
	// near it pack into more than one set.
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
	// TrimClaimDigestName is the file under TrimClaimListDir that holds the digest of the list beside
	// it: the writer puts both in the claim, and the trim container checks the list against that
	// digest before it removes anything.
	TrimClaimDigestName = "paths.sha256"
	// TrimClaimDigestPath is where a claim-carried trim reads that digest, relative to the claim root.
	TrimClaimDigestPath = TrimClaimListDir + "/" + TrimClaimDigestName
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
	// TrimRouteShards carries the list in the ConfigMap shards one Job mounts, at most TrimMaxShards
	// of them.
	TrimRouteShards TrimRoute = iota
	// TrimRouteClaim carries the list in a file in the claim, and the Job mounts the claim alone, so
	// its ceiling is the claim's free space rather than a count of Kubernetes objects. The Job writes
	// the list itself, in an init container, before its trim container reads it.
	TrimRouteClaim
)

// TrimManifest is what one trim needs applied: the Jobs whose pods trim, and for the shard route the
// ConfigMap shards those pods mount.
//
// The list leaves argv for a file mounted or written into the claim because the kernel caps one
// exec's arguments and environment at about a quarter of the stack, 2 MB at the default 8 MB, and a
// first trim of a badly overfilled claim names tens of thousands of paths (gt-cahkg).
type TrimManifest struct {
	route TrimRoute
	// shards is the shard route's list in ConfigMaps, in mount order; a claim-carried trim applies
	// none.
	shards []TrimShard
	// job is the one Job a trim applies, whichever route carries its list. Neither route splits a
	// trim over several Jobs: a list past one set of shards takes the claim.
	job *TrimJob
}

// Route returns the route that carries this trim's removal list.
func (m *TrimManifest) Route() TrimRoute { return m.route }

// Shards returns every shard ConfigMap in the order the Job mounts them; a claim-carried trim
// applies none.
func (m *TrimManifest) Shards() []TrimShard { return m.shards }

// jobs returns the trim Jobs in the order they are applied. It is unexported so Documents stays the
// manifest's only route out: an exported accessor would hand a caller a Job before the shards it
// reads.
func (m *TrimManifest) jobs() []*TrimJob { return []*TrimJob{m.job} }

// Documents returns the manifest encoded for kubectl apply: for a shard trim, the shards first and
// then the Job that mounts them; for a claim-carried trim, the one Job, whose init container writes
// the list into the claim before its trim container reads it. A Job applied before its shards leaves
// a pod waiting on a volume it cannot mount, and an omitted shard trims part of the list. Documents
// is the manifest's only route to the Jobs, so no caller can apply a Job without what it reads.
func (m *TrimManifest) Documents() [][]byte {
	if m.route == TrimRouteClaim {
		return [][]byte{m.job.JSON()}
	}
	docs := make([][]byte, 0, len(m.shards)+1)
	for i := range m.shards {
		docs = append(docs, m.shards[i].JSON())
	}
	return append(docs, m.job.JSON())
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
	// with it. It is the caller's reading of the claim: a claim-carried trim's pod plans the list
	// again when it runs, so a claim that changed in between can end its run under the cap.
	var annotations map[string]string
	if plan.Overflow {
		annotations = map[string]string{TrimOverflowAnnotation: strconv.FormatInt(plan.OverBytes, 10)}
	}

	// The route is decided on the Kubernetes objects the shard route would need, not on the list's
	// bytes: a shard holds at most TrimShardBytes, so a list under a byte ceiling can still pack into
	// more than one set of shards. A list that packs into more than TrimMaxShards takes the claim, and
	// a trim never splits over several Jobs (gt-2rq36).
	if trimShardCount(paths) > TrimMaxShards {
		return claimTrimManifest(in, name, annotations), nil
	}
	shards := trimShards(name, in.Namespace, paths)
	return &TrimManifest{route: TrimRouteShards, shards: shards, job: trimJob(in, name, annotations, shards)}, nil
}

// claimTrimManifest is the manifest for a list past one set of ConfigMap shards: one Job that mounts
// the claim alone, plans the removal list from the claim in an init container, and trims what it
// wrote. Nothing outside the tree has to write the list, so the Job is the whole route, as a shard
// trim's Job and shards are.
func claimTrimManifest(in TrimJobInput, name string, annotations map[string]string) *TrimManifest {
	return &TrimManifest{route: TrimRouteClaim, job: claimTrimJob(in, name, annotations)}
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

// claimTrimJob is the Job document for the claim route: the claim alone is mounted, an init container
// plans the removal list from the claim and writes it into the claim, and the trim container then
// reads it from there. The init container runs first and the Job's pod stops if it fails, so the trim
// container never runs without a list of its own pod's writing.
func claimTrimJob(in TrimJobInput, name string, annotations map[string]string) *TrimJob {
	volumes := []Volume{
		{Name: trimCacheVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName}},
	}
	mounts := []VolumeMount{{Name: trimCacheVolume, MountPath: SeedCacheRoot}}
	return trimJobDocument(in, name, annotations, claimRemoveContainer(in.Image, mounts), volumes,
		claimWriteContainer(in.Image, mounts, in.CapBytes, in.SandboxesRunning))
}

// trimJobDocument is the Job both routes apply: the cache user's trim container, with the volumes and
// mounts its route gives it, and any init container that has to run before it.
func trimJobDocument(in TrimJobInput, name string, annotations map[string]string, container Container, volumes []Volume, init ...Container) *TrimJob {
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
					InitContainers: init,
					Containers:     []Container{container},
					Volumes:        volumes,
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

// claimWriteContainer plans the removal list from the claim and writes it into the claim, with the
// image's trim-list-write helper. The plan rules and their inputs are the caller's: the cap the trim
// brings the claim under and the caller's count of running sandboxes, so the list is PlanTrim's own
// rather than a second set of rules the Job invents.
func claimWriteContainer(image string, mounts []VolumeMount, capBytes int64, sandboxes int) Container {
	return Container{
		Name:    "plan",
		Image:   image,
		Command: []string{HelperPath, "trim-list-write"},
		Args: []string{
			"--root", SeedCacheRoot,
			"--cap-bytes", strconv.FormatInt(capBytes, 10),
			"--sandboxes", strconv.Itoa(sandboxes),
		},
		VolumeMounts: mounts,
	}
}

// claimRemoveContainer deletes the listed files with the image's trim-paths helper, which reads the
// list from the claim file the Job's init container wrote. The helper refuses a list whose digest is
// not the one written beside it, so a list this Job did not write is not trimmed, and a list that is
// not there fails the run rather than trimming nothing.
func claimRemoveContainer(image string, mounts []VolumeMount) Container {
	return Container{
		Name:    "trim",
		Image:   image,
		Command: []string{HelperPath, "trim-paths"},
		Args: []string{
			"--root", SeedCacheRoot,
			"--list-file", TrimClaimListPath,
			"--list-sha256-file", TrimClaimDigestPath,
		},
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

// trimShardCount is how many ConfigMaps trimShards would make of paths: the object count the route
// is decided on, without building the shards a claim-carried trim never applies. The packing is
// trimShards', step for step, so the count and the shards it counts agree; a test pins the two at the
// boundary the route turns on.
func trimShardCount(paths []string) int {
	count := 1
	start, size := 0, 2 // the array's brackets
	for i, p := range paths {
		// A string's JSON encoding cannot fail, so the error is dropped.
		b, _ := json.Marshal(p)
		add := len(b)
		if i > start {
			add++ // the separator before this entry
		}
		if i > start && size+add > TrimShardBytes {
			count++
			start, size, add = i, 2, len(b)
		}
		size += add
	}
	return count
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
