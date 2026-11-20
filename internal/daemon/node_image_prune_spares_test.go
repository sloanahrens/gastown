package daemon

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// nodeImagePruneRealList is a trimmed copy of the control cluster's output of
// `kubectl -n gt get job,deploy,pod -o json`, captured read-only on 2026-11-20.
const nodeImagePruneRealList = "testdata/node_image_prune_list.json"

// digestN is a valid sha256 digest of one hex digit repeated 64 times.
func digestN(c string) string {
	return "sha256:" + strings.Repeat(c, 64)
}

// digestAt is a valid sha256 digest whose hex is i, zero-padded to 64 digits.
func digestAt(i int) string {
	return fmt.Sprintf("sha256:%064x", i)
}

// jsonList wraps items in a kubectl List document.
func jsonList(items ...string) string {
	return `{"kind":"List","items":[` + strings.Join(items, ",") + `]}`
}

// jsonDeployment is a Deployment named name whose one container runs image.
func jsonDeployment(name, image string) string {
	return fmt.Sprintf(`{"kind":"Deployment","metadata":{"name":%q},"spec":{"template":{"spec":{"containers":[{"name":"app","image":%q}]}}}}`, name, image)
}

// TestNodeImagePruneSparesReadsJobsAndDeployments reads the trimmed real capture
// and expects every digest its Jobs, Deployments and Pods run, sorted once each.
func TestNodeImagePruneSparesReadsJobsAndDeployments(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(nodeImagePruneRealList)
	if err != nil {
		t.Fatal(err)
	}
	got, err := nodeImagePruneSpares(data)
	if err != nil {
		t.Fatalf("nodeImagePruneSpares: %v", err)
	}
	want := []string{
		"sha256:3302c7f6e863175bb77bd682c081e10ac938ffe9511569baea5bb0c6019e6d2d",
		"sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e",
		"sha256:c5e35ea5f4442e25c378022089b509bf0acfb7fab2ab9f60000cbf98fc85a212",
		"sha256:d2bbd7dd53fadc7eae01c5bb54584644a3a4a2775cf5f3b2e0023f1de4a25311",
		"sha256:d5c6c79d0509d74502eaaea16aa00d98e73cfcb172b6f194808513c6d15aeefe",
		"sha256:f64129648afd482fa556e285cab750611069f7234048ddbe203da451ae078494",
		"sha256:fecacd4f8de160c92251196b9f425ba5390abd63ec314d47a88b4a725e9b08a8",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spares = %v, want %v", got, want)
	}
}

