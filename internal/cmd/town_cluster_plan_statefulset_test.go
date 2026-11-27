package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/update"
)

// sbxPlanStatefulLive and sbxPlanControllerLive are the fake cluster's answers.
const (
	sbxPlanStatefulLive   = `{"metadata":{"name":"openshell"},"status":{"updateRevision":"openshell-7"}}`
	sbxPlanControllerLive = `{"revision":7}`
)

// sbxPlanAnswer answers a get with body at exit 0.
func sbxPlanAnswer(body string) func() (string, int, error) {
	return func() (string, int, error) { return body, 0, nil }
}

// sbxPlanStatefulSet is the gt-sbx gateway StatefulSet as the render writes it.
func sbxPlanStatefulSet() k8s.StaticObject {
	return k8s.StaticObject{Kind: "StatefulSet", Name: "openshell", Namespace: "openshell",
		Doc: []byte("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: openshell\n")}
}

// sbxPlanStatefulSetKube answers the two gets from stateful and controller, every
// other get as absent, dry runs as success, and fails any call it did not expect.
func sbxPlanStatefulSetKube(t *testing.T, stateful, controller func() (string, int, error), gets map[string]string) update.RunFunc {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if _, _, ok := planDryRunOf(name, args); ok {
			return "configured (server dry run)", 0, nil
		}
		if name != "kubectl" || len(args) < 4 || args[0] != "--kubeconfig" || args[1] == "" || args[2] != "get" {
			t.Fatalf("kubectl %s %v: a sandbox plan makes only dry runs and gets under --kubeconfig", name, args)
			return "", 1, nil
		}
		gets[args[3]] = args[1]
		switch {
		case strings.HasPrefix(args[3], "statefulset/"):
			return stateful()
		case strings.HasPrefix(args[3], "controllerrevision/"):
			return controller()
		}
		return "", 0, nil
	}
}

// sbxPlanStatefulSetRecord plans the static set plus a StatefulSet and returns its record.
func sbxPlanStatefulSetRecord(t *testing.T, kube update.RunFunc) planRecordFile {
	t.Helper()
	dir := t.TempDir()
	objects := append(sbxPlanObjects(t), sbxPlanStatefulSet())
	if err := runTownClusterPlan(context.Background(), sbxPlanDeps(t, objects), kube, io.Discard, dir, sbxPlanFlags(), atomicfile.WriteJSON); err != nil {
		t.Fatalf("runTownClusterPlan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	want := sbxObjectFileName(len(objects)-1, objects[len(objects)-1])
	for _, f := range rec.Files {
		if f.Name == want {
			return f
		}
	}
	t.Fatalf("plan.json does not list the StatefulSet file %s", want)
	return planRecordFile{}
}

// TestClusterShapeSbxPlanStatefulSetRecordsRevision: a live StatefulSet records
// its ControllerRevision's number, with both reads under the apply kubeconfig.
func TestClusterShapeSbxPlanStatefulSetRecordsRevision(t *testing.T) {
	t.Parallel()
	gets := map[string]string{}
	kube := sbxPlanStatefulSetKube(t, sbxPlanAnswer(sbxPlanStatefulLive), sbxPlanAnswer(sbxPlanControllerLive), gets)
	got := sbxPlanStatefulSetRecord(t, kube)
	if got.Revision != "7" || got.Absent || got.ResourceVersion != "" {
		t.Fatalf("plan.json for %s = %+v, want revision 7, not absent, no resourceVersion", got.Name, got)
	}
	for _, resource := range []string{"statefulset/openshell", "controllerrevision/openshell-7"} {
		if read := gets[resource]; read != sbxPlanFlags().applyKubeconfig {
			t.Errorf("%s was read under %q, want the apply kubeconfig", resource, read)
		}
	}
}

// TestClusterShapeSbxPlanStatefulSetRefuses: each listed failure ends the plan with an error naming the resource and writes no plan.json.
func TestClusterShapeSbxPlanStatefulSetRefuses(t *testing.T) {
	t.Parallel()
	s, c := sbxPlanAnswer(sbxPlanStatefulLive), sbxPlanAnswer(sbxPlanControllerLive)
	// The compare reads the StatefulSet first, so this answers it once and fails
	// the plan's own read.
	calls := 0
	failAfterCompare := func() (string, int, error) {
		calls++
		if calls > 1 {
			return "", 1, nil
		}
		return s()
	}
	cases := []struct {
		name                 string
		stateful, controller func() (string, int, error)
		resource, want       string
	}{
		{"the StatefulSet read fails", failAfterCompare, c, "statefulset/openshell", "could not be read"},
		{"the ControllerRevision read fails", s, func() (string, int, error) { return "", 1, nil }, "controllerrevision/openshell-7", "could not be read"},
		{"status.updateRevision is missing", sbxPlanAnswer(`{"metadata":{}}`), c, "statefulset/openshell", "has no status.updateRevision"},
		{"status.updateRevision is empty", sbxPlanAnswer(`{"status":{"updateRevision":""}}`), c, "statefulset/openshell", "has no status.updateRevision"},
		{"the ControllerRevision is absent", s, sbxPlanAnswer(""), "controllerrevision/openshell-7", "does not hold"},
		{"the revision is missing", s, sbxPlanAnswer(`{}`), "controllerrevision/openshell-7", "has no revision"},
		{"the revision is not a number", s, sbxPlanAnswer(`{"revision":"seven"}`), "controllerrevision/openshell-7", "not a positive whole number"},
		{"the revision is not whole", s, sbxPlanAnswer(`{"revision":1.5}`), "controllerrevision/openshell-7", "not a positive whole number"},
		{"the revision is zero", s, sbxPlanAnswer(`{"revision":0}`), "controllerrevision/openshell-7", "not a positive whole number"},
		{"the revision is negative", s, sbxPlanAnswer(`{"revision":-3}`), "controllerrevision/openshell-7", "not a positive whole number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			kube := sbxPlanStatefulSetKube(t, tc.stateful, tc.controller, map[string]string{})
			objects := append(sbxPlanObjects(t), sbxPlanStatefulSet())
			err := runTownClusterPlan(context.Background(), sbxPlanDeps(t, objects), kube, io.Discard, dir, sbxPlanFlags(), atomicfile.WriteJSON)
			if err == nil || !strings.Contains(err.Error(), tc.resource) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan = %v, want an error naming %s and holding %q", err, tc.resource, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !os.IsNotExist(statErr) {
				t.Errorf("plan.json exists after a refusal, want none")
			}
		})
	}
}

// TestClusterShapeSbxPlanStatefulSetAbsent: a StatefulSet the cluster does not hold is recorded absent, with no ControllerRevision read.
func TestClusterShapeSbxPlanStatefulSetAbsent(t *testing.T) {
	t.Parallel()
	gets := map[string]string{}
	got := sbxPlanStatefulSetRecord(t, sbxPlanKube(t, gets))
	if !got.Absent || got.Revision != "" || got.ResourceVersion != "" {
		t.Fatalf("plan.json for %s = %+v, want absent with no revision and no resourceVersion", got.Name, got)
	}
	if _, ok := gets["controllerrevision/openshell-7"]; ok {
		t.Errorf("the plan read a ControllerRevision of an absent StatefulSet")
	}
}
