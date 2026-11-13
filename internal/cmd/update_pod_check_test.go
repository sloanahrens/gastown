package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// podPairManifest is the body the fake registry serves at the polecat dev tag.
// Its digest is the polecat digest the matching values file names, so the tag
// check passes only when the registry serves that manifest.
var podPairManifest = []byte("polecat dev manifest")

var (
	podPairPolecatRef = "registry.internal:5000/town/gastown-polecat@" + testDigestOf(podPairManifest)
	podPairCommit     = rollbackNewCommit
	podPairPair       = update.ImagePair{Daemon: podSwapDigestRef, Polecat: podPairPolecatRef, Commit: podPairCommit}

	// podPairPrev is the pair the daemon ran before the install: the one a
	// refused swap restores.
	podPairPrevDaemon  = "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("a", 64)
	podPairPrevPolecat = "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("9", 64)
	podPairPrevCommit  = strings.Repeat("a", 40)
	podPairPrev        = update.ImagePair{Daemon: podPairPrevDaemon, Polecat: podPairPrevPolecat, Commit: podPairPrevCommit}
)

// podPairCall is one command the fake kubectl was asked to run, with the stdin
// the command was given.
type podPairCall struct {
	args  []string
	stdin []byte
}

// podPairPod is one daemon pod the fake reports. phase is the pod's phase; an
// empty phase is Running. deleting sets the deletion mark. imageID is the image
// id the pod reports for its daemon container: nil leaves the field out, a
// pointer to "" reports an empty one.
type podPairPod struct {
	name     string
	phase    string
	deleting bool
	imageID  *string
}

// runningPod is a live, Running pod that reports id for its daemon container.
func runningPod(name, id string) podPairPod {
	return podPairPod{name: name, phase: "Running", imageID: &id}
}

// podPairHealthy is the pod list of a daemon that runs the values file's pair.
func podPairHealthy() []podPairPod {
	return []podPairPod{runningPod("gastown-daemon-new", podSwapDigestRef)}
}

// podListJSON is the `kubectl get pods -o json` answer for pods. The list is
// never null, so an empty list decodes as a list with no pods.
func podListJSON(pods ...podPairPod) string {
	type status struct {
		Name    string  `json:"name"`
		ImageID *string `json:"imageID,omitempty"`
	}
	type meta struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp,omitempty"`
	}
	type podStatus struct {
		Phase             string   `json:"phase"`
		ContainerStatuses []status `json:"containerStatuses"`
	}
	type item struct {
		Metadata meta      `json:"metadata"`
		Status   podStatus `json:"status"`
	}
	list := struct {
		Items []item `json:"items"`
	}{Items: []item{}}
	for _, p := range pods {
		phase := p.phase
		if phase == "" {
			phase = "Running"
		}
		it := item{
			Metadata: meta{Name: p.name},
			Status: podStatus{
				Phase:             phase,
				ContainerStatuses: []status{{Name: k8sdaemon.Name, ImageID: p.imageID}},
			},
		}
		if p.deleting {
			ts := "2026-11-13T12:00:00Z"
			it.Metadata.DeletionTimestamp = &ts
		}
		list.Items = append(list.Items, it)
	}
	b, err := json.Marshal(list)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// podPairKubectl stands in for kubectl against a daemon pod. A patch puts the
// daemon on the commit of the daemon digest it names. A get answers the pod list
// pods, or fails with getExit and the output a secret would be in. The dev tag
// read answers devTag, or a tag at the pair's digest when it is empty, and
// exits devTagExit when that is set. runErr fails every command as the runner
// would; failWord makes the command carrying that word exit 1. Every call is
// recorded.
type podPairKubectl struct {
	calls   []podPairCall
	commits map[string]string
	running string
	// image and polecat are the pair the daemon Deployment's pod template runs;
	// a patch of the daemon Deployment sets them to the pair it carries.
	image, polecat string
	pods           []podPairPod
	raw            string
	getExit        int
	runErr         error
	failWord       string
	devTag         string
	devTagExit     int
}