// TestNodeImagePruneSparesManyJobs covers the scale of the prune: 24 update Jobs
// with a distinct digest each, and 4 Deployments that add one more digest.
func TestNodeImagePruneSparesManyJobs(t *testing.T) {
	t.Parallel()

	var items []string
	var want []string
	for i := 1; i <= 24; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"Job","metadata":{"name":"gt-update-%02d"},"spec":{"template":{"spec":{"containers":[{"name":"update","image":"forgejo:3000/bot-registry/gastown-daemon@%s"}]}}}}`, i, digestAt(i)))
		want = append(want, digestAt(i))
	}
	items = append(items,
		jsonDeployment("daemon", "forgejo:3000/bot-registry/gastown-daemon@"+digestAt(25)),
		jsonDeployment("dashboard", "forgejo:3000/bot-registry/gastown-daemon@"+digestAt(25)),
		jsonDeployment("bd-gateway", "forgejo:3000/bot-registry/gastown-bd@"+digestAt(3)),
		jsonDeployment("trackerd", "forgejo:3000/bot-registry/gastown-daemon@"+digestAt(24)),
	)
	want = append(want, digestAt(25))

	got, err := nodeImagePruneSpares([]byte(jsonList(items...)))
	if err != nil {
		t.Fatalf("nodeImagePruneSpares: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spares has %d digests %v, want %d %v", len(got), got, len(want), want)
	}
}

// TestNodeImagePruneSparesPodStatuses drives the Pod rules: a Pod's spec digest
// and its runtime imageID are each protected, in every status array, and a Pod
// that has not pulled yet is protected by its spec digest alone.
func TestNodeImagePruneSparesPodStatuses(t *testing.T) {
	t.Parallel()

	deploy := jsonDeployment("api", digestN("a"))
	tests := []struct {
		name    string
		listDoc string
		want    []string
	}{
		{
			name: "pending Pod with no status keeps its spec digest",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-pending"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]}}`),
			want: []string{digestN("a"), digestN("b")},
		},
		{
			name: "container with an empty imageID keeps its spec digest",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-1"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]},"status":{"containerStatuses":[{"name":"app","imageID":""}]}}`),
			want: []string{digestN("a"), digestN("b")},
		},
		{
			name: "tag-only spec image is spared by the containerd imageID",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-2"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon:latest"}]},"status":{"containerStatuses":[{"name":"app","imageID":"containerd://`+digestN("c")+`"}]}}`),
			want: []string{digestN("a"), digestN("c")},
		},
		{
			name: "ephemeral container status with a docker-pullable imageID",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-3"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]},"status":{"ephemeralContainerStatuses":[{"name":"debug","imageID":"docker-pullable://forgejo:3000/bot-registry/gastown-bd@`+digestN("d")+`"}]}}`),
			want: []string{digestN("a"), digestN("b"), digestN("d")},
		},
		{
			name: "init container status imageID is protected",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-4"},"spec":{"initContainers":[{"name":"init","image":"alpine:3"}],"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]},"status":{"initContainerStatuses":[{"name":"init","imageID":"docker-pullable://alpine@`+digestN("e")+`"}]}}`),
			want: []string{digestN("a"), digestN("b"), digestN("e")},
		},
		{
			name: "spec digest and runtime imageID both protected when they differ",
			listDoc: jsonList(deploy,
				`{"kind":"Pod","metadata":{"name":"api-5"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]},"status":{"containerStatuses":[{"name":"app","imageID":"docker-pullable://forgejo:3000/bot-registry/gastown-daemon@`+digestN("c")+`"}]}}`),
			want: []string{digestN("a"), digestN("b"), digestN("c")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := nodeImagePruneSpares([]byte(tt.listDoc))
			if err != nil {
				t.Fatalf("nodeImagePruneSpares: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("spares = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNodeImagePruneSparesRefuses drives every refusal: each names the object
// that cannot be spared, so the operator can find it.
func TestNodeImagePruneSparesRefuses(t *testing.T) {
	t.Parallel()

	ok := jsonDeployment("api", digestN("a"))
	tests := []struct {
		name    string
		listDoc string
		wantSub string
	}{
		{
			name: "Job with a tag-only container image",
			listDoc: jsonList(ok,
				`{"kind":"Job","metadata":{"name":"gt-update-old"},"spec":{"template":{"spec":{"containers":[{"name":"update","image":"forgejo:3000/bot-registry/gastown-daemon:latest"}]}}}}`),
			wantSub: `Job "gt-update-old"`,
		},
		{
			name: "Deployment with a tag-only init container image",
			listDoc: jsonList(
				`{"kind":"Deployment","metadata":{"name":"dashboard"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@` + digestN("b") + `"}],"initContainers":[{"name":"proxy","image":"docker.io/alpine/socat:1.8"}]}}}}`),
			wantSub: `Deployment "dashboard"`,
		},
		{
			name: "Deployment with an uppercase digest",
			listDoc: jsonList(
				`{"kind":"Deployment","metadata":{"name":"trackerd"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@sha256:` + strings.Repeat("A", 64) + `"}]}}}}`),
			wantSub: `Deployment "trackerd"`,
		},
		{
			name: "Pod with a tag-only spec image and no status",
			listDoc: jsonList(ok,
				`{"kind":"Pod","metadata":{"name":"dashboard-1"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon:latest"}]}}`),
			wantSub: `Pod "dashboard-1"`,
		},
		{
			name: "Pod with a tag-only spec image and an empty imageID",
			listDoc: jsonList(ok,
				`{"kind":"Pod","metadata":{"name":"dashboard-2"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon:latest"}]},"status":{"containerStatuses":[{"name":"app","imageID":""}]}}`),
			wantSub: `Pod "dashboard-2"`,
		},
		{
			name: "Pod with a malformed imageID",
			listDoc: jsonList(ok,
				`{"kind":"Pod","metadata":{"name":"api-bad"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("b")+`"}]},"status":{"containerStatuses":[{"name":"app","imageID":"docker-pullable://forgejo:3000/bot-registry/gastown-daemon@sha256:abc"}]}}`),
			wantSub: `Pod "api-bad"`,
		},
		{
			name: "Pod with a tag-only imageID",
			listDoc: jsonList(ok,
				`{"kind":"Pod","metadata":{"name":"api-tag"},"spec":{"containers":[{"name":"app","image":"forgejo:3000/bot-registry/gastown-daemon:latest"}]},"status":{"containerStatuses":[{"name":"app","imageID":"docker-pullable://forgejo:3000/bot-registry/gastown-daemon:latest"}]}}`),
			wantSub: `Pod "api-tag"`,
		},
		{
			name:    "document that is a Job, not a List",
			listDoc: `{"kind":"Job","metadata":{"name":"gt-update-x"},"spec":{"template":{"spec":{"containers":[{"name":"update","image":"forgejo:3000/bot-registry/gastown-daemon@` + digestN("c") + `"}]}}}}`,
			wantSub: `gt-update-x`,
		},
		{
			name:    "empty items",
			listDoc: `{"kind":"List","items":[]}`,
			wantSub: "no items",
		},
		{
			name:    "Deployment whose pod template has no containers",
			listDoc: `{"kind":"List","items":[{"kind":"Deployment","metadata":{"name":"gt-daemon"},"spec":{"template":{"spec":{"containers":[]}}}}]}`,
			wantSub: "names no image digest",
		},
		{
			name:    "list with no Deployment",
			listDoc: jsonList(`{"kind":"Job","metadata":{"name":"gt-update-only"},"spec":{"template":{"spec":{"containers":[{"name":"update","image":"forgejo:3000/bot-registry/gastown-daemon@` + digestN("c") + `"}]}}}}`),
			wantSub: "no Deployment",
		},
		{
			name: "item of a kind the prune does not read",
			listDoc: jsonList(ok,
				`{"kind":"Service","metadata":{"name":"gt-svc"}}`),
			wantSub: `Service "gt-svc"`,
		},
		{
			name: "item with no name",
			listDoc: jsonList(ok,
				`{"kind":"Job","spec":{"template":{"spec":{"containers":[{"name":"update","image":"forgejo:3000/bot-registry/gastown-daemon@`+digestN("c")+`"}]}}}}`),
			wantSub: "has no name",
		},
		{
			name:    "document that does not parse",
			listDoc: `{"kind":"List","items":[`,
			wantSub: "does not parse",
		},
		{
			name:    "item that is not an object",
			listDoc: jsonList(ok, `"gt-update-x"`),
			wantSub: "an item does not parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := nodeImagePruneSpares([]byte(tt.listDoc))
			if err == nil {
				t.Fatalf("nodeImagePruneSpares = %v, want a refusal naming %s", got, tt.wantSub)
			}
			if got != nil {
				t.Errorf("refused, but returned %v", got)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not name %s", err, tt.wantSub)
			}
		})
	}
}
