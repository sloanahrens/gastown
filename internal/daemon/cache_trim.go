package daemon

// cacheTrim applies the shared cache claim's trim from the disk_maintenance
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
// The claim's namespace comes from the claim itself, not from a default or from
// a pod: pods of the same claim name in two namespaces are two claims, and each
// is judged by its own namespace's pods alone.
//
// The listing comes from the cluster, not from this host: the claim is mounted
// only by pods, so the pass reads the pods that mount it with kubectl -- the way
// the registry target's in-use provider reads pods -- counts the ones holding
// it as PlanTrim's sandbox count, and asks a running one of them to print the
// listing with the image's trim-list helper.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

	// cacheTrimDeleteTimeout bounds the foreground delete of an earlier run's
	// Job. kubectl returns once the Job and its pods are gone, so the name the
	// rerun applies next is not racing that delete.
	cacheTrimDeleteTimeout = 60 * time.Second

	// cacheTrimKubeconfig is the town's sandbox kubeconfig, relative to the
	// town root, the file the registry target reads the in-use pods through too.
	cacheTrimKubeconfig = ".runtime/sbx-k8s/kubeconfig"

	// cacheTrimJobNameMax is the most a Kubernetes object name holds. A Job
	// name of a longer claim would be refused at apply, so it is shortened in
	// one place (cacheTrimJobName) rather than at every caller.
	cacheTrimJobNameMax = 63

	// cacheTrimJobNameDigest is how many hex characters of the claim's SHA-256
	// a shortened Job name ends with: enough that two long claims sharing a
	// prefix still get two names.
	cacheTrimJobNameDigest = 8
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
//
// A claim failing this check is refused, never normalized: an uppercase letter
// or an underscore stops the trim rather than becoming a hyphen, so the pass
// never trims a claim other than the one the town named.
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

// cacheTrimJobName is a claim's trim Job name: the trim prefix and the claim
// when the two fit a Kubernetes object name, and otherwise the prefix, as much
// of the claim as fits, and eight hex characters of the claim's SHA-256, so two
// long claims sharing a prefix still get two names.
//
// The claim reaches here after validTrimClaim, so it is already a DNS-1123
// label; the shortening keeps that form because it cuts at a character
// boundary and drops any hyphen the cut left at the end.
func cacheTrimJobName(claim string) string {
	name := cachevol.DefaultTrimJobName + "-" + claim
	if len(name) <= cacheTrimJobNameMax {
		return name
	}
	sum := sha256.Sum256([]byte(claim))
	digest := hex.EncodeToString(sum[:])[:cacheTrimJobNameDigest]
	prefix := cachevol.DefaultTrimJobName + "-"
	keep := cacheTrimJobNameMax - len(prefix) - 1 - len(digest)
	base := strings.TrimRight(claim[:keep], "-")
	return prefix + base + "-" + digest
}

// cacheTrim is the pass's cache trim: each claim a kubernetes sandbox profile
// mounts, in each namespace that holds it, trimmed in the mode the patrol runs
// in.
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
	namespaces, err := d.cacheTrimClaimNamespaces(ctx, kubeconfig)
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim: the cluster's claims could not be read: %v; nothing trimmed", err)
		return
	}
	// trimmed is the claim each namespace this pass trims for. A trim's labels
	// are the same for every claim, so two claims trimming in one namespace
	// would delete each other's objects; the second is refused.
	trimmed := map[string]string{}
	for _, claim := range claims {
		found := namespaces[claim.ClaimName]
		if len(found) == 0 {
			d.logger.Printf("disk_maintenance: cache trim %s: no claim of that name is in the cluster; nothing trimmed", claim.ClaimName)
			continue
		}
		for _, namespace := range found {
			d.cacheTrimClaim(ctx, kubeconfig, claim, namespace, capBytes, mode == agentconfig.DiskMaintenanceModeApply, trimmed)
		}
	}
}

