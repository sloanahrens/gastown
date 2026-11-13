package daemon

import (
	"slices"
	"strings"
	"testing"
)

// TestTrackerBackupReaderMountsTheClaimReadOnly holds the reader to what a copy-out
// needs and no more: the dump claim mounted read-only on the dump image, asleep
// under the helper's bound, never restarted, with no token and no environment.
func TestTrackerBackupReaderMountsTheClaimReadOnly(t *testing.T) {
	t.Parallel()
	r, err := BuildTrackerBackupReader(trackerBackupValues(t))
	if err != nil {
		t.Fatalf("BuildTrackerBackupReader: %v", err)
	}
	if r.Kind != "Pod" || r.Metadata.Name != TrackerBackupReaderName {
		t.Fatalf("reader is a %s %s, want a Pod named %s", r.Kind, r.Metadata.Name, TrackerBackupReaderName)
	}
	if r.Spec.RestartPolicy != "Never" {
		t.Errorf("reader restartPolicy = %q, want Never: a restarted reader would hold the claim", r.Spec.RestartPolicy)
	}
	if r.Spec.AutomountServiceAccountToken == nil || *r.Spec.AutomountServiceAccountToken {
		t.Errorf("reader mounts a service account token: the pod calls no API")
	}
	if len(r.Spec.Containers) != 1 {
		t.Fatalf("reader has %d containers, want 1", len(r.Spec.Containers))
	}
	c := r.Spec.Containers[0]
	if c.Image != testPostgresImage {
		t.Errorf("reader image = %q, want the dump image %q", c.Image, testPostgresImage)
	}
	if want := []string{"sleep", HelperSleepBound}; !slices.Equal(c.Command, want) {
		t.Errorf("reader command = %v, want %v", c.Command, want)
	}
	if len(c.Env) != 0 {
		t.Errorf("reader carries environment %v, want none: no DSN reaches it", c.Env)
	}
	if len(c.VolumeMounts) != 1 {
		t.Fatalf("reader mounts %d volumes, want only the dump claim", len(c.VolumeMounts))
	}
	m := c.VolumeMounts[0]
	if m.Name != TrackerBackupClaim || m.MountPath != TrackerBackupPath {
		t.Errorf("reader mounts %s at %s, want the dump claim at %s", m.Name, m.MountPath, TrackerBackupPath)
	}
	if !m.ReadOnly {
		t.Errorf("reader mounts the dump claim read-write: a copy-out must not write the claim")
	}
	if len(r.Spec.Volumes) != 1 || r.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		r.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != TrackerBackupClaim {
		t.Errorf("reader volumes = %+v, want only the dump claim", r.Spec.Volumes)
	}
}

// TestTrackerBackupReaderRefusesWhatItCannotRender holds the reader to a refusal
// for a values set with no dump, and for one Build would refuse: a dump with no
// tracker, and a dump with no image.
func TestTrackerBackupReaderRefusesWhatItCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(v *Values)
		cause  string
	}{
		{"no dump", func(v *Values) { v.TrackerBackup = false }, "Values.TrackerBackup is not set"},
		{"dump without tracker", func(v *Values) { v.Tracker, v.TrackerDSNSecret = false, "" }, "Values.TrackerBackup is set but Values.Tracker is not"},
		{"dump without image", func(v *Values) { v.TrackerBackupImage = "" }, "Values.TrackerBackupImage is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := trackerBackupValues(t)
			tt.mutate(&v)
			_, err := BuildTrackerBackupReader(v)
			if err == nil {
				t.Fatalf("BuildTrackerBackupReader succeeded, want a refusal naming %q", tt.cause)
			}
			if !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("refusal = %q, want it to name %q", err, tt.cause)
			}
		})
	}
}
