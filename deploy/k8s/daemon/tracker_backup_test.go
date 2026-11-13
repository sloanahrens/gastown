package daemon

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// testPostgresImage is the postgres image reference the tracker-backup goldens
// record, shaped as the caller's digest is: a repository and a digest, never a tag.
const testPostgresImage = "registry.example.com/library/postgres@sha256:" +
	"5e6f7a8b9c0d1e2f5e6f7a8b9c0d1e2f5e6f7a8b9c0d1e2f5e6f7a8b9c0d1e2f"

// trackerBackupValues is the configuration the tracker-backup goldens record: the
// tracker values with the dump on, and the postgres image the dump runs in.
func trackerBackupValues(t *testing.T) Values {
	t.Helper()
	v := trackerValues(t)
	v.TrackerBackup = true
	v.TrackerBackupImage = testPostgresImage
	return v
}

// TestTrackerBackupGolden holds the claim and the CronJob the dump adds to their
// goldens, built from trackerBackupValues.
func TestTrackerBackupGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, trackerBackupValues(t))
	dir := filepath.Join(packageDir(t), "testdata")
	compareGolden(t, dir, "tracker-backup-claim.yaml", document(t, m.TrackerBackupClaim))
	compareGolden(t, dir, "cronjob-tracker-backup.yaml", document(t, m.TrackerBackupCronJob))
}

// TestTrackerBackupAbsentUnlessSet holds that a render without Values.TrackerBackup
// writes neither the claim nor the CronJob, so no empty document leaks into the
// stream.
func TestTrackerBackupAbsentUnlessSet(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, trackerValues(t))
	if m.TrackerBackupClaim.Kind != "" {
		t.Errorf("Values.TrackerBackup is unset but the render carries a %s %s", m.TrackerBackupClaim.Kind, m.TrackerBackupClaim.Metadata.Name)
	}
	if m.TrackerBackupCronJob.Kind != "" {
		t.Errorf("Values.TrackerBackup is unset but the render carries a %s %s", m.TrackerBackupCronJob.Kind, m.TrackerBackupCronJob.Metadata.Name)
	}
	var out bytes.Buffer
	if err := Render(&out, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out.String(), "kind: CronJob") {
		t.Errorf("Values.TrackerBackup is unset but the stream carries a CronJob")
	}
}

// TestTrackerBackupDefaults holds the storage and schedule an unnamed value takes.
func TestTrackerBackupDefaults(t *testing.T) {
	t.Parallel()
	v := trackerBackupValues(t)
	v.TrackerBackupStorage = ""
	v.TrackerBackupSchedule = ""
	m := mustBuild(t, v)
	if got := m.TrackerBackupClaim.Spec.Resources.Requests["storage"]; got != DefaultTrackerBackupStorage {
		t.Errorf("the tracker-backup claim requests %q, want the default %q", got, DefaultTrackerBackupStorage)
	}
	if got := m.TrackerBackupCronJob.Spec.Schedule; got != DefaultTrackerBackupSchedule {
		t.Errorf("the tracker-backup schedule = %q, want the default %q", got, DefaultTrackerBackupSchedule)
	}
}