func (k *podPairKubectl) run(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, podPairCall{args: args, stdin: runStdinOf(ctx)})
	if k.runErr != nil {
		return "", -1, k.runErr
	}
	if k.failWord != "" && containsArg(args, k.failWord) {
		return podSwapOutput, 1, nil
	}
	switch {
	case containsArg(args, "polecat-devtag"):
		if k.devTagExit != 0 {
			return podSwapOutput, k.devTagExit, nil
		}
		if k.devTag != "" {
			return k.devTag, 0, nil
		}
		return podPairDevTagJSON(true, testDigestOf(podPairManifest)), 0, nil
	case containsArg(args, "patch"):
		var patch podSwapPatchBody
		if err := json.Unmarshal([]byte(args[len(args)-1]), &patch); err != nil {
			return "", 0, nil
		}
		for _, c := range patch.Spec.Template.Spec.Containers {
			if c.Name == k8sdaemon.Name {
				k.image = c.Image
			}
		}
		if polecat, ok := patch.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; ok {
			k.polecat = polecat
		}
		if commit, ok := k.commits[k.image]; ok {
			k.running = commit
		}
	case containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.Name):
		// The running pair read: the Deployment's daemon image and polecat
		// annotation.
		return k.image + "\n" + k.polecat, 0, nil
	case containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.DashboardName):
		// The dashboard's existence read the swap makes: the town has a dashboard.
		return "deployment.apps/" + k8sdaemon.DashboardName + "\n", 0, nil
	case containsArg(args, "get"):
		if k.getExit != 0 {
			return podSwapOutput, k.getExit, nil
		}
		if k.raw != "" {
			return k.raw, 0, nil
		}
		return podListJSON(k.pods...), 0, nil
	}
	return "", 0, nil
}

// patches is the argv of every patch the fake was asked to run, in order.
func (k *podPairKubectl) patches() [][]string {
	var out [][]string
	for _, c := range k.calls {
		if containsArg(c.args, "patch") {
			out = append(out, c.args)
		}
	}
	return out
}

// wroteToPod reports whether any call made a directory or copied a file into the pod.
func (k *podPairKubectl) wroteToPod() bool {
	for _, c := range k.calls {
		if containsArg(c.args, "tee") || containsArg(c.args, "mkdir") {
			return true
		}
	}
	return false
}

// podPairVerify reports the build the fake pod runs, the probe a digest install's
// wait reads.
type podPairVerify struct{ kube *podPairKubectl }

func (v podPairVerify) DaemonVersion(context.Context) (string, error) { return v.kube.running, nil }
func (v podPairVerify) GTVersion(context.Context) (string, error)     { return "", nil }
func (v podPairVerify) ImageLabel(context.Context) (string, error)    { return "", nil }
func (v podPairVerify) Health(context.Context) (string, error)        { return "", nil }

// podPairImages wires the registry reader to a fake registry that serves body at
// the polecat dev tag. A nil body serves no dev tag, so the tag is missing.
func podPairImages(body []byte) updateImagesSeams {
	registry := imagevalues.Registry{Scheme: "https", Host: "registry.internal:5000", Owner: testUpdateImagesOwner}
	return updateImagesSeams{
		Registry: func(string) (imagevalues.Registry, error) { return registry, nil },
		Credentials: func() (string, string, error) {
			return testUpdateImagesUser, testUpdateImagesToken, nil
		},
		HTTPClient: &http.Client{Transport: podPairRegistry{body: body}},
	}
}

// podPairRegistry answers the dev tag read in process: the polecat dev tag serves
// body under its own digest, and every other request, or a nil body, is a 404.
// A nonzero status answers every request with that status instead. No socket is
// opened, so the check is tested without the network.
type podPairRegistry struct {
	body   []byte
	status int
}

