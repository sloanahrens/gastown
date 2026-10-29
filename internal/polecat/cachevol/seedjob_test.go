package cachevol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const cloneURL = "http://forgejo:3000/town/gastown.git"

// seedInput is an input that builds: the tests vary one field at a time.
func seedInput() SeedJobInput {
	return SeedJobInput{
		Namespace: "gt",
		Image:     "gastown-polecat:dev",
		ClaimName: "gt-shared-cache",
		Pins: []Pin{
			{Rig: "gastown", Commit: sha, URL: cloneURL},
			{Rig: "devops", Commit: strings.Repeat("ab", 20), URL: "http://forgejo:3000/town/devops.git"},
		},
	}
}

// TestBuildSeedJobRunsAsRootOnlyInItsPodContext pins the Job's identity: the pod runs as root so the
// seed can hand the claim over, and no container carries an identity of its own.
func TestBuildSeedJobRunsAsRootOnlyInItsPodContext(t *testing.T) {
	t.Parallel()
	job, err := BuildSeedJob(seedInput())
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Errorf("apiVersion/kind = %q/%q, want batch/v1 and Job", job.APIVersion, job.Kind)
	}
	if job.Metadata.Name != DefaultSeedJobName || job.Metadata.Namespace != "gt" {
		t.Errorf("metadata = %+v, want %s in gt", job.Metadata, DefaultSeedJobName)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed seed reports itself once", job.Spec.BackoffLimit)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never", got)
	}
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 || sc.RunAsGroup == nil || *sc.RunAsGroup != 0 {
		t.Fatalf("pod securityContext = %+v, want runAsUser and runAsGroup 0", sc)
	}
	if n := len(job.Spec.Template.Spec.InitContainers) + len(job.Spec.Template.Spec.Containers); n != 2 {
		t.Fatalf("containers = %d, want the clone init container and the seed container", n)
	}
	// The rendered document is where the absence matters: a container that names its own identity
	// would override the pod's.
	doc := job.JSON()
	if n := strings.Count(string(doc), `"securityContext"`); n != 1 {
		t.Errorf("the document carries %d securityContext blocks, want the pod's alone", n)
	}
}

// TestBuildSeedJobMountsTheClaimAndPutsTheCachesUnderIt pins the seed container's mounts and env:
// the claim at /cache, GOCACHE and GOMODCACHE under it, and the checkouts at /seed.
func TestBuildSeedJobMountsTheClaimAndPutsTheCachesUnderIt(t *testing.T) {
	t.Parallel()
	job, err := BuildSeedJob(seedInput())
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	volumes := map[string]Volume{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v
	}
	if pvc := volumes["cache"].PersistentVolumeClaim; pvc == nil || pvc.ClaimName != "gt-shared-cache" {
		t.Errorf("cache volume = %+v, want the gt-shared-cache claim", volumes["cache"])
	}
	if volumes["source"].EmptyDir == nil {
		t.Errorf("source volume = %+v, want an emptyDir", volumes["source"])
	}

	seed := job.Spec.Template.Spec.Containers[0]
	mounts := map[string]string{}
	for _, m := range seed.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["cache"] != "/cache" || mounts["source"] != "/seed" {
		t.Errorf("seed mounts = %v, want cache at /cache and source at /seed", mounts)
	}
	env := map[string]string{}
	for _, e := range seed.Env {
		env[e.Name] = e.Value
	}
	if env["GOCACHE"] != "/cache/gocache" || env["GOMODCACHE"] != "/cache/gomodcache" {
		t.Errorf("env = %v, want the two caches under /cache", env)
	}
	if env["GOFLAGS"] != "-p=4 -modcacherw" {
		t.Errorf("GOFLAGS = %q, want the Dockerfile seed RUN's flags so cache keys match", env["GOFLAGS"])
	}
	if got, want := seed.Args[0], "--pins"; got != want {
		t.Errorf("seed args = %v, want the pins first", seed.Args)
	}

	clone := job.Spec.Template.Spec.InitContainers[0]
	if clone.Command[0] != HelperPath || clone.Command[1] != "seed-clone" {
		t.Errorf("clone command = %v, want the image's seed-clone helper", clone.Command)
	}
	if env := envValue(clone.Env, "GIT_TERMINAL_PROMPT"); env != "0" {
		t.Errorf("clone GIT_TERMINAL_PROMPT = %q, want 0: a prompt would hang the Job", env)
	}
}

