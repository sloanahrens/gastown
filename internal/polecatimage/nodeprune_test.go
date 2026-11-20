package polecatimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const nodeEndpoint = "unix:///run/k3s/containerd/containerd.sock"

// nodeRepos are the repositories the fake node holds, five images each.
var nodeRepos = []string{"gastown-polecat", "gastown-daemon", "gastown-bd"}

func imageID(repo string, n int) string { return fmt.Sprintf("%s-%d", repo, n) }

func imageDigest(repo string, n int) string { return fmt.Sprintf("%s@sha256:d%d", repo, n) }

// wouldRemove is the line the dry run prints for image n of repo.
func wouldRemove(repo string, n int) string {
	return fmt.Sprintf("node-prune: would remove %s %s created 2026-11-%02dT00:00:00Z\n", imageID(repo, n), repo, n)
}

// nodeCall is one program call the helper made: the program, its arguments, and the crictl verb in them.
type nodeCall struct {
	name string
	args []string
	verb string
}

// fakeNode is a node of nodeRepos: image n of each repository is created on day n of November 2026,
// tagged <repo>:v<n> and digested <repo>@sha256:d<n>. ps is the reply to `crictl ps`; failImages makes
// the images read fail. run answers the program calls and records each one in calls.
type fakeNode struct {
	t          *testing.T
	ps         string
	failImages bool
	images     []byte
	created    map[string]string
	calls      []nodeCall
}

func newFakeNode(t *testing.T, ps string, failImages bool) *fakeNode {
	t.Helper()
	type image struct {
		ID          string   `json:"id"`
		RepoTags    []string `json:"repoTags"`
		RepoDigests []string `json:"repoDigests"`
	}
	var list struct {
		Images []image `json:"images"`
	}
	created := map[string]string{}
	for _, repo := range nodeRepos {
		for n := 1; n <= 5; n++ {
			id := imageID(repo, n)
			list.Images = append(list.Images, image{
				ID:          id,
				RepoTags:    []string{fmt.Sprintf("%s:v%d", repo, n)},
				RepoDigests: []string{imageDigest(repo, n)},
			})
			created[id] = fmt.Sprintf("2026-11-%02dT00:00:00Z", n)
		}
	}
	images, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("encoding the fake images: %v", err)
	}
	return &fakeNode{t: t, ps: ps, failImages: failImages, images: images, created: created}
}

// run answers one program call from the fake node.
func (n *fakeNode) run(name string, args ...string) ([]byte, error) {
	verb := crictlVerbOf(args)
	n.calls = append(n.calls, nodeCall{name: name, args: args, verb: verb})
	switch verb {
	case "images":
		if n.failImages {
			return nil, errors.New("exit status 1")
		}
		return n.images, nil
	case "inspecti":
		data, err := json.Marshal(map[string]any{
			"info": map[string]any{"imageSpec": map[string]string{"created": n.created[args[len(args)-1]]}},
		})
		if err != nil {
			n.t.Fatalf("encoding the fake inspect reply: %v", err)
		}
		return data, nil
	case "ps":
		return []byte(n.ps), nil
	}
	return nil, fmt.Errorf("unexpected crictl verb %q", verb)
}

// crictlVerbOf is the crictl verb in a call's arguments.
func crictlVerbOf(args []string) string {
	for _, a := range args {
		switch a {
		case "images", "inspecti", "ps", "rmi":
			return a
		}
	}
	return ""
}

// removals counts the calls that remove an image; a dry run makes none.
func (n *fakeNode) removals() int {
	count := 0
	for _, c := range n.calls {
		if c.verb == "rmi" {
			count++
		}
	}
	return count
}

// runNodePrune runs the helper against node, the fake, and returns its exit code and output.
func runNodePrune(node *fakeNode, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := nodePruneWith(args, &out, &errOut, node.run)
	return code, out.String(), errOut.String()
}

func TestNodePruneDryRunNamesWhatItWouldRemove(t *testing.T) {
	t.Parallel()
	node := newFakeNode(t, "{}", false)
	code, out, errOut := runNodePrune(node, "--endpoint", nodeEndpoint)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errOut)
	}
	var want strings.Builder
	for _, repo := range nodeRepos {
		want.WriteString(wouldRemove(repo, 2))
		want.WriteString(wouldRemove(repo, 1))
	}
	want.WriteString("node-prune: dry run planned 6 images\n")
	if out != want.String() {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want.String())
	}
	if n := node.removals(); n != 0 {
		t.Errorf("dry run made %d rmi calls, want 0", n)
	}
}