// cacheTrimClaim trims one claim in one namespace: it reads the listing through
// a pod that mounts the claim there, builds the trim, and in apply mode applies
// it. trimmed is the namespace each claim this pass already trims in, which the
// claim joins.
func (d *Daemon) cacheTrimClaim(ctx context.Context, kubeconfig string, claim cacheTrimClaim, namespace string, capBytes int64, apply bool, trimmed map[string]string) {
	sandboxes, sources, err := d.cacheTrimPods(ctx, kubeconfig, namespace, claim.ClaimName)
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: the pods that mount it could not be read: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return
	}
	if len(sources) == 0 {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: no running pod mounts it, so its listing cannot be read (%d pod(s) hold it); nothing trimmed", claim.ClaimName, namespace, sandboxes)
		return
	}
	listing, err := d.cacheTrimListing(ctx, kubeconfig, sources[0])
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: the listing could not be read: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return
	}

	// The Job is named after the claim, in the claim's own namespace, so two
	// claims get two Jobs and a rerun of a trim overwrites its own rather than a
	// neighbour's.
	jobName := cacheTrimJobName(claim.ClaimName)
	if other, taken := trimmed[namespace]; taken {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: the claim %s is trimmed in the same namespace, and a trim's labels cannot tell the two sets of objects apart; nothing trimmed", claim.ClaimName, namespace, other)
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
		SandboxesRunning: sandboxes,
	})
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return
	}
	if !apply {
		d.logger.Printf("disk_maintenance: cache trim %s in %s dry_run: %s", claim.ClaimName, namespace, cacheTrimManifestLine(jobName, m))
		return
	}
	if err := d.cacheTrimApply(ctx, kubeconfig, namespace, jobName, m); err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s in %s: the apply failed: %v", claim.ClaimName, namespace, err)
		return
	}
	d.logger.Printf("disk_maintenance: cache trim %s in %s applied: %s", claim.ClaimName, namespace, cacheTrimManifestLine(jobName, m))
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
	existing, running, err := d.cacheTrimExisting(ctx, kubeconfig, namespace)
	if err != nil {
		return err
	}
	stale := cachevol.TrimStale(m, existing)
	// An earlier Job that still runs is left alone, and this trim waits for the
	// next pass: deleting it would stop the trim its own pods are in the middle
	// of, and applying over it is refused because its template is immutable.
	for _, o := range stale {
		if o.Kind == "Job" && running[o.Name] {
			return fmt.Errorf("the earlier trim Job %s is still running; its pods mount the claim, so it is left to finish", o.Name)
		}
	}
	for _, object := range stale {
		kind := strings.ToLower(object.Kind)
		args := []string{"delete", kind, object.Name, "-n", namespace, "--ignore-not-found"}
		if kind == "job" {
			// Foreground and bounded: kubectl returns once the Job and its pods
			// are gone, so the same-named Job applied next does not race the
			// delete (gt-tjhfe).
			timeout := strconv.Itoa(int(cacheTrimDeleteTimeout.Seconds())) + "s"
			args = append(args, "--wait=true", "--timeout="+timeout)
		}
		if _, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, args...); err != nil {
			return fmt.Errorf("deleting the stale %s %s: %w", object.Kind, object.Name, err)
		}
		d.logger.Printf("disk_maintenance: cache trim %s: deleted the stale %s %s", jobName, object.Kind, object.Name)
	}

	docs := m.Documents()
	for i, doc := range docs {
		// -n names the namespace the document was built for; an apply that
		// relied on the document alone would run in the caller's default
		// namespace.
		if _, err := d.cacheTrimKubectl(ctx, kubeconfig, doc, "apply", "-f", "-", "-n", namespace); err != nil {
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
	_, stderr, err := d.cacheTrimKubectlRun(ctx, kubeconfig, nil, "wait", "--for=condition=complete", "job/"+jobName, "-n", namespace, "--timeout="+timeout)
	if err != nil {
		d.logger.Printf("disk_maintenance: cache trim %s: %s; its shards stay for the next run", jobName, d.cacheTrimWaitFailure(ctx, kubeconfig, namespace, jobName, stderr))
		return nil
	}
	if _, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "delete", "configmap", "-n", namespace, "-l", cachevol.TrimSelector()); err != nil {
		return fmt.Errorf("deleting the trim's shards: %w", err)
	}
	return nil
}

