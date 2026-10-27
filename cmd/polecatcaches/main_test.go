package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The command reads the town config for each rig's Forgejo remote; the
// hermetic harness keeps the test binary off the live town's state, as every
// package that can reach it must.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

const (
	gastownCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	devopsCommit  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var pinsFile = []byte("# <rig> <commit>\n" +
	"gastown " + gastownCommit + "\ndevops " + devopsCommit + "\n")

// fake is the command's outside world: a pins document, one remote per rig,
// and every kubectl call it was handed, with the manifest on its stdin.
type fake struct {
	pins      []byte
	readErr   error
	remotes   map[string]string
	remoteErr map[string]error
	failCall  int // the 1-based kubectl call that fails; 0 fails none
	calls     []kubectlCall
}

type kubectlCall struct {
	args  []string
	stdin string
}

func (f *fake) deps() deps {
	return deps{
		readFile: func(string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			return f.pins, nil
		},
		remoteURL: func(rigName string) (string, error) {
			if err, ok := f.remoteErr[rigName]; ok {
				return "", err
			}
			if u, ok := f.remotes[rigName]; ok {
				return u, nil
			}
			return "", errors.New("rig " + rigName + " has no merge_queue.forgejo.remote_url")
		},
		kubectl: func(stdin io.Reader, args ...string) ([]byte, error) {
			call := kubectlCall{args: args}
			if stdin != nil {
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, stdin); err != nil {
					return nil, err
				}
				call.stdin = buf.String()
			}
			f.calls = append(f.calls, call)
			if f.failCall == len(f.calls) {
				return nil, errors.New("kubectl failed")
			}
			return []byte("ok\n"), nil
		},
	}
}

func newFake() *fake {
	return &fake{
		pins: pinsFile,
		remotes: map[string]string{
			"gastown": "http://host.docker.internal:3000/sloan/gastown.git",
			"devops":  "http://host.docker.internal:3000/sloan/devops.git",
		},
	}
}

// run is the command's one path, with the fakes in place of the town, the pins
// file and the cluster.
func run(t *testing.T, f *fake, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw bytes.Buffer
	code = runWith(args, &out, &errw, f.deps())
	return code, out.String(), errw.String()
}

// baseArgs are the flags every successful run needs.
func baseArgs(extra ...string) []string {
	return append([]string{"-town", "/town", "-image", "gastown-polecat:dev"}, extra...)
}

// appliedJob decodes the document the command handed to kubectl apply.
func appliedJob(t *testing.T, f *fake) cachevol.SeedJob {
	t.Helper()
	if len(f.calls) != 2 || strings.Join(f.calls[1].args, " ") != "apply -f -" {
		t.Fatalf("kubectl calls = %+v, want a delete then the manifest on apply's stdin", f.calls)
	}
	var job cachevol.SeedJob
	if err := json.Unmarshal([]byte(f.calls[1].stdin), &job); err != nil {
		t.Fatalf("the manifest kubectl applied is not a Job document: %v", err)
	}
	return job
}

