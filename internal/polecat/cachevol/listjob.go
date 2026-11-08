package cachevol

const (
	// DefaultListJobName is the lister Job's name when the input names none.
	DefaultListJobName = "gt-cache-list"
	// ListJobTTLSeconds is how long a finished lister Job is kept, so the reader can take its log
	// before the cluster deletes the Job and its pod.
	ListJobTTLSeconds = 300
)

// ListJobInput is what the lister Job is built from: the polecat image its container runs, the claim
// it lists, and the pool its pod is placed on.
type ListJobInput struct {
	// Name is the Job's name; DefaultListJobName when empty.
	Name string
	// Namespace is the Job's namespace; empty leaves the choice to the applying command.
	Namespace string
	// Image is the polecat image tag, which carries the trim-list helper.
	Image string
	// ClaimName is the shared cache claim the Job lists, mounted read-only.
	ClaimName string
	// NodeSelector and Tolerations place the pod on the pool that mounts the claim, as the trim Job's
	// placement does.
	NodeSelector map[string]string
	Tolerations  []Toleration
}

// ListManifest is the lister Job: the one object whose log is the claim's listing, which the trim
// parses to decide what to remove.
type ListManifest struct {
	job *SeedJob
}

// Documents returns the lister Job encoded for kubectl apply.
func (m *ListManifest) Documents() [][]byte {
	return [][]byte{m.job.JSON()}
}

// BuildListManifest returns the lister Job for in, or the reason it cannot be rendered.
//
// The Job runs trim-list on the claim and writes nothing: the claim is mounted read-only, and the
// container runs as SeedUserID, the identity the trim and the sandboxes use on the claim, so the
// listing is the one a sandbox would see.
func BuildListManifest(in ListJobInput) (*ListManifest, error) {
	name := in.Name
	if name == "" {
		name = DefaultListJobName
	}
	if err := checkJobIdentity(name, in.Namespace, in.Image, in.ClaimName); err != nil {
		return nil, err
	}
	return &ListManifest{job: listJob(in, name)}, nil
}

// listJob is the lister Job document: the claim read-only, one container running trim-list, and no
// retry, since a failed listing reports itself in the log.
func listJob(in ListJobInput, name string) *SeedJob {
	return &SeedJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    listLabels(),
		},
		Spec: JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(ListJobTTLSeconds),
			Template: PodTemplate{
				Metadata: TemplateMeta{Labels: listLabels()},
				Spec: PodSpec{
					RestartPolicy:   "Never",
					NodeSelector:    in.NodeSelector,
					Tolerations:     in.Tolerations,
					SecurityContext: &PodSecurityContext{RunAsUser: int64Ptr(SeedUserID), RunAsGroup: int64Ptr(SeedUserID)},
					Containers:      []Container{listContainer(in.Image)},
					Volumes: []Volume{
						{Name: trimCacheVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName, ReadOnly: true}},
					},
				},
			},
		},
	}
}

// listContainer runs trim-list on the claim, the command the daemon runs through kubectl exec when a
// pod holds the claim, so the Job's log and that listing are the same output.
func listContainer(image string) Container {
	return Container{
		Name:            "list",
		Image:           image,
		ImagePullPolicy: PullPolicyFor(image),
		Command:         []string{HelperPath, "trim-list"},
		Args:            []string{"--root", SeedCacheRoot},
		VolumeMounts:    []VolumeMount{{Name: trimCacheVolume, MountPath: SeedCacheRoot, ReadOnly: true}},
	}
}

// listLabels are the labels on the lister Job and its pod. They are not trimLabels, so TrimSelector
// and TrimStale never reach the lister Job.
func listLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "polecat-cache-list",
		"app.kubernetes.io/managed-by": "gt",
	}
}