func (r podPairRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	header := http.Header{}
	if r.status != 0 {
		return &http.Response{StatusCode: r.status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	if r.body == nil || req.URL.Path != "/v2/"+testUpdateImagesOwner+"/"+imagevalues.PolecatImage+"/manifests/"+podPolecatTag {
		return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	header.Set("Docker-Content-Digest", testDigestOf(r.body))
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(r.body)), Request: req}, nil
}

// podPairDevTagJSON is the one line `gt daemon polecat-devtag` prints, encoded
// through the type the command itself writes.
func podPairDevTagJSON(present bool, digest string) string {
	b, err := json.Marshal(polecatDevTagRead{Present: present, Digest: digest})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// podPairTarget is the Deployment every pair check in this file names.
func podPairTarget() podDaemonTarget {
	return podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}
}

// podPairValuesFile writes the values file for podPairPair and returns its path and bytes.
func podPairValuesFile(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "images.yaml")
	if err := imagevalues.Write(path, imagevalues.Values{Daemon: podPairPair.Daemon, Polecat: podPairPair.Polecat, Commit: podPairPair.Commit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the values file: %v", err)
	}
	return path, data
}

// TestUpdatePodPairCheckRefusesAPairThePodDoesNotHold holds the daemon pod read
// to its own refusals. The pods' image ids are judged in
// TestUpdatePodPairCheckJudgesEveryDaemonPod, and the dev tag in
// TestPodPairDevTag. Every refusal names its step and writes nothing into the pod.
func TestUpdatePodPairCheckRefusesAPairThePodDoesNotHold(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		raw     string
		getExit int
		want    string
	}{
		{
			name:    "the read of the daemon pods exits nonzero",
			getExit: 1,
			want:    "exited 1",
		},
		{
			name: "the read of the daemon pods is not a pod list",
			raw:  "a read that is not JSON",
			want: "is not a pod list",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path, _ := podPairValuesFile(t)
			kube := &podPairKubectl{raw: row.raw, getExit: row.getExit}
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
			err := check.Check(context.Background(), podPairPair)
			if err == nil {
				t.Fatal("Check accepted the pair, want a refusal")
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("error = %q, want it to name %q", err, row.want)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("error %q carries a command's output", err)
			}
			if kube.wroteToPod() {
				t.Errorf("a refused pair wrote into the pod: calls = %+v", kube.calls)
			}
		})
	}
}

// TestUpdatePodPairCheckJudgesEveryDaemonPod holds the image check to each pod
// the list reports. A pod being deleted is skipped; any other pod must be Running
// and report an id whose digest is the values file's. A list with no pod left to
// judge fails. A passing row goes on to the copy into the pod; a refusing row
// writes nothing.
func TestUpdatePodPairCheckJudgesEveryDaemonPod(t *testing.T) {
	t.Parallel()
	ptr := func(s string) *string { return &s }
	rows := []struct {
		name string
		pods []podPairPod
		want string
	}{
		{
			name: "every daemon pod runs the values file's digest",
			pods: []podPairPod{runningPod("new-a", podSwapDigestRef), runningPod("new-b", podSwapDigestRef)},
		},
		{
			name: "one pod runs another daemon digest",
			pods: []podPairPod{runningPod("new-a", podSwapDigestRef), runningPod("old-b", podPairPrevDaemon)},
			want: "pod old-b runs daemon digest",
		},
		{
			name: "a pod reports an empty image id",
			pods: []podPairPod{runningPod("new-a", "")},
			want: "pod new-a reports no image id",
		},
		{
			name: "a pod's image id field is absent",
			pods: []podPairPod{{name: "new-a", phase: "Running"}},
			want: "pod new-a reports no image id",
		},
		{
			name: "the pod list is empty",
			pods: nil,
			want: "no daemon pod to verify",
		},
		{
			name: "a pod's image id names no digest",
			pods: []podPairPod{runningPod("new-a", "gastown-daemon:dev")},
			want: "pod new-a reports an image id that names no digest",
		},
		{
			name: "an old terminating pod beside a new matching pod",
			pods: []podPairPod{{name: "old-a", phase: "Running", deleting: true, imageID: ptr(podPairPrevDaemon)}, runningPod("new-b", podSwapDigestRef)},
		},
		{
			name: "an old terminating pod beside a new pod with no image id",
			pods: []podPairPod{{name: "old-a", phase: "Running", deleting: true, imageID: ptr(podPairPrevDaemon)}, runningPod("new-b", "")},
			want: "pod new-b reports no image id",
		},
		{
			name: "a live pod that is not Running",
			pods: []podPairPod{{name: "new-a", phase: "Pending", imageID: ptr(podSwapDigestRef)}},
			want: `pod new-a is in phase "Pending", not Running`,
		},
		{
			name: "only terminating pods remain",
			pods: []podPairPod{{name: "old-a", phase: "Running", deleting: true, imageID: ptr(podPairPrevDaemon)}},
			want: "no daemon pod to verify",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path, _ := podPairValuesFile(t)
			kube := &podPairKubectl{pods: row.pods}
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
			err := check.Check(context.Background(), podPairPair)
			if row.want == "" {
				if err != nil {
					t.Fatalf("Check: %v, want the pods judged clean", err)
				}
				if !kube.wroteToPod() {
					t.Errorf("a passing pod list wrote nothing into the pod: calls = %+v", kube.calls)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Check error = %v, want it to name %q", err, row.want)
			}
			if kube.wroteToPod() {
				t.Errorf("a refused pod list wrote into the pod: calls = %+v", kube.calls)
			}
		})
	}
}

