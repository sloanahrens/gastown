package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s/sbx"
)

// sbxCompareSeam is the host a gt-sbx compare reads through: the set render it
// answers, the kubectl reads it records, and the answer it gives them.
type sbxCompareSeam struct {
	renders []sbx.Inputs
	reads   []string
	answer  string
	exit    int
}

// kubectl records each read as its program and arguments, and answers it with
// the seam's output and exit.
func (s *sbxCompareSeam) kubectl(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	s.reads = append(s.reads, name+" "+strings.Join(args, " "))
	return s.answer, s.exit, nil
}

// sbxCompareRecord is the recorded render flags of a gt-sbx render, with its
// three paths set.
func sbxCompareRecord() *clusterRenderFlags {
	return &clusterRenderFlags{
		cluster:      clusterSbx,
		sbxManifests: "/sbx/agent-sandbox.yaml",
		sbxChart:     "/sbx/chart",
		sbxValues:    "/sbx/values.yaml",
	}
}

// sbxCompareDeps is a gt-sbx compare over a town that holds the record, when one
// is given. The render answers sbxDriftSet and the reads go to the seam, so no
// test reads a cluster or runs helm.
func sbxCompareDeps(t *testing.T, record *clusterRenderFlags) (clusterDriftDeps, *sbxCompareSeam, *bytes.Buffer) {
	t.Helper()
	town := clusterRenderTestTown(t)
	if record != nil {
		if err := saveRenderFlags(renderFlagsPathIn(town), *record); err != nil {
			t.Fatalf("recording the render flags: %v", err)
		}
	}
	seam := &sbxCompareSeam{}
	out := &bytes.Buffer{}
	render := clusterRenderDepsAt(t, town, out)
	render.renderSbx = func(_ context.Context, in sbx.Inputs) (sbx.Set, error) {
		seam.renders = append(seam.renders, in)
		return sbxDriftSet(), nil
	}
	return clusterDriftDeps{render: render, cluster: clusterSbx, out: out, kubectl: seam.kubectl}, seam, out
}

// TestClusterDriftSbxCompareRendersTheRecordedSetAndFollowsTheCompare holds the
// compare to the recorded render's paths and its status to the cluster's answer:
// nil for a cluster in step, drift for a port held differently, unreadable for a
// read that failed, each through one read.
func TestClusterDriftSbxCompareRendersTheRecordedSetAndFollowsTheCompare(t *testing.T) {
	t.Parallel()
	inStep := `{"kind":"List","items":[` + sbxDriftService("dashboard", 8080) + "," + sbxDriftService("dashboard-runtime", 8080) + `]}`
	drifted := `{"kind":"List","items":[` + sbxDriftService("dashboard", 8081) + "," + sbxDriftService("dashboard-runtime", 8080) + `]}`
	for _, tc := range []struct {
		name   string
		answer string
		exit   int
		want   int
	}{
		{name: "a cluster in step is clean", answer: inStep, want: 0},
		{name: "a difference is drift", answer: drifted, want: 1},
		{name: "a read that exits nonzero is unreadable", exit: 1, want: driftExitUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := sbxCompareRecord()
			d, seam, out := sbxCompareDeps(t, record)
			seam.answer, seam.exit = tc.answer, tc.exit
			if got := driftExit(compareSbxClusterDrift(context.Background(), d)); got != tc.want {
				t.Fatalf("exit = %d, want %d; printed:\n%s", got, tc.want, out)
			}
			if len(seam.renders) != 1 {
				t.Fatalf("rendered the set %d times, want once", len(seam.renders))
			}
			in := seam.renders[0]
			if in.AgentSandboxPath != record.sbxManifests || in.ChartDir != record.sbxChart || in.ValuesPath != record.sbxValues {
				t.Errorf("rendered from %q, %q, %q; want the recorded %q, %q, %q",
					in.AgentSandboxPath, in.ChartDir, in.ValuesPath, record.sbxManifests, record.sbxChart, record.sbxValues)
			}
			if want := "kubectl -n openshell get service -o json"; len(seam.reads) != 1 || seam.reads[0] != want {
				t.Errorf("reads %q, want one plain read %q", seam.reads, want)
			}
		})
	}
}