// TestRunAppliesTheSeedJobManifest pins what reaches kubectl: the delete that
// lets changed pins reseed, then the apply of a root Job that fills the claim
// with the named image.
func TestRunAppliesTheSeedJobManifest(t *testing.T) {
	t.Parallel()
	f := newFake()
	code, stdout, stderr := run(t, f, baseArgs()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if len(f.calls) != 2 {
		t.Fatalf("kubectl calls = %d, want a delete then an apply", len(f.calls))
	}
	if got := strings.Join(f.calls[0].args, " "); got != "delete job gt-cache-seed --ignore-not-found" {
		t.Errorf("first kubectl call = %q, want the Job delete", got)
	}

	job := appliedJob(t, f)
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Metadata.Name != cachevol.DefaultSeedJobName {
		t.Errorf("manifest = %s %s %q, want batch/v1 Job %s", job.APIVersion, job.Kind, job.Metadata.Name, cachevol.DefaultSeedJobName)
	}
	if job.Spec.Template.Spec.SecurityContext == nil || *job.Spec.Template.Spec.SecurityContext.RunAsUser != 0 {
		t.Errorf("the Job's pod is not the root pass: %+v", job.Spec.Template.Spec.SecurityContext)
	}
	var claim string
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != "gt-cache" {
		t.Errorf("the Job mounts claim %q, want gt-cache", claim)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 || job.Spec.Template.Spec.Containers[0].Image != "gastown-polecat:dev" {
		t.Errorf("the seed container = %+v, want the polecat image given on -image", job.Spec.Template.Spec.Containers)
	}
	if !strings.Contains(stdout, "applied Job gt-cache-seed filling gt-cache from 2 pin(s): gastown devops") {
		t.Errorf("stdout = %q, want the applied Job, claim and rigs reported", stdout)
	}
}

// TestRunBuildsAPinForEachPinnedRig pins the pins the Job clones: one per row
// of the pins file, in its order, at its commit and Forgejo URL.
func TestRunBuildsAPinForEachPinnedRig(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	want := []cachevol.Pin{
		{Rig: "gastown", Commit: gastownCommit, URL: "http://host.docker.internal:3000/sloan/gastown.git"},
		{Rig: "devops", Commit: devopsCommit, URL: "http://host.docker.internal:3000/sloan/devops.git"},
	}
	pins := seedPins(t, appliedJob(t, f))
	if len(pins) != len(want) {
		t.Fatalf("the Job seeds %d pins, want both rows of the pins file: %+v", len(pins), pins)
	}
	for i, p := range want {
		if pins[i] != p {
			t.Errorf("pin %d = %+v, want %+v", i, pins[i], p)
		}
	}
}

// TestRunSeedsOnlyTheRigsNamedOnTheCommandLine keeps `make polecat-caches
// RIGS=devops` seeding one rig, the way the target it replaces took rigs.
func TestRunSeedsOnlyTheRigsNamedOnTheCommandLine(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("devops")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	pins := seedPins(t, appliedJob(t, f))
	if len(pins) != 1 || pins[0].Rig != "devops" {
		t.Errorf("the Job seeds %+v, want devops alone", pins)
	}
}

// TestRunSwapsTheCloneHost points the Job's clones at the host it can reach
// from the cluster, which is what -forgejo-host exists for.
func TestRunSwapsTheCloneHost(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("-forgejo-host", "forgejo.openshell:3000")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	for _, p := range seedPins(t, appliedJob(t, f)) {
		if !strings.HasPrefix(p.URL, "http://forgejo.openshell:3000/sloan/") {
			t.Errorf("pin %s clones from %q, want the -forgejo-host authority", p.Rig, p.URL)
		}
	}
}

// TestRunPassesTheNamespaceToTheDeleteAndTheManifest keeps the Job and the
// delete that precedes it in one namespace.
func TestRunPassesTheNamespaceToTheDeleteAndTheManifest(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("-namespace", "openshell")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := strings.Join(f.calls[0].args, " "); !strings.Contains(got, "--namespace openshell") {
		t.Errorf("delete call = %q, want the namespace", got)
	}
	if job := appliedJob(t, f); job.Metadata.Namespace != "openshell" {
		t.Errorf("manifest namespace = %q, want openshell", job.Metadata.Namespace)
	}
}

// TestRunReportsEveryRefusal drives each way a run stops before or during
// kubectl, so a refusal cannot regress into a partial apply.
func TestRunReportsEveryRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		mutate   func(*fake)
		wantCode int
		wantErr  string
		wantCall int // kubectl calls the refusal allows
	}{
		{
			name:     "no town",
			args:     []string{"-image", "img"},
			wantCode: 2,
			wantErr:  "-town is required",
		},
		{
			name:     "no image",
			args:     []string{"-town", "/town"},
			wantCode: 2,
			wantErr:  "-image is required",
		},
		{
			name:     "unreadable pins file",
			args:     baseArgs(),
			mutate:   func(f *fake) { f.readErr = errors.New("permission denied") },
			wantCode: 1,
			wantErr:  "cannot read deploy/polecat/seed-pins: permission denied",
		},
		{
			name:     "pins file naming no rig",
			args:     baseArgs(),
			mutate:   func(f *fake) { f.pins = []byte("# only comments\n") },
			wantCode: 1,
			wantErr:  "names no rig",
		},
		{
			name:     "rig with no pin line",
			args:     baseArgs("om"),
			wantCode: 1,
			wantErr:  "om names no pin in deploy/polecat/seed-pins",
		},
		{
			name: "rig with no Forgejo remote",
			args: baseArgs(),
			mutate: func(f *fake) {
				f.remoteErr = map[string]error{"devops": errors.New("rig devops has no merge_queue.forgejo.remote_url")}
			},
			wantCode: 1,
			wantErr:  "no merge_queue.forgejo.remote_url",
		},
		{
			name:     "delete fails",
			args:     baseArgs(),
			mutate:   func(f *fake) { f.failCall = 1 },
			wantCode: 1,
			wantErr:  "kubectl failed",
			wantCall: 1,
		},
		{
			name:     "apply fails",
			args:     baseArgs(),
			mutate:   func(f *fake) { f.failCall = 2 },
			wantCode: 1,
			wantErr:  "kubectl failed",
			wantCall: 2,
		},
		{
			name:     "image the builder refuses",
			args:     []string{"-town", "/town", "-image", "not an image"},
			wantCode: 1,
			wantErr:  "the seed Job image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			if tt.mutate != nil {
				tt.mutate(f)
			}
			code, _, stderr := run(t, f, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d; stderr: %s", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", stderr, tt.wantErr)
			}
			if len(f.calls) != tt.wantCall {
				t.Errorf("kubectl calls = %d, want %d", len(f.calls), tt.wantCall)
			}
		})
	}
}

