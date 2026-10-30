package daemon

// cache_trim applies the shared cache claim's trim from the disk_maintenance
// patrol (gt-rm4am): in the patrol's window the pass reads the claim's listing,
// builds the trim Job with internal/polecat/cachevol, and in apply mode applies
// the manifest's documents in order, the shard ConfigMaps before the Job that
// mounts them.
//
// The patrol is the one applier, and dry run is its default as for every
// maintenance target: the manifest is built and the objects it would apply are
// logged, and nothing is written. A listing that cannot be read refuses the
// trim and says so, because a listing read as empty would plan a trim of a
// claim nothing looked at.
//
// The listing comes from the cluster, not from this host: the claim is mounted
// only by pods, so the pass reads the pods that mount it with kubectl -- the way
// the registry target's in-use provider reads pods -- counts the running ones as
// PlanTrim's sandbox count, and asks one of them to print the listing with the
// image's trim-list helper.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// cacheTrimCapEnv is the size the claim is trimmed under: the knob the
	// polecat-image plugin already bounds each per-rig cache volume with, so a
	// town sets one cache cap rather than one per cache.
	cacheTrimCapEnv = "POLECAT_CACHE_CAP"

	// cacheTrimDefaultCap is the cap when the setting names none, the plugin's
	// own default.
	cacheTrimDefaultCap = "10GB"

	// cacheTrimWaitTimeout bounds the wait on a trim Job before its shard
	// ConfigMaps are deleted. The pass itself is bounded by
	// diskMaintRunTimeout, which the command inherits, so a Job still running
	// then is reported and its shards are left for the next run.
	cacheTrimWaitTimeout = 2 * time.Minute

	// cacheTrimKubeconfig is the town's sandbox kubeconfig, relative to the
	// town root, the file the registry target reads the in-use pods through too.
	cacheTrimKubeconfig = ".runtime/sbx-k8s/kubeconfig"
)

// cacheTrimClaimRE is the DNS-1123 label form a PersistentVolumeClaim's name
// takes.
var cacheTrimClaimRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// cacheTrimClaim is one shared cache claim a pass trims, and the image its
// trim Job runs: the image the sandbox profiles that mount it run, since the
// Job's containers are helpers of that image.
type cacheTrimClaim struct {
	ClaimName string
	Image     string
}

// cacheTrimClaims returns the claims the sandbox profiles mount, one per claim,
// sorted by claim name, and the profiles that cannot be trimmed as they stand.
//
// Only a kubernetes profile mounts a claim at all. A claim two profiles name
// with different images is refused: the trim Job runs one image's helpers, and
// which one is the town's to say, not this pass's to pick.
func cacheTrimClaims(profiles map[string]agentconfig.SandboxProfile) ([]cacheTrimClaim, []error) {
	images := map[string]map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		profile := profiles[name]
		if profile.Driver != agentconfig.SandboxDriverKubernetes || profile.KubernetesCacheClaim == "" {
			continue
		}
		if images[profile.KubernetesCacheClaim] == nil {
			images[profile.KubernetesCacheClaim] = map[string]bool{}
		}
		images[profile.KubernetesCacheClaim][profile.Image] = true
	}

	var claims []cacheTrimClaim
	var errs []error
	for _, claim := range slices.Sorted(maps.Keys(images)) {
		names := slices.Sorted(maps.Keys(images[claim]))
		if len(names) != 1 {
			errs = append(errs, fmt.Errorf("the cache claim %s is mounted by sandbox profiles with %d images (%s); the trim Job runs one image, so none is chosen", claim, len(names), strings.Join(names, ", ")))
			continue
		}
		if !validTrimClaim(claim) {
			errs = append(errs, fmt.Errorf("the cache claim %s: want a DNS-1123 label of at most 63 characters", claim))
			continue
		}
		claims = append(claims, cacheTrimClaim{ClaimName: claim, Image: names[0]})
	}
	return claims, errs
}