// TestUpdatePodPairCheckRefusesAValuesFileThatNoLongerHoldsThePair holds the
// check to the pair the swap put in force: a values file rewritten to another
// commit is refused before any command runs.
func TestUpdatePodPairCheckRefusesAValuesFileThatNoLongerHoldsThePair(t *testing.T) {
	t.Parallel()
	path, _ := podPairValuesFile(t)
	kube := &podPairKubectl{pods: podPairHealthy()}
	check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
	other := podPairPair
	other.Commit = strings.Repeat("c", 40)
	if err := check.Check(context.Background(), other); err == nil || !strings.Contains(err.Error(), "no longer holds the pair") {
		t.Fatalf("Check error = %v, want the pair mismatch named", err)
	}
	if len(kube.calls) != 0 {
		t.Errorf("a mismatched pair ran commands: %+v", kube.calls)
	}
}

// TestUpdatePodPairCheckRefusesAnUnreadableValuesFile holds the check's one
// local input to its refusals: a values file that does not read or parse. Each
// refusal comes before any command runs, so nothing is written into the pod.
func TestUpdatePodPairCheckRefusesAnUnreadableValuesFile(t *testing.T) {
	t.Parallel()
	garbage := filepath.Join(t.TempDir(), "garbage.yaml")
	if err := os.WriteFile(garbage, []byte("daemon: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write the garbage file: %v", err)
	}
	rows := []struct {
		name string
		path string
		want string
	}{
		{name: "the values file is missing", path: filepath.Join(t.TempDir(), "missing.yaml"), want: "reading the values file for the pod check"},
		{name: "the values file does not parse", path: garbage, want: "the values file for the pod check"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			kube := &podPairKubectl{pods: podPairHealthy()}
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: row.path}
			err := check.Check(context.Background(), podPairPair)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Check error = %v, want it to name %q", err, row.want)
			}
			if len(kube.calls) != 0 {
				t.Errorf("a refused values file ran commands: calls = %+v", kube.calls)
			}
		})
	}
}

// TestUpdatePodPairCheckStopsAtEachFailedStep holds each kubectl step to its own
// refusal: a read the runner cannot run, and a directory or a copy the pod
// refuses. Each error names its step, carries no output, and ends the check, so
// the commands after it do not run.
func TestUpdatePodPairCheckStopsAtEachFailedStep(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name     string
		kube     podPairKubectl
		want     string
		commands int
	}{
		{name: "the runner cannot run the pod read", kube: podPairKubectl{runErr: errors.New("kubectl is not installed")}, want: "running kubectl for the pod's read of the daemon pods", commands: 1},
		{name: "the update directory cannot be made in the pod", kube: podPairKubectl{pods: podPairHealthy(), failWord: "mkdir"}, want: "creation of the update directory exited 1", commands: 3},
		{name: "the values file cannot be copied into the pod", kube: podPairKubectl{pods: podPairHealthy(), failWord: "tee"}, want: "copy of the values file exited 1", commands: 4},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path, _ := podPairValuesFile(t)
			kube := row.kube
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
			err := check.Check(context.Background(), podPairPair)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Check error = %v, want it to name %q", err, row.want)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("error %q carries a command's output", err)
			}
			if len(kube.calls) != row.commands {
				t.Errorf("commands = %d, want %d: the check stops at the failed step", len(kube.calls), row.commands)
			}
		})
	}
}