// TestTrackerBackupRunPolicy holds the CronJob's run policy: no overlapping runs,
// one successful and three failed Jobs kept, one retry, a 1800 second deadline, a
// pod that is never restarted in place, and no ServiceAccount token.
func TestTrackerBackupRunPolicy(t *testing.T) {
	t.Parallel()
	cj := mustBuild(t, trackerBackupValues(t)).TrackerBackupCronJob
	if cj.APIVersion != "batch/v1" || cj.Kind != "CronJob" || cj.Metadata.Name != TrackerBackupName || cj.Metadata.Namespace != DefaultNamespace {
		t.Fatalf("the tracker backup = %s %s %s/%s, want CronJob batch/v1 %s/%s", cj.APIVersion, cj.Kind, cj.Metadata.Namespace, cj.Metadata.Name, DefaultNamespace, TrackerBackupName)
	}
	if cj.Spec.ConcurrencyPolicy != "Forbid" {
		t.Errorf("the tracker backup concurrencyPolicy = %q, want Forbid: two dumps must never overlap", cj.Spec.ConcurrencyPolicy)
	}
	if cj.Spec.SuccessfulJobsHistoryLimit != 1 || cj.Spec.FailedJobsHistoryLimit != 3 {
		t.Errorf("the tracker backup keeps %d successful and %d failed Jobs, want 1 and 3", cj.Spec.SuccessfulJobsHistoryLimit, cj.Spec.FailedJobsHistoryLimit)
	}
	job := cj.Spec.JobTemplate.Spec
	if job.BackoffLimit != 1 {
		t.Errorf("the tracker backup backoffLimit = %d, want 1", job.BackoffLimit)
	}
	if job.ActiveDeadlineSeconds != 1800 {
		t.Errorf("the tracker backup activeDeadlineSeconds = %d, want 1800", job.ActiveDeadlineSeconds)
	}
	pod := job.Template.Spec
	if pod.RestartPolicy != "Never" {
		t.Errorf("the tracker backup pod restartPolicy = %q, want Never: the Job's backoffLimit counts a failed pod", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("the tracker backup pod sets automountServiceAccountToken to %v, want false", pod.AutomountServiceAccountToken)
	}
	if pod.SecurityContext != nil {
		t.Errorf("the tracker backup pod sets a securityContext %+v, want none: the postgres image runs as root", *pod.SecurityContext)
	}
}

// TestTrackerBackupContainers holds the two containers: an init container from the
// daemon image that copies trackerbackup into the emptyDir, and the main container
// from the postgres image that runs it against the claim.
func TestTrackerBackupContainers(t *testing.T) {
	t.Parallel()
	v := trackerBackupValues(t)
	pod := mustBuild(t, v).TrackerBackupCronJob.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || len(pod.Containers) != 1 {
		t.Fatalf("the tracker backup pod has %d init and %d regular containers, want one of each", len(pod.InitContainers), len(pod.Containers))
	}
	initCtr := pod.InitContainers[0]
	if initCtr.Name != TrackerBackupCopyContainer || initCtr.Image != v.Image {
		t.Errorf("the init container = %s on %s, want %s on the daemon image %s", initCtr.Name, initCtr.Image, TrackerBackupCopyContainer, v.Image)
	}
	if want := []string{"cp", "-p", "/usr/local/bin/trackerbackup", trackerBackupBinary}; !slices.Equal(initCtr.Command, want) {
		t.Errorf("the init container command = %q, want %q", initCtr.Command, want)
	}
	if want := []VolumeMount{{Name: TrackerBackupBinVolume, MountPath: TrackerBackupBinPath}}; !slices.Equal(initCtr.VolumeMounts, want) {
		t.Errorf("the init container mounts %+v, want %+v", initCtr.VolumeMounts, want)
	}

	dump := pod.Containers[0]
	if dump.Name != TrackerBackupDumpContainer || dump.Image != v.TrackerBackupImage {
		t.Errorf("the dump container = %s on %s, want %s on the postgres image %s", dump.Name, dump.Image, TrackerBackupDumpContainer, v.TrackerBackupImage)
	}
	if want := []string{trackerBackupBinary}; !slices.Equal(dump.Command, want) {
		t.Errorf("the dump command = %q, want %q: Command replaces the postgres entrypoint", dump.Command, want)
	}
	wantArgs := []string{"-dir", "/backups", "-keep", "7", "-pg-bin", "/usr/lib/postgresql/17/bin"}
	if !slices.Equal(dump.Args, wantArgs) {
		t.Errorf("the dump args = %q, want %q", dump.Args, wantArgs)
	}
	wantMounts := []VolumeMount{
		{Name: TrackerBackupClaim, MountPath: TrackerBackupPath},
		{Name: TrackerBackupBinVolume, MountPath: TrackerBackupBinPath, ReadOnly: true},
	}
	if !slices.Equal(dump.VolumeMounts, wantMounts) {
		t.Errorf("the dump mounts %+v, want %+v: the claim writable, the binary read-only", dump.VolumeMounts, wantMounts)
	}
}

// TestTrackerBackupDSNOnlyFromTheSecret holds the DSN to one route: the environment
// variable trackerbackup reads, taken from the dsn key of the DSN Secret. The init
// container carries no environment at all, and the dump's arguments carry none.
func TestTrackerBackupDSNOnlyFromTheSecret(t *testing.T) {
	t.Parallel()
	v := trackerBackupValues(t)
	pod := mustBuild(t, v).TrackerBackupCronJob.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.InitContainers[0].Env) != 0 {
		t.Errorf("the init container carries environment %+v, want none", pod.InitContainers[0].Env)
	}
	env := pod.Containers[0].Env
	if len(env) != 1 || env[0].Name != TrackerDSNEnv {
		t.Fatalf("the dump environment = %+v, want %s alone", env, TrackerDSNEnv)
	}
	if env[0].Value != "" {
		t.Errorf("the %s entry carries the literal value %q beside its Secret reference", TrackerDSNEnv, env[0].Value)
	}
	sel := env[0].ValueFrom.SecretKeyRef
	if sel == nil || sel.Name != v.TrackerDSNSecret || sel.Key != TrackerDSNKey {
		t.Errorf("the %s secretKeyRef = %+v, want %s/%s", TrackerDSNEnv, sel, v.TrackerDSNSecret, TrackerDSNKey)
	}
}