func TestNodePruneDryRunFailedRead(t *testing.T) {
	t.Parallel()
	node := newFakeNode(t, "{}", true)
	code, out, errOut := runNodePrune(node, "--endpoint", nodeEndpoint)
	if code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if out != "" {
		t.Errorf("stdout %q, want nothing: a node that was not read plans nothing", out)
	}
	if !strings.Contains(errOut, "cannot read the node") {
		t.Errorf("stderr %q, want the reason the node could not be read", errOut)
	}
	if n := node.removals(); n != 0 {
		t.Errorf("made %d rmi calls, want 0", n)
	}
}

// TestNodePruneDryRunSpareKeepsImage: a --spare value, an image id or a digest, is in use, so the plan
// leaves that image out. The last row is a node whose container list is present but empty.
func TestNodePruneDryRunSpareKeepsImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		spare string
		ps    string
	}{
		{"id", imageID("gastown-daemon", 2), "{}"},
		{"digest", imageDigest("gastown-daemon", 2), "{}"},
		{"id on a node with no containers listed", imageID("gastown-daemon", 2), `{"containers":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t, tc.ps, false)
			code, out, errOut := runNodePrune(node, "--endpoint", nodeEndpoint, "--spare", tc.spare)
			if code != 0 {
				t.Fatalf("exit %d, want 0; stderr %q", code, errOut)
			}
			var want strings.Builder
			want.WriteString(wouldRemove("gastown-polecat", 2))
			want.WriteString(wouldRemove("gastown-polecat", 1))
			want.WriteString(wouldRemove("gastown-daemon", 1))
			want.WriteString(wouldRemove("gastown-bd", 2))
			want.WriteString(wouldRemove("gastown-bd", 1))
			want.WriteString("node-prune: dry run planned 5 images\n")
			if out != want.String() {
				t.Errorf("stdout:\n%s\nwant:\n%s", out, want.String())
			}
			if n := node.removals(); n != 0 {
				t.Errorf("dry run made %d rmi calls, want 0", n)
			}
		})
	}
}

// TestNodePruneDryRunRefusesUnreadableContainers: a ps reply that is not a JSON object cannot say which
// images a container uses. Read as no containers, it would plan removing images in use, so the node is
// refused with exit 3 and no would-remove line, with or without a --spare.
func TestNodePruneDryRunRefusesUnreadableContainers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ps   string
		args []string
	}{
		{"null reply", "null", nil},
		{"null reply with a spare", "null", []string{"--spare", imageID("gastown-daemon", 2)}},
		{"reply that is not JSON", "not json", nil},
		{"containers that is not a list", `{"containers":"oops"}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t, tc.ps, false)
			code, out, errOut := runNodePrune(node, append([]string{"--endpoint", nodeEndpoint}, tc.args...)...)
			if code != 3 {
				t.Errorf("exit %d, want 3", code)
			}
			if out != "" {
				t.Errorf("stdout %q, want nothing", out)
			}
			if errOut == "" {
				t.Errorf("stderr empty, want the reason the containers could not be read")
			}
			if n := node.removals(); n != 0 {
				t.Errorf("made %d rmi calls, want 0", n)
			}
		})
	}
}

// TestNodePruneRunsCrictlThroughTheProgram: crictl runs through --crictl-bin and its --crictl-arg lead,
// with the CRI socket as --runtime-endpoint, before the call's own arguments.
func TestNodePruneRunsCrictlThroughTheProgram(t *testing.T) {
	t.Parallel()
	node := newFakeNode(t, "{}", false)
	code, _, errOut := runNodePrune(node, "--endpoint", nodeEndpoint, "--crictl-bin", "k3s", "--crictl-arg", "crictl")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errOut)
	}
	if len(node.calls) == 0 {
		t.Fatal("no program call was made")
	}
	first := node.calls[0]
	if first.name != "k3s" {
		t.Errorf("program %q, want k3s", first.name)
	}
	wantPrefix := "crictl --runtime-endpoint " + nodeEndpoint + " --timeout 120s images"
	if got := strings.Join(first.args, " "); !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("arguments %q, want the prefix %q", got, wantPrefix)
	}
}

// TestNodePruneUsageRefusals: a usage error exits 2 before any program runs, and -h exits 0.
func TestNodePruneUsageRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"-h"}, 0},
		{"unknown flag", []string{"--endpoint", nodeEndpoint, "--bogus"}, 2},
		{"no endpoint", []string{"--keep", "3"}, 2},
		{"negative keep", []string{"--endpoint", nodeEndpoint, "--keep", "-1"}, 2},
		{"empty repository", []string{"--endpoint", nodeEndpoint, "--repos", "gastown-polecat,"}, 2},
		{"empty spare", []string{"--endpoint", nodeEndpoint, "--spare", ""}, 2},
		{"spare with a quote", []string{"--endpoint", nodeEndpoint, "--spare", `a"b`}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t, "{}", false)
			code, _, _ := runNodePrune(node, tc.args...)
			if code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
			if len(node.calls) != 0 {
				t.Errorf("made %d program calls, want none", len(node.calls))
			}
		})
	}
}
