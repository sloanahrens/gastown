package daemon

import "errors"

// TrackerBackupReaderName names the reader Pod, the pod `kubectl exec` runs in to
// list the dumps and copy one out of the claim.
const TrackerBackupReaderName = "tracker-backup-reader"

// TrackerBackupReader is the reader Pod: the dump image asleep under a bound, with
// the dump claim mounted read-only. The CronJob's pods exit when a run ends, so
// nothing else keeps the claim up to copy from. Like the seed helper, the render
// writes it and does not apply it.
type TrackerBackupReader struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta `yaml:"metadata"`
	Spec     PodSpec    `yaml:"spec"`
}

// BuildTrackerBackupReader returns the reader Pod for v, or the reason v cannot
// render one: a v without the dump, or one Build would refuse.
func BuildTrackerBackupReader(v Values) (TrackerBackupReader, error) {
	if !v.TrackerBackup {
		return TrackerBackupReader{}, errors.New("Values.TrackerBackup is not set: the reader mounts the dump claim, which renders only with it")
	}
	v = v.withDefaults()
	if err := v.validate(); err != nil {
		return TrackerBackupReader{}, err
	}
	namespace := v.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return TrackerBackupReader{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Pod"},
		Metadata: ObjectMeta{Name: TrackerBackupReaderName, Namespace: namespace, Labels: trackerBackupReaderLabels()},
		Spec:     trackerBackupReaderPod(v),
	}, nil
}

// trackerBackupReaderPod is the reader's spec: the dump image running sleep, with
// the claim mounted at TrackerBackupPath read-only. It runs once and is never
// restarted, and it carries no environment and no Secret, so no DSN reaches it.
// Its service account token is off, because the pod calls no API.
func trackerBackupReaderPod(v Values) PodSpec {
	noToken := false
	return PodSpec{
		AutomountServiceAccountToken: &noToken,
		RestartPolicy:                "Never",
		Containers: []Container{{
			Name:         TrackerBackupReaderName,
			Image:        v.TrackerBackupImage,
			Command:      []string{"sleep", HelperSleepBound},
			VolumeMounts: []VolumeMount{{Name: TrackerBackupClaim, MountPath: TrackerBackupPath, ReadOnly: true}},
		}},
		Volumes: []Volume{{Name: TrackerBackupClaim, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: TrackerBackupClaim}}},
	}
}

// trackerBackupReaderLabels name the reader, not the daemon, so the daemon's
// selector never matches it.
func trackerBackupReaderLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "gt",
		HelperRoleLabel:                TrackerBackupReaderName,
	}
}