// cacheTrimWaitFailure says why the wait ended without the Job completing: the
// Job failed, the wait timed out, or kubectl could not watch the Job. Each is a
// different repair, so each is a different line.
//
// Every outcome leaves the shards in place, so a status that cannot be read
// only costs the line its precision; it does not change what the pass does.
func (d *Daemon) cacheTrimWaitFailure(ctx context.Context, kubeconfig, namespace, jobName string, stderr []byte) string {
	if d.cacheTrimJobFailed(ctx, kubeconfig, namespace, jobName) {
		return "the Job failed"
	}
	if bytes.Contains(stderr, []byte("timed out waiting for the condition")) {
		return "the Job has not finished within the wait (timed out)"
	}
	return "the wait on the Job failed"
}

// cacheTrimJobFailed reports whether the Job's own status says it failed: a
// failed count, or a Failed condition set true.
func (d *Daemon) cacheTrimJobFailed(ctx context.Context, kubeconfig, namespace, jobName string) bool {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "job", jobName, "-n", namespace, "-o", "json")
	if err != nil {
		return false
	}
	var job struct {
		Status struct {
			Failed     int `json:"failed"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &job) != nil {
		return false
	}
	if job.Status.Failed > 0 {
		return true
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			return true
		}
	}
	return false
}

// cacheTrimExisting lists the objects a trim applies in namespace: the Jobs and
// shard ConfigMaps carrying TrimSelector, the labels only a trim applies, and
// which of the Jobs still run.
//
// A reply without an item list refuses: an unreadable answer would read as "no
// trim ran here", and the next apply's Job would be refused by the running one.
func (d *Daemon) cacheTrimExisting(ctx context.Context, kubeconfig, namespace string) ([]cachevol.TrimObject, map[string]bool, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "job,configmap", "-n", namespace, "-l", cachevol.TrimSelector(), "-o", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("listing the trim's objects: %w", err)
	}
	var list struct {
		Items *[]struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Active int `json:"active"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, nil, fmt.Errorf("decoding the trim's objects: %w", err)
	}
	if list.Items == nil {
		return nil, nil, errors.New("decoding the trim's objects: the reply names no object list")
	}
	objects := make([]cachevol.TrimObject, 0, len(*list.Items))
	running := map[string]bool{}
	for _, item := range *list.Items {
		objects = append(objects, cachevol.TrimObject{Kind: item.Kind, Name: item.Metadata.Name, Labels: item.Metadata.Labels})
		if item.Kind == "Job" && item.Status.Active > 0 {
			running[item.Metadata.Name] = true
		}
	}
	return objects, running, nil
}

// cacheTrimClaimNamespaces reads the cluster's PersistentVolumeClaims and
// returns, per claim name, the namespaces that hold one, sorted. The namespace
// a trim runs in is read from the claim item itself: a pod that mounts a claim
// of the same name in another namespace is not this claim's sandbox.
//
// This is the pass's one cluster-wide read; every call after it names the
// namespace it found here. A reply that cannot be decoded or names no item list
// refuses: an unreadable answer would read as "no such claim", and the trim
// would never run.
func (d *Daemon) cacheTrimClaimNamespaces(ctx context.Context, kubeconfig string) (map[string][]string, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "persistentvolumeclaim", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items *[]struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decoding the claim list: %w", err)
	}
	if list.Items == nil {
		return nil, errors.New("decoding the claim list: the reply names no item list")
	}
	namespaces := map[string][]string{}
	for _, item := range *list.Items {
		if item.Metadata.Name == "" || item.Metadata.Namespace == "" {
			return nil, errors.New("decoding the claim list: an item names no name or no namespace")
		}
		namespaces[item.Metadata.Name] = append(namespaces[item.Metadata.Name], item.Metadata.Namespace)
	}
	for _, found := range namespaces {
		slices.Sort(found)
	}
	return namespaces, nil
}