// TestUpdatePodPairCheckWritesTheValuesFileIntoThePod holds the success path to
// its commands: the pods are read, the dev tag is read in the daemon container,
// the update directory is made in the pod, and the values file's bytes go into
// the pod on stdin.
func TestUpdatePodPairCheckWritesTheValuesFileIntoThePod(t *testing.T) {
	t.Parallel()
	path, data := podPairValuesFile(t)
	kube := &podPairKubectl{pods: podPairHealthy()}
	check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path}
	if err := check.Check(context.Background(), podPairPair); err != nil {
		t.Fatalf("Check: %v", err)
	}

	dir := update.Dir(k8sdaemon.TownRootPath)
	prefix := []string{"--context", podSwapContext, "-n", podSwapNamespace}
	if len(kube.calls) != 4 {
		t.Fatalf("calls = %+v, want the pod read, the dev tag read, the directory, then the copy", kube.calls)
	}
	get := kube.calls[0]
	if !slices.Equal(get.args[:len(prefix)], prefix) || !slices.Contains(get.args, "app.kubernetes.io/name="+k8sdaemon.Name) {
		t.Errorf("read argv = %q, want the daemon pods by label in the namespace", get.args)
	}
	devTag := kube.calls[1]
	devTagWant := append(slices.Clone(prefix), "exec", "deployment/"+k8sdaemon.Name, "-c", k8sdaemon.Name, "--")
	devTagWant = append(devTagWant, polecatDevTagCommand...)
	if !slices.Equal(devTag.args, devTagWant) {
		t.Errorf("dev tag argv = %q, want %q", devTag.args, devTagWant)
	}
	mkdir := kube.calls[2]
	if want := append(slices.Clone(prefix), "exec", "deployment/"+k8sdaemon.Name, "--", "mkdir", "-p", dir); !slices.Equal(mkdir.args, want) {
		t.Errorf("mkdir argv = %q, want %q", mkdir.args, want)
	}
	copyCall := kube.calls[3]
	want := append(slices.Clone(prefix), "exec", "-i", "deployment/"+k8sdaemon.Name, "--", "tee", filepath.Join(dir, podImagesValuesName))
	if !slices.Equal(copyCall.args, want) {
		t.Errorf("copy argv = %q, want %q", copyCall.args, want)
	}
	if string(copyCall.stdin) != string(data) {
		t.Errorf("copy stdin = %q, want the values file's bytes %q", copyCall.stdin, data)
	}
}

// TestPodPairDevTag holds the dev tag check to the sandbox registry read in the
// daemon pod: a tag at the pair's digest, a tag at another digest, an absent
// tag, and a read that fails. Each refusal names the state it last saw after the
// wait and writes nothing into the pod.
func TestPodPairDevTag(t *testing.T) {
	t.Parallel()
	otherDigest := testDigestOf([]byte("another polecat manifest"))
	rows := []struct {
		name       string
		devTag     string
		devTagExit int
		want       string
	}{
		{name: "the tag names the pair's polecat digest"},
		{name: "the tag names another digest", devTag: podPairDevTagJSON(true, otherDigest), want: "names digest " + otherDigest},
		{name: "the tag is absent", devTag: podPairDevTagJSON(false, ""), want: "has no dev tag"},
		{name: "the read fails", devTagExit: 1, want: "could not be read"},
		{name: "the read does not answer JSON", devTag: "gt version 0123456789ab built on test", want: "did not answer JSON"},
		{name: "the read names no digest", devTag: `{"present":true,"digest":"not-a-digest"}`, want: "named no sha256 digest"},
		{
			name:   "a kubectl warning surrounds the answer",
			devTag: "Warning: something the cluster said\n" + podPairDevTagJSON(true, testDigestOf(podPairManifest)) + "\nWarning: and another",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path, _ := podPairValuesFile(t)
			kube := &podPairKubectl{pods: podPairHealthy(), devTag: row.devTag, devTagExit: row.devTagExit}
			clock := &rollbackTestClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
			check := podPairCheck{Run: kube.run, Target: podPairTarget(), ValuesPath: path, Clock: clock, Sleep: clock.sleep}
			err := check.Check(context.Background(), podPairPair)
			if row.want == "" {
				if err != nil {
					t.Fatalf("Check: %v, want the tag's digest accepted", err)
				}
				if !kube.wroteToPod() {
					t.Errorf("a matching tag wrote nothing into the pod: calls = %+v", kube.calls)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Check error = %v, want it to name %q", err, row.want)
			}
			if !strings.Contains(err.Error(), "after waiting") {
				t.Errorf("error = %q, want the bound named: a tag still being copied is waited for", err)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("error %q carries a command's output", err)
			}
			if kube.wroteToPod() {
				t.Errorf("a refused tag wrote into the pod: calls = %+v", kube.calls)
			}
		})
	}
}