// TestClusterDriftSbxCompareReadsThroughExecVia holds a docker-exec form to the
// one read it builds, through docker exec.
func TestClusterDriftSbxCompareReadsThroughExecVia(t *testing.T) {
	t.Parallel()
	d, seam, out := sbxCompareDeps(t, sbxCompareRecord())
	d.execVia = "docker-exec:gt-sbx-ctx:sandbox-node"
	seam.answer = `{"kind":"List","items":[` + sbxDriftService("dashboard", 8080) + "," + sbxDriftService("dashboard-runtime", 8080) + `]}`
	if err := compareSbxClusterDrift(context.Background(), d); err != nil {
		t.Fatalf("compare = %v, want a cluster in step; printed:\n%s", err, out)
	}
	want := "docker --context gt-sbx-ctx exec sandbox-node kubectl --kubeconfig /etc/kubernetes/admin.conf -n openshell get service -o json"
	if len(seam.reads) != 1 || seam.reads[0] != want {
		t.Errorf("reads %q, want one read %q", seam.reads, want)
	}
}

// TestClusterDriftSbxCompareInterimHeaderFollowsTheExecViaForm holds the interim
// header to the docker-exec form: printed for a context and a container, and not
// printed for the empty form, which reads with a plain kubectl.
func TestClusterDriftSbxCompareInterimHeaderFollowsTheExecViaForm(t *testing.T) {
	t.Parallel()
	inStep := `{"kind":"List","items":[` + sbxDriftService("dashboard", 8080) + "," + sbxDriftService("dashboard-runtime", 8080) + `]}`
	for _, tc := range []struct {
		name    string
		execVia string
		want    bool
	}{
		{name: "the docker-exec form prints the header", execVia: "docker-exec:gt-sbx-ctx:sandbox-node", want: true},
		{name: "the empty form prints no header", execVia: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, seam, out := sbxCompareDeps(t, sbxCompareRecord())
			d.execVia = tc.execVia
			seam.answer = inStep
			if err := compareSbxClusterDrift(context.Background(), d); err != nil {
				t.Fatalf("compare = %v, want a cluster in step; printed:\n%s", err, out)
			}
			if got := strings.Contains(out.String(), ClusterExecViaInterimHeader); got != tc.want {
				t.Errorf("header printed = %t, want %t; printed:\n%s", got, tc.want, out)
			}
		})
	}
}

// TestClusterDriftSbxCompareRefusesBeforeAnyRead holds each input the compare
// cannot use to a refusal with nothing read: no town, a record that does
// not decode, no record, an exec-via form the guard does not accept, and a render
// that refuses.
func TestClusterDriftSbxCompareRefusesBeforeAnyRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		record  *clusterRenderFlags
		adjust  func(*clusterDriftDeps)
		corrupt bool
		wantErr string
	}{
		{name: "there is no town", record: sbxCompareRecord(), adjust: func(d *clusterDriftDeps) {
			d.render.findTown = func() (string, error) { return "", errors.New("no town here") }
		}, wantErr: "not in a Gas Town workspace"},
		{name: "the recorded flags do not decode", record: sbxCompareRecord(), corrupt: true, wantErr: "are refused"},
		{name: "no render has recorded its flags", record: nil, wantErr: "no live render has recorded its flags"},
		{name: "the exec-via form is not accepted", record: sbxCompareRecord(), adjust: func(d *clusterDriftDeps) { d.execVia = "ssh:gt-sbx-ctx:sandbox-node" }, wantErr: "is not accepted"},
		{name: "the render refuses", record: sbxCompareRecord(), adjust: func(d *clusterDriftDeps) {
			d.render.renderSbx = func(context.Context, sbx.Inputs) (sbx.Set, error) { return sbx.Set{}, errors.New("helm failed") }
		}, wantErr: "cannot render the gt-sbx desired set to compare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, seam, out := sbxCompareDeps(t, tc.record)
			if tc.adjust != nil {
				tc.adjust(&d)
			}
			if tc.corrupt {
				root, err := d.render.findTown()
				if err != nil {
					t.Fatalf("finding the town: %v", err)
				}
				if err := os.WriteFile(renderFlagsPathIn(root), []byte("not json"), 0o600); err != nil {
					t.Fatalf("corrupting the recorded flags: %v", err)
				}
			}
			err := compareSbxClusterDrift(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want one naming %q; printed:\n%s", err, tc.wantErr, out)
			}
			if len(seam.reads) != 0 {
				t.Errorf("read %q, want nothing", seam.reads)
			}
		})
	}
}