// validTrimClaim reports whether claim is a name a PersistentVolumeClaim takes:
// a DNS-1123 label. The settings check the same form, and this pass checks it
// again rather than trust a config built in code, since the name reaches a
// kubectl argument.
func validTrimClaim(claim string) bool {
	return len(claim) <= 63 && cacheTrimClaimRE.MatchString(claim)
}

// cacheTrimCap returns the size the claim is trimmed under. A setting that is
// not a size refuses the trim rather than reading as a cap near zero, which
// would trim everything.
func cacheTrimCap(getenv func(string) string) (int64, error) {
	value := getenv(cacheTrimCapEnv)
	if value == "" {
		value = cacheTrimDefaultCap
	}
	capBytes, ok := polecatimage.CapBytes(value)
	if !ok {
		return 0, fmt.Errorf("%s=%q is not a size such as 10GB", cacheTrimCapEnv, value)
	}
	return capBytes, nil
}

// cacheTrim is the pass's cache trim: each claim a kubernetes sandbox profile
// mounts, trimmed in the mode the patrol runs in.
func (d *Daemon) cacheTrim(ctx context.Context, mode string) {
	s := d.diskMaintenanceSeams()
	profiles, err := s.sandboxes()
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim: the sandbox profiles could not be read: %v; nothing trimmed", err)
		return
	}
	claims, errs := cacheTrimClaims(profiles)
	for _, err := range errs {
		d.logger.Printf("disk_maintenance: cache trim: not applied: %v", err)
	}
	if len(claims) == 0 {
		return
	}
	capBytes, err := cacheTrimCap(s.getenv)
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim: not applied: %v", err)
		return
	}
	kubeconfig := filepath.Join(d.config.TownRoot, filepath.FromSlash(cacheTrimKubeconfig))
	// trimmed is the namespace each claim this pass trims in. A trim's labels
	// are the same for every claim, so two claims trimming in one namespace
	// would delete each other's objects; the second is refused.
	trimmed := map[string]string{}
	for _, claim := range claims {
		d.cacheTrimClaim(ctx, kubeconfig, claim, capBytes, mode == agentconfig.DiskMaintenanceModeApply, trimmed)
	}
}

// cacheTrimClaim trims one claim: it reads the listing through a pod that
// mounts the claim, builds the trim, and in apply mode applies it. trimmed is
// the namespace each claim this pass already trims in, which the claim joins.
func (d *Daemon) cacheTrimClaim(ctx context.Context, kubeconfig string, claim cacheTrimClaim, capBytes int64, apply bool, trimmed map[string]string) {
	running, sources, err := d.cacheTrimPods(ctx, kubeconfig, claim.ClaimName)
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: the pods that mount it could not be read: %v; nothing trimmed", claim.ClaimName, err)
		return
	}
	if len(sources) == 0 {
		d.logger.Printf("disk_maintenance: cache trim %s: no running pod mounts it, so its listing cannot be read; nothing trimmed", claim.ClaimName)
		return
	}
	listing, err := d.cacheTrimListing(ctx, kubeconfig, sources[0])
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: the listing could not be read: %v; nothing trimmed", claim.ClaimName, err)
		return
	}

	// The Job is named after the claim so two claims in one namespace get two
	// Jobs, and a rerun of a trim overwrites its own rather than a neighbour's.
	jobName := cachevol.DefaultTrimJobName + "-" + claim.ClaimName
	namespace := sources[0].Namespace
	if other, taken := trimmed[namespace]; taken {
		d.logger.Printf("disk_maintenance: cache trim %s: the claim %s is trimmed in the same namespace (%s), and a trim's labels cannot tell the two sets of objects apart; nothing trimmed", claim.ClaimName, other, namespace)
		return
	}
	trimmed[namespace] = claim.ClaimName
	m, err := cachevol.BuildTrimManifest(cachevol.TrimJobInput{
		Name:             jobName,
		Namespace:        namespace,
		Image:            claim.Image,
		ClaimName:        claim.ClaimName,
		Listing:          listing,
		CapBytes:         capBytes,
		SandboxesRunning: running,
	})
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: %v; nothing trimmed", claim.ClaimName, err)
		return
	}
	if !apply {
		d.logger.Printf("disk_maintenance: cache trim %s dry_run: %s", claim.ClaimName, cacheTrimManifestLine(jobName, m))
		return
	}
	if err := d.cacheTrimApply(ctx, kubeconfig, namespace, jobName, m); err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: the apply failed: %v", claim.ClaimName, err)
		return
	}
	d.logger.Printf("disk_maintenance: cache trim %s applied: %s", claim.ClaimName, cacheTrimManifestLine(jobName, m))
}

