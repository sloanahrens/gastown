package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// fakeKube records the seam calls in order and serves canned reads.
type fakeKube struct {
	calls            []string
	list, logs, sent []byte
}

func (k *fakeKube) call(name string) error                       { k.calls = append(k.calls, name); return nil }
func (k *fakeKube) ListJSON(context.Context) ([]byte, error)     { return k.list, k.call("ListJSON") }
func (k *fakeKube) Apply(_ context.Context, m []byte) error      { k.sent = m; return k.call("Apply") }
func (k *fakeKube) WaitComplete(context.Context, string) error   { return k.call("WaitComplete") }
func (k *fakeKube) Logs(context.Context, string) ([]byte, error) { return k.logs, k.call("Logs") }
func (k *fakeKube) Delete(context.Context, string) error         { return k.call("Delete") }

// TestNodeImagePruneRunDry runs a dry run against a fake helper log whose lines
// end in newlines. The planned count is the summary's, the calls run in order,
// and the Job's command has no --apply.
func TestNodeImagePruneRunDry(t *testing.T) {
	t.Parallel()

	kube := &fakeKube{
		list: []byte(jsonList(jsonDeployment("gt-daemon", digestN("a")))),
		logs: []byte("node-prune: would remove sha256:bbb gastown-polecat created 2026-11-20T03:00:00Z\n" +
			"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z\n" +
			"node-prune: dry run planned 2 images\n"),
	}
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