// TestBuildSeedJobGivesTheCloneContainerTheClaimToCheck pins the rerun's evidence: the init
// container mounts the same claim the seed container fills, read-only and at the same path, so a
// claim whose marker already holds the pins is cloned into nothing rather than cloned again
// (gt-posdj).
func TestBuildSeedJobGivesTheCloneContainerTheClaimToCheck(t *testing.T) {
	t.Parallel()
	job, err := BuildSeedJob(seedInput())
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	clone := job.Spec.Template.Spec.InitContainers[0]
	if got := argValue(clone.Args, "--root"); got != SeedCacheRoot {
		t.Errorf("clone --root = %q, want %s: the claim's marker is the no-op's evidence", got, SeedCacheRoot)
	}
	mounts := map[string]VolumeMount{}
	for _, m := range clone.VolumeMounts {
		mounts[m.Name] = m
	}
	if got := mounts["cache"]; got.MountPath != SeedCacheRoot || !got.ReadOnly {
		t.Errorf("clone cache mount = %+v, want %s read-only: the init container reads the marker and never writes the claim", got, SeedCacheRoot)
	}
	if got := mounts["source"]; got.MountPath != SeedSourceRoot {
		t.Errorf("clone source mount = %+v, want %s", got, SeedSourceRoot)
	}
}

// TestBuildSeedJobHandsBothContainersThePins pins the argv both containers read: each pin's rig,
// commit and clone URL, in seed order.
func TestBuildSeedJobHandsBothContainersThePins(t *testing.T) {
	t.Parallel()
	in := seedInput()
	job, err := BuildSeedJob(in)
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	for _, c := range []Container{job.Spec.Template.Spec.InitContainers[0], job.Spec.Template.Spec.Containers[0]} {
		raw := argValue(c.Args, "--pins")
		var got []Pin
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("%s --pins is not a pin list: %v", c.Name, err)
		}
		if !reflect.DeepEqual(got, in.Pins) {
			t.Errorf("%s --pins = %+v, want %+v", c.Name, got, in.Pins)
		}
	}
}

// TestSeedJobJSONIsTheDocumentKubectlApplies pins the encoding: indented, a clone URL's query
// readable, and a round trip back to the same Job.
func TestSeedJobJSONIsTheDocumentKubectlApplies(t *testing.T) {
	t.Parallel()
	in := seedInput()
	in.Pins[0].URL = cloneURL + "?ref=main&depth=1"
	job, err := BuildSeedJob(in)
	if err != nil {
		t.Fatalf("BuildSeedJob: %v", err)
	}
	doc := job.JSON()
	if !strings.Contains(string(doc), "\n  \"kind\": \"Job\"") {
		t.Errorf("JSON is not indented:\n%s", doc)
	}
	if !strings.Contains(string(doc), "ref=main&depth=1") {
		t.Errorf("JSON escaped the URL's query:\n%s", doc)
	}
	var back SeedJob
	if err := json.Unmarshal(doc, &back); err != nil {
		t.Fatalf("the document is not a Job: %v", err)
	}
	if !reflect.DeepEqual(&back, job) {
		t.Errorf("JSON round trip = %+v, want %+v", back, *job)
	}
}