// cacheTrimManifestLine names what a trim applies: the shard ConfigMaps the
// Job mounts, or the one Job that plans its own list in the claim.
func cacheTrimManifestLine(jobName string, m *cachevol.TrimManifest) string {
	if m.Route() == cachevol.TrimRouteClaim {
		return fmt.Sprintf("%s: one Job, which plans its list in the claim", jobName)
	}
	names := make([]string, 0, len(m.Shards()))
	for _, shard := range m.Shards() {
		names = append(names, shard.Metadata.Name)
	}
	return fmt.Sprintf("%s: %d shard ConfigMap(s) (%s), then the Job that mounts them", jobName, len(names), strings.Join(names, ", "))
}

// cacheTrimApply applies the manifest: it deletes what an earlier run left
// behind, applies the documents in order -- the shards before the Job that
// mounts them -- and, once the Job has finished, deletes the shards it read.
//
// The deletions are the applier's alone: nothing else may create or delete a
// shard, so a trim's leftovers go only through the selector the trim's own
// objects carry (gt-ej1yz).
func (d *Daemon) cacheTrimApply(ctx context.Context, kubeconfig, namespace, jobName string, m *cachevol.TrimManifest) error {
	existing, err := d.cacheTrimExisting(ctx, kubeconfig, namespace)
	if err != nil {
		return err
	}
	for _, stale := range cachevol.TrimStale(m, existing) {
		kind := strings.ToLower(stale.Kind)
		if _, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "delete", kind, stale.Name, "-n", namespace, "--ignore-not-found"); err != nil {
			return fmt.Errorf("deleting the stale %s %s: %w", stale.Kind, stale.Name, err)
		}
		d.logger.Printf("disk_maintenance: cache trim %s: deleted the stale %s %s", jobName, stale.Kind, stale.Name)
	}

	docs := m.Documents()
	for i, doc := range docs {
		if _, err := d.cacheTrimKubectl(ctx, kubeconfig, doc, "apply", "-f", "-"); err != nil {
			return fmt.Errorf("applying document %d of %d: %w", i+1, len(docs), err)
		}
	}
	return d.cacheTrimCleanShards(ctx, kubeconfig, namespace, jobName, m)
}

// cacheTrimCleanShards deletes the shard ConfigMaps a trim applied once its Job
// has finished. A Job a trim applies folds in the claim within the pass, and the
// shards are its input alone; the next run would leave them behind otherwise,
// since a rerun of the same trim names the same shards and updates them rather
// than deleting them.
//
// A Job that has not finished by the wait keeps its shards: deleting a mount a
// running pod reads would fail the trim it is part of. The next run deletes the
// Job, and its pods with it, before it applies.
func (d *Daemon) cacheTrimCleanShards(ctx context.Context, kubeconfig, namespace, jobName string, m *cachevol.TrimManifest) error {
	if len(m.Shards()) == 0 {
		return nil
	}
	timeout := strconv.Itoa(int(cacheTrimWaitTimeout.Seconds())) + "s"
	if _, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "wait", "--for=condition=complete", "job/"+jobName, "-n", namespace, "--timeout="+timeout); err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: the Job has not finished (%v); its shards stay for the next run", jobName, err)
		return nil
	}
	if _, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "delete", "configmap", "-n", namespace, "-l", cachevol.TrimSelector()); err != nil {
		return fmt.Errorf("deleting the trim's shards: %w", err)
	}
	return nil
}

