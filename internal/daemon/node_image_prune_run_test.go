package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeKube records the seam calls in order, serves canned reads, and refuses the
// calls named fail and alsoFail. The call named cancelOn runs cancel first, and
// Delete records whether its context was cancelled and the deadline it carries.
type fakeKube struct {
	calls                    []string
	fail, alsoFail, cancelOn string
	cancel                   context.CancelFunc
	deleteErr                error
	deleteDeadline           time.Time
	list, logs, sent         []byte
}

func (k *fakeKube) call(name string) error {
	k.calls = append(k.calls, name)
	if name == k.cancelOn {
		k.cancel()
	}
	if name == k.fail || name == k.alsoFail {
		return fmt.Errorf("%s refused", name)
	}
	return nil
}
func (k *fakeKube) ListJSON(context.Context) ([]byte, error)     { return k.list, k.call("ListJSON") }
func (k *fakeKube) Apply(_ context.Context, m []byte) error      { k.sent = m; return k.call("Apply") }
func (k *fakeKube) WaitComplete(context.Context, string) error   { return k.call("WaitComplete") }
func (k *fakeKube) Logs(context.Context, string) ([]byte, error) { return k.logs, k.call("Logs") }
func (k *fakeKube) Delete(ctx context.Context, _ string) error {
	k.deleteErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		k.deleteDeadline = deadline
	}
	return k.call("Delete")
}

// nodePruneLogDry is a dry run's log as the helper writes it: every line ends in a newline.
const nodePruneLogDry = "node-prune: would remove sha256:bbb gastown-polecat created 2026-11-20T03:00:00Z\n" +
	"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z\n" +
	"node-prune: dry run planned 2 images\n"

// zeroNodePruneReport reports whether rep is the zero report: no lines and no counts.
func zeroNodePruneReport(rep nodePruneReport) bool {
	return len(rep.Lines) == 0 && rep.Planned == 0 && rep.Removed == 0 && rep.Failed == 0
}

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
// returns an error and no step after it runs, except the Job's delete once the apply
// was attempted; a bad listing or image never reaches Apply. A refusal before the
// log is read returns the zero report; the refused delete keeps the log's report.
func TestNodeImagePruneRunStopsAtFailure(t *testing.T) {
	t.Parallel()

	good := []byte(jsonList(jsonDeployment("gt-daemon", digestN("a"))))
	for _, tc := range []struct {
		name, fail, calls string
		list              []byte
		emptyImage        bool
		keepsReport       bool
	}{
		{name: "list refused", fail: "ListJSON", calls: "ListJSON", list: good},
		{name: "listing has no items", calls: "ListJSON", list: []byte(`{"kind":"List","items":[]}`)},
		{name: "empty image", calls: "ListJSON", list: good, emptyImage: true},
		{name: "apply refused", fail: "Apply", calls: "ListJSON Apply Delete", list: good},
		{name: "wait refused", fail: "WaitComplete", calls: "ListJSON Apply WaitComplete Delete", list: good},
		{name: "logs refused", fail: "Logs", calls: "ListJSON Apply WaitComplete Logs Delete", list: good},
		{name: "delete refused", fail: "Delete", calls: "ListJSON Apply WaitComplete Logs Delete", list: good, keepsReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kube := &fakeKube{fail: tc.fail, list: tc.list, logs: []byte(nodePruneLogDry)}
			image := "gt-bd:1"
			if tc.emptyImage {
				image = ""
			}
			rep, err := runNodeImagePrune(context.Background(), kube, "node-prune-x", image, 2, false)
			if err == nil {
				t.Fatalf("runNodeImagePrune error = nil, want a refusal")
			}
			if tc.keepsReport {
				if rep.Planned != 2 || len(rep.Lines) != 2 {
					t.Errorf("report = %+v, want the log's report: Planned 2 with 2 lines", rep)
				}
			} else if !zeroNodePruneReport(rep) {
				t.Errorf("report = %+v, want the zero report for a refusal before the log is read", rep)
			}
			if got := strings.Join(kube.calls, " "); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
		})
	}
}