// TestPodPairDevTagNotFromTownRegistry holds the check to the registry the
// daemon publishes the dev tag to: the town's Forgejo registry is wired to
// refuse every read, and the check still passes because the daemon pod reports
// the pair's digest. A check that went back to reading townImageRegistry fails
// here (gt-5yh32c).
func TestPodPairDevTagNotFromTownRegistry(t *testing.T) {
	t.Parallel()
	path, _ := podPairValuesFile(t)
	kube := &podPairKubectl{pods: podPairHealthy()}
	refused := podPairImages(podPairManifest)
	refused.Registry = func(string) (imagevalues.Registry, error) {
		return imagevalues.Registry{}, errors.New("the pair check read the town's Forgejo registry")
	}
	refused.Credentials = func() (string, string, error) {
		return "", "", errors.New("the pair check read the viewer credential")
	}
	refused.HTTPClient = &http.Client{Transport: podPairRegistry{status: http.StatusInternalServerError}}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	deps := update.Deps{Clock: clock, Sleep: clock.sleep}
	seams := updateSeams{
		PodRun:   kube.run,
		Images:   refused,
		Notifier: func(string) notify.Notifier { return notifyfake.New() },
	}
	target := podPairTarget()
	installer, err := digestInstaller(seams, t.TempDir(), deps, path, &target, io.Discard)
	if err != nil {
		t.Fatalf("digestInstaller: %v", err)
	}
	install, ok := installer.(*update.DigestInstaller)
	if !ok {
		t.Fatalf("installer = %T, want the digest installer", installer)
	}
	if err := install.PairCheck(context.Background(), podPairPair); err != nil {
		t.Fatalf("PairCheck: %v, want the pair accepted from the daemon pod's read", err)
	}
	read := false
	for _, call := range kube.calls {
		if containsArg(call.args, "polecat-devtag") {
			read = true
		}
	}
	if !read {
		t.Errorf("the check never read the dev tag in the daemon pod: calls = %+v", kube.calls)
	}
}