// TestTrackerBackupVolumesHoldNoHostPath holds the pod to the claim and the emptyDir
// alone: no host path reaches the Mac, and the claim is the dumps' only durable
// volume.
func TestTrackerBackupVolumesHoldNoHostPath(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, trackerBackupValues(t)).TrackerBackupCronJob.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.Volumes) != 2 {
		t.Fatalf("the tracker backup pod mounts %d volumes (%+v), want the claim and the binary's emptyDir", len(pod.Volumes), pod.Volumes)
	}
	claimVol, binVol := pod.Volumes[0], pod.Volumes[1]
	if claimVol.PersistentVolumeClaim == nil || claimVol.PersistentVolumeClaim.ClaimName != TrackerBackupClaim {
		t.Errorf("the first volume = %+v, want the %s claim", claimVol, TrackerBackupClaim)
	}
	if binVol.EmptyDir == nil || binVol.Name != TrackerBackupBinVolume {
		t.Errorf("the second volume = %+v, want the %s emptyDir", binVol, TrackerBackupBinVolume)
	}
	for _, vol := range pod.Volumes {
		if vol.HostPath != nil {
			t.Errorf("the tracker backup pod has hostPath %s at volume %s", vol.HostPath.Path, vol.Name)
		}
	}
}

// TestTrackerBackupClaim holds the claim the dumps land on: the name the main
// container mounts, the daemon's storage class and the size the caller names.
func TestTrackerBackupClaim(t *testing.T) {
	t.Parallel()
	v := trackerBackupValues(t)
	v.TrackerBackupStorage = "12Gi"
	c := mustBuild(t, v).TrackerBackupClaim
	if c.Kind != "PersistentVolumeClaim" || c.Metadata.Name != TrackerBackupClaim || c.Metadata.Namespace != DefaultNamespace {
		t.Fatalf("the tracker-backup claim = %s %s/%s, want %s in %s", c.Kind, c.Metadata.Namespace, c.Metadata.Name, TrackerBackupClaim, DefaultNamespace)
	}
	if c.Spec.StorageClassName != DefaultStorageClass {
		t.Errorf("the tracker-backup claim class = %q, want the default %q", c.Spec.StorageClassName, DefaultStorageClass)
	}
	if got := c.Spec.Resources.Requests["storage"]; got != "12Gi" {
		t.Errorf("the tracker-backup claim requests %q, want the caller's 12Gi", got)
	}
}

// TestTrackerBackupRefusals drives each refusal the backup values have: no tracker,
// no DSN Secret, no postgres image, and a size outside the grammar. Each names the
// value at fault.
func TestTrackerBackupRefusals(t *testing.T) {
	t.Parallel()
	noTracker := trackerBackupValues(t)
	noTracker.Tracker = false
	// No DSN either, so validateTracker's own refusal of a DSN with no tracker
	// cannot stand in for the backup's.
	noTracker.TrackerDSNSecret = ""
	noDSN := trackerBackupValues(t)
	noDSN.TrackerDSNSecret = ""
	noImage := trackerBackupValues(t)
	noImage.TrackerBackupImage = ""
	badStorage := trackerBackupValues(t)
	badStorage.TrackerBackupStorage = "5GB"
	for _, tc := range []struct {
		name string
		v    Values
		want string
	}{
		{"no tracker", noTracker, "Values.Tracker is not"},
		{"no DSN Secret", noDSN, "Values.TrackerDSNSecret is empty"},
		{"no postgres image", noImage, "Values.TrackerBackupImage is empty"},
		{"a size outside the grammar", badStorage, "Values.TrackerBackupStorage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Build(tc.v)
			if err == nil {
				t.Fatalf("Build accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Build refused %s with %q, want the error to name %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestTrackerBackupRenderOrder holds where the dump's objects fall in the stream:
// the claim among the claims, and the CronJob after the tracker Deployment.
func TestTrackerBackupRenderOrder(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, trackerBackupValues(t))
	var out bytes.Buffer
	if err := Render(&out, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got []string
	dec := yaml.NewDecoder(&out)
	for {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the stream: %v", err)
		}
		got = append(got, obj.Kind+"/"+obj.Metadata.Name)
	}
	want := []string{
		"Namespace/" + DefaultNamespace,
		"ServiceAccount/" + ServiceAccountName,
		"PersistentVolumeClaim/" + TownStateClaim,
		"PersistentVolumeClaim/" + LandingWorkClaim,
		"PersistentVolumeClaim/" + DoltBackupClaim,
		"PersistentVolumeClaim/" + TrackerBackupClaim,
		"Service/" + ForgejoName,
		"Service/" + TrackerName,
	}
	for _, file := range m.Secrets {
		want = append(want, "Secret/"+file.Metadata.Name)
	}
	want = append(want,
		"Deployment/"+Name,
		"Deployment/"+TrackerName,
		"CronJob/"+TrackerBackupName,
		"RoleBinding/"+m.SweepRoleBinding.Metadata.Name,
		"RoleBinding/"+m.CacheTrimRoleBinding.Metadata.Name,
		"ClusterRoleBinding/"+m.SweepReaderBinding.Metadata.Name,
	)
	if !slices.Equal(got, want) {
		t.Errorf("Render wrote %q, want %q", got, want)
	}
}