// TestCloneURL pins the URL rule: the rig's remote as configured, rehosted
// when asked, with userinfo never carried and never echoed.
func TestCloneURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		remote  string
		host    string
		want    string
		wantErr string
	}{
		{
			name:   "no host keeps the configured remote",
			remote: "http://host.docker.internal:3000/sloan/gastown.git",
			want:   "http://host.docker.internal:3000/sloan/gastown.git",
		},
		{
			name:   "host and port replace the remote's",
			remote: "http://host.docker.internal:3000/sloan/gastown.git",
			host:   "forgejo.openshell:3001",
			want:   "http://forgejo.openshell:3001/sloan/gastown.git",
		},
		{
			name:   "a host with no port keeps the remote's port",
			remote: "https://forgejo.example:8443/sloan/gastown.git",
			host:   "forgejo.openshell",
			want:   "https://forgejo.openshell:8443/sloan/gastown.git",
		},
		{
			name:   "userinfo is dropped",
			remote: "http://bot:secret@forgejo.example/sloan/gastown.git",
			want:   "http://forgejo.example/sloan/gastown.git",
		},
		{
			name:    "an ssh remote is refused without quoting it",
			remote:  "git@forgejo.example:sloan/gastown.git",
			wantErr: "not an http(s) URL",
		},
		{
			name:    "an empty remote is refused",
			remote:  "",
			wantErr: "not an http(s) URL",
		},
		{
			name:    "a host with no name is refused",
			remote:  "http://forgejo.example/x.git",
			host:    "-bad-",
			wantErr: "want a bare host name",
		},
		{
			name:    "a port that is not a port is refused",
			remote:  "http://forgejo.example/x.git",
			host:    "forgejo.example:0",
			wantErr: "want host[:port]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cloneURL("gastown", tt.remote, tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("cloneURL(%q, %q) = %q, %v; want an error naming %q", tt.remote, tt.host, got, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("the error quotes the URL's userinfo: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cloneURL(%q, %q): %v", tt.remote, tt.host, err)
			}
			if got != tt.want {
				t.Errorf("cloneURL(%q, %q) = %q, want %q", tt.remote, tt.host, got, tt.want)
			}
		})
	}
}

// seedPins decodes the pins document the Job's containers take as argv.
func seedPins(t *testing.T, job cachevol.SeedJob) []cachevol.Pin {
	t.Helper()
	for _, c := range job.Spec.Template.Spec.Containers {
		for i, a := range c.Args {
			if a != "--pins" || i+1 >= len(c.Args) {
				continue
			}
			var pins []cachevol.Pin
			if err := json.Unmarshal([]byte(c.Args[i+1]), &pins); err != nil {
				t.Fatalf("container %s: the pins argument is not a pins document: %v", c.Name, err)
			}
			return pins
		}
	}
	t.Fatalf("no container carries a --pins argument: %+v", job.Spec.Template.Spec.Containers)
	return nil
}