// cacheTrimExisting lists the objects a trim applies in namespace: the Jobs and
// shard ConfigMaps carrying TrimSelector, the labels only a trim applies.
//
// A reply without an item list refuses: an unreadable answer would read as "no
// trim ran here", and the next apply's Job would be refused by the running one.
func (d *Daemon) cacheTrimExisting(ctx context.Context, kubeconfig, namespace string) ([]cachevol.TrimObject, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "job,configmap", "-n", namespace, "-l", cachevol.TrimSelector(), "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("listing the trim's objects: %w", err)
	}
	var list struct {
		Items *[]struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decoding the trim's objects: %w", err)
	}
	if list.Items == nil {
		return nil, errors.New("decoding the trim's objects: the reply names no object list")
	}
	objects := make([]cachevol.TrimObject, 0, len(*list.Items))
	for _, item := range *list.Items {
		objects = append(objects, cachevol.TrimObject{Kind: item.Kind, Name: item.Metadata.Name, Labels: item.Metadata.Labels})
	}
	return objects, nil
}

// cacheTrimSource is a pod a listing can be read through: the container that
// mounts the claim and the claim's path inside it.
type cacheTrimSource struct {
	Namespace string
	Pod       string
	Container string
	Root      string
}

// cacheTrimPods reads the pods across the cluster that mount claim, and returns
// how many of them are running -- PlanTrim's sandbox count -- with the pods a
// listing can be read through, in a stable order.
//
// Only a running pod can answer an exec, so only a running pod is a source. A
// reply that cannot be decoded or names no item list refuses: an unreadable
// reply would read as no sandbox running, which is a claim of idle again.
func (d *Daemon) cacheTrimPods(ctx context.Context, kubeconfig, claim string) (int, []cacheTrimSource, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return 0, nil, err
	}
	var list struct {
		Items *[]cacheTrimPod `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return 0, nil, fmt.Errorf("decoding the pod list: %w", err)
	}
	if list.Items == nil {
		return 0, nil, errors.New("decoding the pod list: the reply names no item list")
	}

	running, sources := 0, []cacheTrimSource{}
	for _, pod := range *list.Items {
		volume := cacheTrimVolume(pod, claim)
		if volume == "" {
			continue
		}
		if pod.Status.Phase != "Running" {
			continue
		}
		running++
		if source, ok := cacheTrimPodSource(pod, volume); ok {
			sources = append(sources, source)
		}
	}
	slices.SortFunc(sources, func(a, b cacheTrimSource) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		if c := strings.Compare(a.Pod, b.Pod); c != 0 {
			return c
		}
		return strings.Compare(a.Container, b.Container)
	})
	return running, sources, nil
}

// cacheTrimPod is the part of a pod `kubectl get pods -o json` names that finds
// the claim's mount.
type cacheTrimPod struct {
	Metadata cacheTrimPodMeta `json:"metadata"`
	Spec     cacheTrimPodSpec `json:"spec"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type cacheTrimPodMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type cacheTrimPodSpec struct {
	Containers []cacheTrimContainer `json:"containers"`
	Volumes    []cacheTrimPodVolume `json:"volumes"`
}

type cacheTrimContainer struct {
	Name         string                 `json:"name"`
	VolumeMounts []cacheTrimVolumeMount `json:"volumeMounts"`
}

type cacheTrimVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type cacheTrimPodVolume struct {
	Name                  string                          `json:"name"`
	PersistentVolumeClaim *cacheTrimPersistentVolumeClaim `json:"persistentVolumeClaim"`
}

type cacheTrimPersistentVolumeClaim struct {
	ClaimName string `json:"claimName"`
}

// cacheTrimVolume is the name of the pod's volume that mounts claim, or "" when
// the pod mounts none.
func cacheTrimVolume(pod cacheTrimPod, claim string) string {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return volume.Name
		}
	}
	return ""
}

