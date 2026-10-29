package cachevol

import (
	"reflect"
	"strings"
	"testing"
)

// trimInput is an input that builds: one build-cache file over the cap, the tests vary one field.
func trimInput() TrimJobInput {
	return TrimJobInput{
		Namespace:        "gt",
		Image:            "gastown-polecat:dev",
		ClaimName:        "gt-cache",
		Listing:          []Entry{file("gocache/ab/cdef", BuildCache, 100, 8, 8)},
		CapBytes:         50,
		SandboxesRunning: 0,
	}
}

// TestBuildTrimJobRunsAsTheCacheUserWithTheClaimAtCache pins the Job's identity and its mount: the
// pod runs as SeedUserID, the user the seed handed the claim to, so the Job can remove only what a
// sandbox could write, and the claim is the one volume it sees.
func TestBuildTrimJobRunsAsTheCacheUserWithTheClaimAtCache(t *testing.T) {
	t.Parallel()
	job, err := BuildTrimJob(trimInput())
	if err != nil {
		t.Fatalf("BuildTrimJob: %v", err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Errorf("apiVersion/kind = %q/%q, want batch/v1 and Job", job.APIVersion, job.Kind)
	}
	if job.Metadata.Name != DefaultTrimJobName || job.Metadata.Namespace != "gt" {
		t.Errorf("metadata = %+v, want %s in gt", job.Metadata, DefaultTrimJobName)
	}
	if job.Metadata.Labels["app.kubernetes.io/name"] != "polecat-cache-trim" {
		t.Errorf("labels = %v, want the trim Job's name label", job.Metadata.Labels)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed trim reports itself once", job.Spec.BackoffLimit)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never", got)
	}
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != SeedUserID || sc.RunAsGroup == nil || *sc.RunAsGroup != SeedUserID {
		t.Fatalf("pod securityContext = %+v, want runAsUser and runAsGroup %d", sc, SeedUserID)
	}
	// The rendered document is where the absence matters: a container that names its own identity
	// would override the pod's.
	if n := strings.Count(string(job.JSON()), `"securityContext"`); n != 1 {
		t.Errorf("the document carries %d securityContext blocks, want the pod's alone", n)
	}

	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want the trim container alone", len(pod.Containers))
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "gt-cache" {
		t.Fatalf("volumes = %+v, want the gt-cache claim alone", pod.Volumes)
	}
	trim := pod.Containers[0]
	if trim.Image != "gastown-polecat:dev" {
		t.Errorf("image = %q, want the polecat image", trim.Image)
	}
	if len(trim.VolumeMounts) != 1 || trim.VolumeMounts[0].MountPath != SeedCacheRoot ||
		trim.VolumeMounts[0].Name != "cache" {
		t.Errorf("mounts = %+v, want the cache volume at %s", trim.VolumeMounts, SeedCacheRoot)
	}
	if !strings.Contains(string(job.JSON()), `"runAsUser": 10001`) {
		t.Errorf("the document does not carry the cache user: %s", job.JSON())
	}
}

// TestBuildTrimJobRemovesExactlyThePlannedFiles pins the container's argv: one rm, the planned paths
// under the claim's mount, and nothing else the listing holds.
func TestBuildTrimJobRemovesExactlyThePlannedFiles(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.Listing = []Entry{
		file("gocache/oldest", BuildCache, 100, 1, 1),
		file("gocache/older", BuildCache, 100, 2, 2),
		file("gocache/newest", BuildCache, 100, 9, 9),
		file("gomodcache/mod", ModuleCache, 100, 0, 0),
	}
	in.CapBytes = 250
	in.SandboxesRunning = 2
	job, err := BuildTrimJob(in)
	if err != nil {
		t.Fatalf("BuildTrimJob: %v", err)
	}
	// 400 bytes over a 250-byte cap, so the two least recently used build-cache files go and the
	// module cache stays, since a sandbox is running.
	want := []string{"rm", "-f", "--", "/cache/gocache/oldest", "/cache/gocache/older"}
	if got := job.Spec.Template.Spec.Containers[0].Command; !reflect.DeepEqual(got, want) {
		t.Errorf("command = %v, want %v", got, want)
	}
}

// TestBuildTrimJobTakesTheSandboxCountFromItsCaller pins the wiring of PlanTrim's idle rule: the
// count the caller passed decides whether the module cache may go, and the Job itself asks nothing.
func TestBuildTrimJobTakesTheSandboxCountFromItsCaller(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		running  int
		wantMod  bool
		wantOver bool
	}{
		// 200 bytes of build cache and 100 of module cache under a 50-byte cap: the build cache
		// alone cannot reach the cap, so the module cache goes only when no sandbox runs.
		{name: "idle", running: 0, wantMod: true},
		{name: "one running", running: 1, wantOver: true},
		{name: "three running", running: 3, wantOver: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			in.Listing = []Entry{
				file("gocache/a", BuildCache, 100, 1, 1),
				file("gocache/b", BuildCache, 100, 2, 2),
				file("gomodcache/m", ModuleCache, 100, 3, 3),
			}
			in.CapBytes = 50
			in.SandboxesRunning = tc.running
			job, err := BuildTrimJob(in)
			if err != nil {
				t.Fatalf("BuildTrimJob: %v", err)
			}
			want := []string{"rm", "-f", "--"}
			for _, r := range PlanTrim(in.Listing, in.CapBytes, tc.running > 0).Removals {
				want = append(want, "/cache/"+r.Path)
			}
			command := job.Spec.Template.Spec.Containers[0].Command
			if !reflect.DeepEqual(command, want) {
				t.Errorf("command = %v, want PlanTrim's removals %v", command, want)
			}
			if got := hasModulePath(command); got != tc.wantMod {
				t.Errorf("command names the module cache = %v, want %v", got, tc.wantMod)
			}
			_, annotated := job.Metadata.Annotations[TrimOverflowAnnotation]
			if annotated != tc.wantOver {
				t.Errorf("overflow annotation present = %v, want %v", annotated, tc.wantOver)
			}
		})
	}
}