// TestNodeImagePruneRunMalformedLogIsAnError feeds the run a cut log: a would-remove
// line, then the summary with no final newline. The run returns an error naming the
// log and the zero report, never one that reads as an empty plan, and deletes the Job.
func TestNodeImagePruneRunMalformedLogIsAnError(t *testing.T) {
	t.Parallel()

	cut := "node-prune: would remove sha256:bbb gastown-polecat created 2026-11-20T03:00:00Z\n" +
		"node-prune: dry run planned 2 images"
	kube := &fakeKube{list: []byte(jsonList(jsonDeployment("gt-daemon", digestN("a")))), logs: []byte(cut)}
	rep, err := runNodeImagePrune(context.Background(), kube, "node-prune-cut", "gt-bd:1", 2, false)
	if err == nil {
		t.Fatalf("runNodeImagePrune(cut log) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "log") {
		t.Errorf("error = %q, want it to name the log", err)
	}
	if !zeroNodePruneReport(rep) {
		t.Errorf("report = %+v, want the zero report for a log that does not parse", rep)
	}
	if got, want := strings.Join(kube.calls, " "), "ListJSON Apply WaitComplete Logs Delete"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
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
		{name: "deployment with no containers refused", list: []byte(`{"kind":"List","items":[{"kind":"Deployment","metadata":{"name":"gt-daemon"},"spec":{"template":{"spec":{"containers":[]}}}}]}`), image: "gt-bd:1", step: "work out the spare digests"},
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

// TestNodeImagePruneRunDeleteFailureKeepsTheReport refuses the Job's delete after
// an apply whose log reads and parses. The run returns the log's report and an
// error naming the delete, never a zero report.
func TestNodeImagePruneRunDeleteFailureKeepsTheReport(t *testing.T) {
	t.Parallel()

	applied := strings.Repeat("node-prune: removed sha256:a1\n", 7) +
		"node-prune: applied removed 7 failed 2\n"
	kube := &fakeKube{fail: "Delete", list: []byte(jsonList(jsonDeployment("gt-daemon", digestN("a")))), logs: []byte(applied)}
	rep, err := runNodeImagePrune(context.Background(), kube, "node-prune-apply", "gt-bd:1", 2, true)
	if err == nil {
		t.Fatalf("runNodeImagePrune error = nil, want the delete's error")
	}
	if !strings.Contains(err.Error(), "delete job") {
		t.Errorf("error = %q, want it to name the delete", err)
	}
	if rep.Removed != 7 || rep.Failed != 2 {
		t.Errorf("Removed/Failed = %d/%d, want 7/2", rep.Removed, rep.Failed)
	}
}

// TestNodeImagePruneRunFailsClosedDeletesTheJob refuses a step once the apply was
// attempted and cancels the caller's context while that step runs. The Job is still
// deleted, on a context the cancellation does not reach, and the error names each
// step that failed: a refused delete joins its own error to the step's.
func TestNodeImagePruneRunFailsClosedDeletesTheJob(t *testing.T) {
	t.Parallel()

	good := []byte(jsonList(jsonDeployment("gt-daemon", digestN("a"))))
	for _, tc := range []struct {
		name, fail, alsoFail string
		steps                []string
	}{
		{name: "apply refused", fail: "Apply", steps: []string{"apply job"}},
		{name: "wait refused", fail: "WaitComplete", steps: []string{"wait for job"}},
		{name: "logs refused", fail: "Logs", steps: []string{"read the log of job"}},
		{name: "wait and delete refused", fail: "WaitComplete", alsoFail: "Delete", steps: []string{"wait for job", "delete job"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			kube := &fakeKube{fail: tc.fail, alsoFail: tc.alsoFail, cancelOn: tc.fail, cancel: cancel, list: good, logs: []byte(nodePruneLogDry)}
			_, err := runNodeImagePrune(ctx, kube, "node-prune-x", "gt-bd:1", 2, false)
			if err == nil {
				t.Fatalf("runNodeImagePrune error = nil, want a refusal")
			}
			for _, step := range tc.steps {
				if !strings.Contains(err.Error(), step) {
					t.Errorf("error = %q, want it to name the step %q", err, step)
				}
			}
			if ctx.Err() == nil {
				t.Fatalf("caller's context not cancelled; the run never saw a cancellation")
			}
			if !slices.Contains(kube.calls, "Delete") {
				t.Fatalf("calls = %q, want Delete called after the refusal", strings.Join(kube.calls, " "))
			}
			if kube.deleteErr != nil {
				t.Errorf("Delete context err = %v, want it not cancelled with the caller's", kube.deleteErr)
			}
			if kube.deleteDeadline.IsZero() {
				t.Errorf("Delete context has no deadline, want one so a hung delete ends")
			}
		})
	}
}

// TestNodeImagePruneRunDeleteDeadlineIsShort pins that the delete deadline the
// failed-wait and good-read runs give Delete is at most 120 seconds away.
func TestNodeImagePruneRunDeleteDeadlineIsShort(t *testing.T) {
	t.Parallel()

	for _, fail := range []string{"WaitComplete", ""} {
		t.Run("fail="+fail, func(t *testing.T) {
			t.Parallel()
			kube := &fakeKube{fail: fail, list: []byte(jsonList(jsonDeployment("gt-daemon", digestN("a")))), logs: []byte(nodePruneLogDry)}
			before := time.Now()
			if _, err := runNodeImagePrune(context.Background(), kube, "node-prune-x", "gt-bd:1", 2, false); (err != nil) != (fail != "") {
				t.Fatalf("runNodeImagePrune error = %v, want error=%v", err, fail != "")
			}
			if kube.deleteDeadline.IsZero() {
				t.Fatalf("Delete context has no deadline, want one so a hung delete ends")
			}
			if away := kube.deleteDeadline.Sub(before); away > 120*time.Second {
				t.Errorf("Delete deadline is %v away, want at most 2m", away)
			}
		})
	}
}