// TestBuildSeedJobRefusesWhatItCannotRender drives every refusal: an input the Job could not run
// is an error naming the field, never a manifest the apiserver rejects later.
func TestBuildSeedJobRefusesWhatItCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		change  func(*SeedJobInput)
		wantErr string // a substring the error must carry
	}{
		{name: "no image", change: func(in *SeedJobInput) { in.Image = "" }, wantErr: "image"},
		{name: "an image with a space", change: func(in *SeedJobInput) { in.Image = "polecat dev" }, wantErr: "image"},
		{name: "a name the pod's job-name label cannot hold", change: func(in *SeedJobInput) { in.Name = strings.Repeat("a", 64) }, wantErr: "name"},
		{name: "an uppercase name", change: func(in *SeedJobInput) { in.Name = "Seed" }, wantErr: "name"},
		{name: "an uppercase namespace", change: func(in *SeedJobInput) { in.Namespace = "GT" }, wantErr: "namespace"},
		{name: "a claim longer than 63 characters", change: func(in *SeedJobInput) { in.ClaimName = strings.Repeat("a", 64) }, wantErr: "claim"},
		{name: "no pins", change: func(in *SeedJobInput) { in.Pins = nil }, wantErr: "no pin"},
		{name: "a commit that is not a full sha", change: func(in *SeedJobInput) { in.Pins[1].Commit = "abc123" }, wantErr: "40-hex"},
		{name: "a rig named twice", change: func(in *SeedJobInput) { in.Pins[1].Rig = "gastown" }, wantErr: "twice"},
		{name: "a rig that is a path", change: func(in *SeedJobInput) { in.Pins[0].Rig = "../gastown" }, wantErr: "DNS-1123"},
		{name: "no clone URL", change: func(in *SeedJobInput) { in.Pins[0].URL = "" }, wantErr: "clone URL"},
		{name: "a clone URL that is not http", change: func(in *SeedJobInput) { in.Pins[0].URL = "ssh://forgejo:22/town/gastown.git" }, wantErr: "http"},
		{name: "a clone URL with no host", change: func(in *SeedJobInput) { in.Pins[0].URL = "http:///town/gastown.git" }, wantErr: "host"},
		{name: "a scp-style clone URL", change: func(in *SeedJobInput) { in.Pins[0].URL = "git@forgejo:town/gastown.git" }, wantErr: "http"},
		{name: "a clone URL with userinfo", change: func(in *SeedJobInput) { in.Pins[0].URL = "https://bot:s3cret@forgejo:3000/town/gastown.git" }, wantErr: "userinfo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := seedInput()
			tc.change(&in)
			job, err := BuildSeedJob(in)
			if err == nil {
				t.Fatalf("BuildSeedJob(%+v) = %+v, want a refusal", in, job)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error quotes the URL's password: %q", err)
			}
		})
	}
}

// TestSeedMarkerIsThePinsOneLineEach pins the marker: a rerun compares it with the pins it is
// handed, so the format is the contract between the seed pass and its rerun.
func TestSeedMarkerIsThePinsOneLineEach(t *testing.T) {
	t.Parallel()
	got := SeedMarker(seedInput().Pins)
	want := "gastown " + sha + "\ndevops " + strings.Repeat("ab", 20) + "\n"
	if got != want {
		t.Errorf("SeedMarker = %q, want %q", got, want)
	}
}

// TestPlanSeedRefusesARigItCannotUseAsADirectory: the seed Job joins the rig name into a path, so a
// name that is not a DNS-1123 label is refused before it can escape that directory.
func TestPlanSeedRefusesARigItCannotUseAsADirectory(t *testing.T) {
	t.Parallel()
	for _, rig := range []string{"../gastown", "gas/town", "Gas", "", "gas.town", strings.Repeat("a", 64)} {
		if got, err := PlanSeed([]Pin{{Rig: rig, Commit: sha}}); err == nil {
			t.Errorf("PlanSeed(rig %q) = %+v, want a refusal", rig, got)
		}
	}
}

// envValue and argValue read one env entry and one flag's value out of a container, so a test names
// the field it pins rather than its offset.
func envValue(env []EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
