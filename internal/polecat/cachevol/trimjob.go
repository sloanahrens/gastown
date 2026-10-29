package cachevol

import (
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
)

// TrimJobInput is what the trim Job is built from: the polecat image its container runs, the claim
// it trims, and the listing PlanTrim decides on.
type TrimJobInput struct {
	// Name is the Job's name; DefaultTrimJobName when empty.
	Name string
	// Namespace is the Job's namespace; empty leaves the choice to the applying command.
	Namespace string
	// Image is the polecat image tag, which carries rm.
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

// BuildTrimJob returns the trim Job for in, or the reason it cannot be rendered.
//
// The removal list is PlanTrim's, so the Job deletes what the rules allow and nothing else: the
// container runs as SeedUserID with the claim at SeedCacheRoot, so it can remove only what a
// sandbox could write.
func BuildTrimJob(in TrimJobInput) (*TrimJob, error) {
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

	plan := PlanTrim(in.Listing, in.CapBytes, in.SandboxesRunning > 0)
	if len(plan.Removals) == 0 {
		return nil, fmt.Errorf("the trim Job on %s would remove nothing: the listing is %d bytes, and the cap is %d", in.ClaimName, plan.Total, in.CapBytes)
	}
	paths, err := claimPaths(plan.Removals)
	if err != nil {
		return nil, err
	}

	// The caller raises the alarm from the same plan; the annotation puts the fact on the object,
	// where an operator reading the Job later finds it.
	var annotations map[string]string
	if plan.Overflow {
		annotations = map[string]string{TrimOverflowAnnotation: strconv.FormatInt(plan.OverBytes, 10)}
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
					Containers: []Container{removeContainer(in.Image, paths)},
					Volumes: []Volume{
						{Name: "cache", PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName}},
					},
				},
			},
		},
	}, nil
}

// trimLabels are the labels on the Job and its pods: what a selector matches and what a cleanup
// removes.
func trimLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "polecat-cache-trim",
		"app.kubernetes.io/managed-by": "gt",
	}
}

// removeContainer deletes the planned files in one rm argv. No shell sees the paths, so each names
// exactly the file the listing did, and -f leaves a file another run already removed a success.
func removeContainer(image string, paths []string) Container {
	return Container{
		Name:         "trim",
		Image:        image,
		Command:      append([]string{"rm", "-f", "--"}, paths...),
		VolumeMounts: []VolumeMount{{Name: "cache", MountPath: SeedCacheRoot}},
	}
}

// claimPaths places each removal under the claim's mount. A path that is absolute, not clean or
// above the claim root could name a file the listing did not, so it is refused rather than joined.
func claimPaths(removals []Removal) ([]string, error) {
	out := make([]string, 0, len(removals))
	for _, r := range removals {
		if !claimRelative(r.Path) {
			return nil, fmt.Errorf("the trim plan names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", r.Path)
		}
		out = append(out, path.Join(SeedCacheRoot, r.Path))
	}
	return out, nil
}

// claimRelative reports whether p is a path a walk of the claim gives: relative to the claim root,
// already clean, and below it rather than at it or above it.
func claimRelative(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	first, _, _ := strings.Cut(p, "/")
	return first != "." && first != ".."
}