// cacheTrimSource is a pod a listing can be read through: the container that
// mounts the claim and the claim's path inside it.
type cacheTrimSource struct {
	Namespace string
	Pod       string
	Container string
	Root      string
}

// cacheTrimPods reads the pods in namespace that mount claim, and returns how
// many of them hold it -- PlanTrim's sandbox count -- with the pods a listing
// can be read through, in a stable order.
//
// A pod that has finished holds nothing: it runs no sandbox and cannot be
// exec'd, so it is neither counted nor a source. A Pending pod counts, because
// a sandbox that has not started still holds the cache; only a running pod can
// answer an exec, so only a running pod is a source.
//
// A reply that cannot be decoded, names no item list, or names a pod of another
// namespace refuses: an unreadable reply would read as no sandbox running, which
// is a claim of idle again.
func (d *Daemon) cacheTrimPods(ctx context.Context, kubeconfig, namespace, claim string) (int, []cacheTrimSource, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, nil, "get", "pods", "-n", namespace, "-o", "json")
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

	sandboxes, sources := 0, []cacheTrimSource{}
	for _, pod := range *list.Items {
		if pod.Metadata.Namespace != "" && pod.Metadata.Namespace != namespace {
			return 0, nil, fmt.Errorf("the pod list for namespace %s names the pod %s in %s", namespace, pod.Metadata.Name, pod.Metadata.Namespace)
		}
		volume := cacheTrimVolume(pod, claim)
		if volume == "" {
			continue
		}
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		sandboxes++
		if pod.Status.Phase != "Running" {
			continue
		}
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
	return sandboxes, sources, nil
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

// cacheTrimKubectlEnvKeys are the only variables a trim's kubectl runs with:
// PATH and HOME for the binary and its cache, and KUBECONFIG for a config the
// flag does not name.
var cacheTrimKubectlEnvKeys = []string{"PATH", "HOME", "KUBECONFIG"}

// cacheTrimKubectlEnv is the environment kubectl runs with. The daemon's whole
// environment stays out: a cluster client has no use for the town's settings,
// and every variable named here is one a kubeconfig and a binary need.
//
// The registry target's in-use read passes no variable of its own through the
// same runner (internal/diskmaint/registry/inuse.go), so these three are the
// whole contract.
func cacheTrimKubectlEnv(environ []string) []string {
	env := make([]string, 0, len(cacheTrimKubectlEnvKeys))
	for _, key := range cacheTrimKubectlEnvKeys {
		if value, ok := environValue(environ, key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// environValue returns key's value in environ, the last entry winning as it
// would for a process.
func environValue(environ []string, key string) (string, bool) {
	for _, kv := range slices.Backward(environ) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// cacheTrimKubectl runs one kubectl command through the daemon's exec seam, so
// the unit tier never launches kubectl. stdin, when it is not nil, is the
// document an apply reads.
//
// A failure names the command and its last stderr line, never its whole output,
// which can hold a secret.
func (d *Daemon) cacheTrimKubectl(ctx context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, error) {
	stdout, stderr, err := d.cacheTrimKubectlRun(ctx, kubeconfig, stdin, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, lastLine(string(stderr)))
	}
	return stdout, nil
}

// cacheTrimKubectlRun runs one kubectl command and returns its streams too, for
// the waiter that tells a timeout from a Job that failed.
func (d *Daemon) cacheTrimKubectlRun(ctx context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, []byte, error) {
	argv := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...) //nolint:gosec // G204: a fixed program, fixed verbs, and names validated as DNS-1123 labels
	cmd.Env = cacheTrimKubectlEnv(d.diskMaintenanceSeams().environ())
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return d.runCmd(cmd)
}