// TestBuildTrimJobAnnotatesTheOverflow pins the byte count the rules hand the caller: the Job says
// how far over the cap the claim stays, so a Job read without the caller's log still explains
// itself, and a trim that reaches the cap carries no annotation.
func TestBuildTrimJobAnnotatesTheOverflow(t *testing.T) {
	t.Parallel()
	in := trimInput()
	in.Listing = []Entry{
		file("gocache/a", BuildCache, 100, 1, 1),
		file("gomodcache/m", ModuleCache, 100, 2, 2),
	}
	in.CapBytes = 50
	in.SandboxesRunning = 1
	job, err := BuildTrimJob(in)
	if err != nil {
		t.Fatalf("BuildTrimJob: %v", err)
	}
	if got := job.Metadata.Annotations[TrimOverflowAnnotation]; got != "50" {
		t.Errorf("%s = %q, want 50 bytes over the cap", TrimOverflowAnnotation, got)
	}
	if doc := string(job.JSON()); !strings.Contains(doc, `"`+TrimOverflowAnnotation+`": "50"`) {
		t.Errorf("the document does not carry the overflow annotation: %s", doc)
	}

	// The same listing with no sandbox running loses the module cache too, so nothing overflows.
	in.SandboxesRunning = 0
	job, err = BuildTrimJob(in)
	if err != nil {
		t.Fatalf("BuildTrimJob: %v", err)
	}
	if _, ok := job.Metadata.Annotations[TrimOverflowAnnotation]; ok {
		t.Errorf("annotations = %v, want none when the trim reaches the cap", job.Metadata.Annotations)
	}
}

// hasModulePath reports whether argv names a module-cache file.
func hasModulePath(argv []string) bool {
	for _, a := range argv {
		if strings.HasPrefix(a, SeedCacheRoot+"/"+SeedModuleCacheDir+"/") {
			return true
		}
	}
	return false
}

// TestBuildTrimJobRefusesAnInputItCannotRender pins each refusal: a name, namespace, image or claim
// Kubernetes would reject, a cap or count that is not a size, a listing with nothing to remove, and
// a path that could name a file outside the claim.
func TestBuildTrimJobRefusesAnInputItCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*TrimJobInput)
	}{
		{"a name that is not a label", func(in *TrimJobInput) { in.Name = "gt-cache-trim/1" }},
		{"a namespace that is not a label", func(in *TrimJobInput) { in.Namespace = "gt cache" }},
		{"no image", func(in *TrimJobInput) { in.Image = "" }},
		{"an image with a space", func(in *TrimJobInput) { in.Image = "gastown polecat:dev" }},
		{"no claim", func(in *TrimJobInput) { in.ClaimName = "" }},
		{"a negative cap", func(in *TrimJobInput) { in.CapBytes = -1 }},
		{"a negative sandbox count", func(in *TrimJobInput) { in.SandboxesRunning = -1 }},
		{"nothing listed", func(in *TrimJobInput) { in.Listing = nil }},
		{"a listing under the cap", func(in *TrimJobInput) { in.CapBytes = 1000 }},
		{"an absolute path", func(in *TrimJobInput) {
			in.Listing = []Entry{file("/etc/passwd", BuildCache, 100, 1, 1)}
		}},
		{"an absolute path under the mount", func(in *TrimJobInput) {
			in.Listing = []Entry{file(SeedCacheRoot+"/gocache/a", BuildCache, 100, 1, 1)}
		}},
		{"a path above the claim root", func(in *TrimJobInput) {
			in.Listing = []Entry{file("../gocache/a", BuildCache, 100, 1, 1)}
		}},
		{"the claim root itself", func(in *TrimJobInput) {
			in.Listing = []Entry{file(".", BuildCache, 100, 1, 1)}
		}},
		{"an unclean path", func(in *TrimJobInput) {
			in.Listing = []Entry{file("gocache//a", BuildCache, 100, 1, 1)}
		}},
		{"a path with a current-directory element", func(in *TrimJobInput) {
			in.Listing = []Entry{file("gocache/./a", BuildCache, 100, 1, 1)}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			tc.edit(&in)
			if job, err := BuildTrimJob(in); err == nil {
				t.Errorf("BuildTrimJob(%+v) = %+v, want a refusal", in, job)
			}
		})
	}
}