// TestUpdatePodDigestInstallRestoresPreviousWhenPodRunsAnotherImage runs a pod
// digest install through the production wiring, with the pod reporting the
// previous daemon digest after the swap. The install must fail at the check and
// put the recorded previous pair back, and the pod's values file is not written.
func TestUpdatePodDigestInstallRestoresPreviousWhenPodRunsAnotherImage(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ledger := update.FileImageLedger{Dir: update.Dir(townRoot)}
	if err := ledger.Write(update.ImageRecord{Current: podPairPrev}); err != nil {
		t.Fatalf("record the previous pair: %v", err)
	}
	path, _ := podPairValuesFile(t)
	kube := &podPairKubectl{
		commits: map[string]string{podSwapDigestRef: podPairCommit, podPairPrevDaemon: podPairPrevCommit},
		running: podPairPrevCommit,
		// The Deployment runs the previous pair before the install, so the running
		// pair read names the pair the restore must put back.
		image:   podPairPrevDaemon,
		polecat: podPairPrevPolecat,
		pods:    []podPairPod{runningPod("gastown-daemon-old", podPairPrevDaemon)},
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	deps := update.Deps{
		Clock:     clock,
		Sleep:     clock.sleep,
		Verify:    podPairVerify{kube: kube},
		Installer: &podHostInstaller{},
	}
	seams := updateSeams{
		Notifier: func(string) notify.Notifier { return notifyfake.New() },
		PodRun:   kube.run,
	}
	target := podPairTarget()
	installer, err := digestInstaller(seams, townRoot, deps, path, &target, io.Discard)
	if err != nil {
		t.Fatalf("digestInstaller: %v", err)
	}
	install, ok := installer.(*update.DigestInstaller)
	if !ok {
		t.Fatalf("installer = %T, want the digest installer", installer)
	}

	res, err := install.Install(context.Background(), podPairCommit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if update.ClassifyInstall(res) != update.ResultFailed {
		t.Fatalf("result = %+v, want a failed install", res)
	}
	if !strings.Contains(res.Reason, "the daemon did not run the pair") || !strings.Contains(res.Reason, "restored "+podPairPrevDaemon) {
		t.Errorf("reason = %q, want the failed check and the restore named", res.Reason)
	}
	// Each Deployment is patched to the new pair, then restored to the previous one.
	var daemonPatches, dashboardPatches [][]string
	for _, p := range kube.patches() {
		switch {
		case slices.Contains(p, "deployment/"+k8sdaemon.Name):
			daemonPatches = append(daemonPatches, p)
		case slices.Contains(p, "deployment/"+k8sdaemon.DashboardName):
			dashboardPatches = append(dashboardPatches, p)
		}
	}
	for _, set := range []struct {
		name    string
		patches [][]string
	}{{"daemon", daemonPatches}, {"dashboard", dashboardPatches}} {
		if len(set.patches) != 2 || !strings.Contains(strings.Join(set.patches[0], " "), podSwapDigestRef) || !strings.Contains(strings.Join(set.patches[1], " "), podPairPrevDaemon) {
			t.Errorf("%s patches = %q, want the new pair then the restore of the previous pair", set.name, set.patches)
		}
	}
	if kube.wroteToPod() {
		t.Errorf("a failed check wrote the values file into the pod: calls = %+v", kube.calls)
	}
	rec, err := ledger.Read()
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	// The pair restored is the one read from the Deployment, which names no commit.
	restored := update.ImagePair{Daemon: podPairPrevDaemon, Polecat: podPairPrevPolecat}
	if want := (update.ImageRecord{V: 1, Current: restored, Previous: restored}); rec != want {
		t.Errorf("record after the restore = %+v, want %+v", rec, want)
	}
}

// TestBuildCommandCarriesTheRunStdin holds the production command to the stdin
// its context carries: the bytes reach the command's stdin, and a context with
// none gives the command no stdin.
func TestBuildCommandCarriesTheRunStdin(t *testing.T) {
	t.Parallel()
	data := []byte("daemon: sha256:e\n")
	cmd := buildCommand(withRunStdin(context.Background(), data), "", nil, "kubectl", "exec", "-i")
	if cmd.Stdin == nil {
		t.Fatal("the command has no stdin, want the carried bytes")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read the command's stdin: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("stdin = %q, want %q", got, data)
	}
	if plain := buildCommand(context.Background(), "", nil, "kubectl"); plain.Stdin != nil {
		t.Errorf("a context with no stdin gave the command %T", plain.Stdin)
	}
}

// TestNewRunFuncForwardsTheRunStdin holds the production RunFunc to the stdin its
// context carries. It runs newRunFunc with an injected commandOutput that reads
// the Stdin of the *exec.Cmd newRunFunc builds, the reader exec would hand the
// child. The unit tier forbids a subprocess, so no command runs: the bytes a child
// would read are read off the command, and that forwarding is what is under test.
func TestNewRunFuncForwardsTheRunStdin(t *testing.T) {
	t.Parallel()
	data := []byte("daemon: sha256:e\n")
	rows := []struct {
		name string
		ctx  context.Context
		want []byte
	}{
		{name: "the carried bytes reach the command's stdin", ctx: withRunStdin(context.Background(), data), want: data},
		{name: "a context with no stdin gives the command none", ctx: context.Background(), want: nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var read []byte
			output := func(cmd *exec.Cmd) ([]byte, error) {
				if cmd.Stdin == nil {
					return nil, nil
				}
				b, err := io.ReadAll(cmd.Stdin)
				read = b
				return b, err
			}
			out, exit, err := newRunFunc(output)(row.ctx, "", nil, "kubectl", "exec", "-i")
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if exit != 0 {
				t.Fatalf("exit = %d, want 0", exit)
			}
			if string(read) != string(row.want) {
				t.Errorf("command stdin = %q, want %q", read, row.want)
			}
			if out != string(row.want) {
				t.Errorf("output = %q, want the bytes the command read %q", out, row.want)
			}
		})
	}
}
