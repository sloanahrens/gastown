package cachevol

import (
	"encoding/json"
	"fmt"
	"path"
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
	// TrimMaxShards is the most shards one trim Job mounts, so the most list one pod carries is this
	// many TrimShardBytes. A longer list is split over several Jobs, each mounting at most this many
	// shards, so no pod is given more mounts than the route was sized for (gt-284u5).
	TrimMaxShards = 64
	// TrimListRoot is where the trim container mounts the shards: shard i is the directory
	// <TrimListRoot>/<i>, holding the file TrimShardKey.
	TrimListRoot = "/trim"
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

// TrimManifest is what one trim needs applied: the ConfigMap shards carrying PlanTrim's removal
// list, and the Jobs whose pods mount them. A list that fits TrimMaxShards is one Job; a longer one
// is split over several, each Job mounting its own run of the shards.
//
// The list leaves argv for a mounted file because the kernel caps one exec's arguments and
// environment at about a quarter of the stack, 2 MB at the default 8 MB, and a first trim of a
// badly overfilled claim names tens of thousands of paths (gt-cahkg).
type TrimManifest struct {
	runs []trimRun
}

// trimRun is one Job and the shards its pod mounts, in mount order.
type trimRun struct {
	shards []TrimShard
	job    *TrimJob
}

// Shards returns every shard ConfigMap in the order the Jobs mount them.
func (m *TrimManifest) Shards() []TrimShard {
	var all []TrimShard
	for _, run := range m.runs {
		all = append(all, run.shards...)
	}
	return all
}

// Jobs returns the trim Jobs in the order they are applied: one per run of at most TrimMaxShards
// shards.
func (m *TrimManifest) Jobs() []*TrimJob {
	jobs := make([]*TrimJob, 0, len(m.runs))
	for _, run := range m.runs {
		jobs = append(jobs, run.job)
	}
	return jobs
}

// Documents returns the manifest encoded for kubectl apply, each run's shards first and then its Job:
// a Job applied before its shards leaves a pod waiting on a volume it cannot mount, and an omitted
// shard trims part of the list. It is the manifest's only route to the Jobs, so no caller can apply
// a Job without its shards.
func (m *TrimManifest) Documents() [][]byte {
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
	shards := trimShards(name, in.Namespace, paths)

	// The annotation carries PlanTrim's own verdict onto the object: an operator reading the Job
	// after the run finds from it why the claim is still over its cap, since the plan is not applied
	// with it.
	var annotations map[string]string
	if plan.Overflow {
		annotations = map[string]string{TrimOverflowAnnotation: strconv.FormatInt(plan.OverBytes, 10)}
	}

	runs, err := trimRuns(in, name, annotations, shards)
	if err != nil {
		return nil, err
	}
	return &TrimManifest{runs: runs}, nil
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

// trimJob is the Job document: the cache user trimming the claim, reading the list from the shards
// its pod mounts.
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
					Containers: []Container{removeContainer(in.Image, len(shards), mounts)},
					Volumes:    volumes,
				},
			},
		},
	}
}

// trimLabels are the labels on the Job, its pods and its shards, so a selector matches all of them.
// Nothing in the tree deletes the shards yet: a rerun of the same trim overwrites them by name, and
// the most a stale set can leave behind is the shards of a longer earlier run.
func trimLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "polecat-cache-trim",
		"app.kubernetes.io/managed-by": "gt",
	}
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
