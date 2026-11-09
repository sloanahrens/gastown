package daemon

import (
	"errors"
	"fmt"
)

const (
	// HelperName names the seed helper Pod and its one container.
	HelperName = "seed-helper"
	// HelperRoleLabel marks the helper pod, with HelperRoleValue as its value, so
	// a reader or a test can tell it from the daemon pod.
	HelperRoleLabel = "gt/role"
	HelperRoleValue = HelperName
	// HelperSleepBound is the helper's sleep: the largest 32-bit second count,
	// about 68 years, so the pod stays up for the life of the seed without a loop
	// or a shell to keep it there.
	HelperSleepBound = "2147483647"
	// helperUserID is the uid and gid the helper writes the claims as: root, the
	// identity the daemon runs as. The daemon image's final stage has no USER
	// instruction, so the Deployment's pod inherits root, and the helper pins it
	// because the files the seed writes must belong to the user the daemon reads
	// and rewrites them as. TestHelperRunsAsTheDaemonImageDoes holds the pin to
	// the image.
	helperUserID = 0
)

// Helper is the seed helper Pod: the pod the seed streams `gt town state import`
// into before the daemon starts. The Deployment cannot do this job, because it
// starts the daemon, and the claims must be filled while nothing else holds them.
type Helper struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta `yaml:"metadata"`
	Spec     PodSpec    `yaml:"spec"`
}

// BuildHelper returns the seed helper pod for v, or the reason v cannot be
// rendered: an image that is not a digest, a namespace that is not a DNS-1123
// label, a rehearsal in a namespace the live render owns, or a live render
// whose container the operator has not confirmed stopped.
func BuildHelper(v Values) (Helper, error) {
	namespace, err := v.helperNamespace()
	if err != nil {
		return Helper{}, err
	}
	return Helper{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Pod"},
		Metadata: ObjectMeta{Name: HelperName, Namespace: namespace, Labels: helperLabels()},
		Spec:     helperPod(v),
	}, nil
}

// helperNamespace returns the namespace the helper lands in, or the reason v
// names none this pod may use.
//
// A rehearsal takes the Deployment's namespace rule rather than the confirmation:
// a helper in the live namespace would mount the live town-state claim and write
// it while the Docker Desktop container still owns it, which is the second writer
// the confirmation exists to prevent.
func (v Values) helperNamespace() (string, error) {
	if v.Rehearsal {
		if err := v.validateRehearsalNamespace(); err != nil {
			return "", err
		}
	}
	if v.Namespace != "" && !validDNS1123Label(v.Namespace) {
		return "", fmt.Errorf("Values.Namespace %q: want a DNS-1123 label of at most 63 characters, such as gt", v.Namespace)
	}
	if err := v.validateImage(); err != nil {
		return "", err
	}
	if !v.Rehearsal && !v.ConfirmContainerStopped {
		return "", errors.New("Values.ConfirmContainerStopped is false: the Docker Desktop container owns the town until it is stopped, and the helper would write the live town-state claim beside it, so it is not rendered until the operator confirms the container is down")
	}
	if v.Namespace == "" {
		return DefaultNamespace, nil
	}
	return v.Namespace, nil
}

// helperPod is the helper pod: the daemon image asleep under a bound, with the
// two claims mounted read-write and nothing else. It carries no environment, no
// hostAlias and no Secret, so no Docker-Desktop-only setting, no Dolt name and no
// credential reaches the pod that fills the claims. Its service account token is
// off, because the pod calls no API and the seed reaches it through the kubelet.
func helperPod(v Values) PodSpec {
	noToken := false
	root := int64(helperUserID)
	return PodSpec{
		AutomountServiceAccountToken: &noToken,
		SecurityContext:              &PodSecurityContext{RunAsUser: &root, RunAsGroup: &root},
		Containers: []Container{{
			Name:    HelperName,
			Image:   v.Image,
			Command: []string{"sleep", HelperSleepBound},
			VolumeMounts: []VolumeMount{
				{Name: TownStateClaim, MountPath: TownRootPath},
				{Name: LandingWorkClaim, MountPath: LandingWorkPath},
			},
		}},
		Volumes: []Volume{
			{Name: TownStateClaim, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: TownStateClaim}},
			{Name: LandingWorkClaim, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: LandingWorkClaim}},
		},
	}
}

// helperLabels are the helper pod's labels. They are not labels(): that map
// carries app.kubernetes.io/name, which is exactly what the Deployment selects
// its pods by, and the ReplicaSet would take the helper for one of its own.
func helperLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "gt",
		HelperRoleLabel:                HelperRoleValue,
	}
}
