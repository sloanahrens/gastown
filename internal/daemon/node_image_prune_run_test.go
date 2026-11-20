package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeKube records the seam calls in order, serves canned reads, and refuses the
// call named fail.
type fakeKube struct {
	calls            []string
	fail             string
	list, logs, sent []byte
}

func (k *fakeKube) call(name string) error {
	k.calls = append(k.calls, name)
	if name == k.fail {
		return fmt.Errorf("%s refused", name)
	}
	return nil
}
func (k *fakeKube) ListJSON(context.Context) ([]byte, error)     { return k.list, k.call("ListJSON") }
func (k *fakeKube) Apply(_ context.Context, m []byte) error      { k.sent = m; return k.call("Apply") }
func (k *fakeKube) WaitComplete(context.Context, string) error   { return k.call("WaitComplete") }
func (k *fakeKube) Logs(context.Context, string) ([]byte, error) { return k.logs, k.call("Logs") }
func (k *fakeKube) Delete(context.Context, string) error         { return k.call("Delete") }

// nodePruneLogDry is a dry run's log as the helper writes it: every line ends in a newline.
const nodePruneLogDry = "node-prune: would remove sha256:bbb gastown-polecat created 2026-11-20T03:00:00Z\n" +
	"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z\n" +
	"node-prune: dry run planned 2 images\n"

// TestNodeImagePruneRunDry runs a dry run against a fake helper log. The planned
// count is the summary's, the calls run in order, and the Job's command has no --apply.
func TestNodeImagePruneRunDry(t *testing.T) {
	t.Parallel()

	kube := &fakeKube{list: []byte(jsonList(jsonDeployment("gt-daemon", digestN("a")))), logs: []byte(nodePruneLogDry)}
	rep, err := runNodeImagePrune(context.Background(), kube, "node-prune-dry", "gt-bd:1", 2, false)
	if err != nil {
		t.Fatalf("runNodeImagePrune(dry run) error = %v", err)
	}
	if got, want := strings.Join(kube.calls, " "), "ListJSON Apply WaitComplete Logs Delete"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if rep.Planned != 2 || len(rep.Lines) != 2 {
		t.Errorf("Planned/Lines = %d/%d, want 2/2", rep.Planned, len(rep.Lines))
	}
	var doc nodePruneJobDoc
	if err := json.Unmarshal(kube.sent, &doc); err != nil {
		t.Fatalf("applied manifest does not parse: %v", err)
	}
	if command := doc.Spec.Template.Spec.Containers[0].Command; slices.Contains(command, "--apply") {
		t.Errorf("dry run command = %q, must not contain --apply", command)
	}
}

// TestNodeImagePruneRunStopsAtFailure refuses one step at a time. Each refusal
// returns an error and no later step runs; a bad listing or image never reaches Apply.
func TestNodeImagePruneRunStopsAtFailure(t *testing.T) {
	t.Parallel()

	good := []byte(jsonList(jsonDeployment("gt-daemon", digestN("a"))))
	for _, tc := range []struct {
		name, fail, calls string
		list              []byte
		emptyImage        bool
	}{
		{name: "list refused", fail: "ListJSON", calls: "ListJSON", list: good},
		{name: "listing has no items", calls: "ListJSON", list: []byte(`{"kind":"List","items":[]}`)},
		{name: "empty image", calls: "ListJSON", list: good, emptyImage: true},
		{name: "apply refused", fail: "Apply", calls: "ListJSON Apply", list: good},
		{name: "wait refused", fail: "WaitComplete", calls: "ListJSON Apply WaitComplete", list: good},
		{name: "logs refused", fail: "Logs", calls: "ListJSON Apply WaitComplete Logs", list: good},
		{name: "delete refused", fail: "Delete", calls: "ListJSON Apply WaitComplete Logs Delete", list: good},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kube := &fakeKube{fail: tc.fail, list: tc.list, logs: []byte(nodePruneLogDry)}
			image := "gt-bd:1"
			if tc.emptyImage {
				image = ""
			}
			if _, err := runNodeImagePrune(context.Background(), kube, "node-prune-x", image, 2, false); err == nil {
				t.Fatalf("runNodeImagePrune error = nil, want a refusal")
			}
			if got := strings.Join(kube.calls, " "); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
		})
	}
}

// TestNodeImagePruneRunFailsClosedBeforeTheJob refuses each step before the Job is
// applied: a failed list, a refused spare list and a refused manifest. Each run ends
// with an error naming its step, and Apply is never called.
func TestNodeImagePruneRunFailsClosedBeforeTheJob(t *testing.T) {
	t.Parallel()

	good := []byte(jsonList(jsonDeployment("gt-daemon", digestN("a"))))
	for _, tc := range []struct {
		name, fail, image, step string
		list                    []byte
	}{
		{name: "list refused", fail: "ListJSON", list: good, image: "gt-bd:1", step: "list the gt namespace"},
		{name: "spare list refused", list: []byte(`{"kind":"List","items":[]}`), image: "gt-bd:1", step: "work out the spare digests"},
		{name: "manifest refused", list: good, image: "", step: "build the job manifest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kube := &fakeKube{fail: tc.fail, list: tc.list, logs: []byte(nodePruneLogDry)}
			_, err := runNodeImagePrune(context.Background(), kube, "node-prune-x", tc.image, 2, false)
			if err == nil {
				t.Fatalf("runNodeImagePrune error = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.step) {
				t.Errorf("error = %q, want it to name the step %q", err, tc.step)
			}
			if slices.Contains(kube.calls, "Apply") || kube.sent != nil {
				t.Errorf("calls = %q, want Apply not called", strings.Join(kube.calls, " "))
			}
		})
	}
}