// cacheTrimPodSource is where the claim sits in the pod's container that mounts
// it: the first container, by name, that mounts the claim's volume.
func cacheTrimPodSource(pod cacheTrimPod, volume string) (cacheTrimSource, bool) {
	containers := slices.Clone(pod.Spec.Containers)
	slices.SortFunc(containers, func(a, b cacheTrimContainer) int { return strings.Compare(a.Name, b.Name) })
	for _, container := range containers {
		for _, mount := range container.VolumeMounts {
			if mount.Name != volume || mount.MountPath == "" {
				continue
			}
			return cacheTrimSource{
				Namespace: pod.Metadata.Namespace,
				Pod:       pod.Metadata.Name,
				Container: container.Name,
				Root:      mount.MountPath,
			}, true
		}
	}
	return cacheTrimSource{}, false
}

// cacheTrimListing reads the claim's listing through a pod that mounts it, with
// the image's trim-list helper, and decodes what PlanTrim decides on.
func (d *Daemon) cacheTrimListing(ctx context.Context, kubeconfig string, source cacheTrimSource) ([]cachevol.Entry, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "exec", "-n", source.Namespace, source.Pod, "-c", source.Container,
		"--", cachevol.HelperPath, "trim-list", "--root", source.Root)
	if err != nil {
		return nil, err
	}
	return cacheTrimParseListing(out)
}

// cacheTrimListingEntry is one entry of the JSON `polecat-image trim-list`
// prints.
type cacheTrimListingEntry struct {
	Path  string    `json:"path"`
	Cache string    `json:"cache"`
	Size  int64     `json:"size"`
	Atime time.Time `json:"atime"`
	Mtime time.Time `json:"mtime"`
}

// cacheTrimParseListing turns the helper's JSON into PlanTrim's entries. A
// cache name it does not know refuses the listing: PlanTrim chooses what to
// remove by the cache, and a name read as the wrong one would admit a module
// file to a trim while a sandbox builds.
func cacheTrimParseListing(out []byte) ([]cachevol.Entry, error) {
	var entries []cacheTrimListingEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("decoding the trim listing: %w", err)
	}
	listing := make([]cachevol.Entry, 0, len(entries))
	for _, entry := range entries {
		var cache cachevol.Cache
		switch entry.Cache {
		case polecatimage.TrimListCacheBuild:
			cache = cachevol.BuildCache
		case polecatimage.TrimListCacheModule:
			cache = cachevol.ModuleCache
		default:
			return nil, fmt.Errorf("the trim listing names the cache %q for %s: want %s or %s",
				entry.Cache, entry.Path, polecatimage.TrimListCacheBuild, polecatimage.TrimListCacheModule)
		}
		listing = append(listing, cachevol.Entry{
			Path:  entry.Path,
			Cache: cache,
			Size:  entry.Size,
			Atime: entry.Atime,
			Mtime: entry.Mtime,
		})
	}
	return listing, nil
}

// cacheTrimKubectl runs one kubectl command through the daemon's exec seam, so
// the unit tier never launches kubectl. stdin, when it is not nil, is the
// document an apply reads.
//
// A failure names the command and its last stderr line, never its whole output,
// which can hold a secret.
func (d *Daemon) cacheTrimKubectl(ctx context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, error) {
	argv := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...) //nolint:gosec // G204: a fixed program, fixed verbs, and a claim name validated as a DNS-1123 label
	cmd.Env = os.Environ()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, lastLine(string(stderr)))
	}
	return stdout, nil
}
