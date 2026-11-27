package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/deploy/k8s/sbx"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/update"
)

func sbxPlanDeps(t *testing.T, objects []k8s.StaticObject) clusterRenderDeps {
	t.Helper()
	d := clusterRenderDepsAt(t, planTown(t), io.Discard)
	d.renderSbx = func(context.Context, sbx.Inputs) (sbx.Set, error) { return sbx.Set{Objects: objects}, nil }
	return d
}

func sbxPlanFlags() planFlags {
	return planFlags{cluster: clusterSbx, policyKubeconfig: "/policy", applyKubeconfig: "/apply",
		sbx: planSbxInputs{manifests: "/manifest", chart: "/chart", values: "/values"}}
}

func sbxPlanObjects(t *testing.T) []k8s.StaticObject {
	t.Helper()
	objects, err := k8s.SbxStatic()
	if err != nil {
		t.Fatalf("reading the gt-sbx static set: %v", err)
	}
	return objects
}

// sbxPlanKube fails a call with no --kubeconfig, so a read through the operator's
// default cluster is caught; a get records the kubeconfig under its resource.
func sbxPlanKube(t *testing.T, gets map[string]string) update.RunFunc {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if name != "kubectl" || len(args) < 3 || args[0] != "--kubeconfig" {
			t.Fatalf("kubectl %s %v: a sandbox plan call with no --kubeconfig", name, args)
		}
		switch args[2] {
		case "apply":
			return "configured", 0, nil
		case "get":
			gets[args[3]] = args[1]
			return "", 0, nil
		}
		t.Fatalf("kubectl %v: a sandbox plan makes only dry runs and gets", args)
		return "", 1, nil
	}
}

// TestClusterShapeSbxPlanRecordsClusterAndPhases: plan.json records gt-sbx, puts the
// cluster-scoped objects in P and the namespaced ones in W by scope, and its id
// differs from the control id for the same files.
func TestClusterShapeSbxPlanRecordsClusterAndPhases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	objects := sbxPlanObjects(t)
	if err := runTownClusterPlan(context.Background(), sbxPlanDeps(t, objects), sbxPlanKube(t, map[string]string{}), io.Discard, dir, sbxPlanFlags(), atomicfile.WriteJSON); err != nil {
		t.Fatalf("runTownClusterPlan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	if rec.Cluster != clusterSbx {
		t.Fatalf("plan.json cluster = %q, want %q", rec.Cluster, clusterSbx)
	}
	want := map[string]string{}
	static := 0
	for i, object := range objects {
		phase := planPhaseWorkloads
		if object.Cluster {
			phase = planPhaseStatic
			static++
		}
		want[sbxObjectFileName(i, object)] = phase
	}
	if len(rec.Files) != len(objects) {
		t.Fatalf("plan.json lists %d files, want %d", len(rec.Files), len(objects))
	}
	for _, f := range rec.Files {
		if f.Phase != want[f.Name] {
			t.Errorf("%s is in phase %s, want %s by its scope", f.Name, f.Phase, want[f.Name])
		}
	}
	if static != 4 {
		t.Errorf("phase P holds %d objects, want the four PriorityClasses", static)
	}
	listed := planListedFiles(t, dir)
	if full, _ := planIDFor(clusterSbx, listed); rec.ID != full {
		t.Errorf("plan.json id = %s, want %s", rec.ID, full)
	}
	if control, _ := planIDFor(clusterControl, listed); control == rec.ID {
		t.Errorf("the gt-sbx id equals the control id for the same files")
	}
}

// TestClusterShapeSbxPlanReadsThroughPhaseKubeconfig: a cluster-scoped object is
// read under the policy kubeconfig and a namespaced one under the apply kubeconfig.
func TestClusterShapeSbxPlanReadsThroughPhaseKubeconfig(t *testing.T) {
	t.Parallel()
	gets := map[string]string{}
	if err := runTownClusterPlan(context.Background(), sbxPlanDeps(t, sbxPlanObjects(t)), sbxPlanKube(t, gets), io.Discard, t.TempDir(), sbxPlanFlags(), atomicfile.WriteJSON); err != nil {
		t.Fatalf("runTownClusterPlan: %v", err)
	}
	flags := sbxPlanFlags()
	var static, workloads int
	for resource, kubeconfig := range gets {
		want := flags.applyKubeconfig
		if strings.HasPrefix(resource, "priorityclass/") {
			want = flags.policyKubeconfig
			static++
		} else {
			workloads++
		}
		if kubeconfig != want {
			t.Errorf("%s was read under %s, want %s by its scope", resource, kubeconfig, want)
		}
	}
	if static == 0 || workloads == 0 {
		t.Fatalf("read %d cluster-scoped and %d namespaced objects, want both", static, workloads)
	}
}

// TestClusterShapeSbxPlanRefuses: a Secret document, an unknown cluster and the sbx
// inputs given to control each refuse before any plan.json is written.
func TestClusterShapeSbxPlanRefuses(t *testing.T) {
	t.Parallel()
	secret := k8s.StaticObject{Kind: "Secret", Name: "bd-token", Namespace: "gt", Doc: []byte("apiVersion: v1\nkind: Secret\n")}
	for _, tc := range []struct {
		name    string
		objects []k8s.StaticObject
		flags   planFlags
		want    string
	}{
		{"a Secret document", append(sbxPlanObjects(t), secret), sbxPlanFlags(), "Secrets stay operator-created"},
		{"an unknown cluster", sbxPlanObjects(t), planFlags{cluster: "gt-prod"}, "not a cluster the plan knows"},
		{"the sbx manifest for control", sbxPlanObjects(t), planFlags{cluster: clusterControl, sbx: planSbxInputs{manifests: "/manifest"}}, "gt-sbx only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			err := runTownClusterPlan(context.Background(), sbxPlanDeps(t, tc.objects), sbxPlanKube(t, map[string]string{}), io.Discard, dir, tc.flags, atomicfile.WriteJSON)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan = %v, want a refusal naming %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !os.IsNotExist(statErr) {
				t.Errorf("plan.json exists after a refusal, want none")
			}
		})
	}
}

// TestClusterShapePlanIDCoversTheCluster: the control id is planID's; any other
// cluster's id differs for the same files and is stable.
func TestClusterShapePlanIDCoversTheCluster(t *testing.T) {
	t.Parallel()
	files := []planFile{{Name: "01-namespace-gt.yaml", Data: []byte("kind: Namespace\n")}}
	control, _ := planID(files)
	if got, _ := planIDFor("", files); got != control {
		t.Errorf("planIDFor(empty) = %s, want the control id %s", got, control)
	}
	if got, _ := planIDFor(clusterControl, files); got != control {
		t.Errorf("planIDFor(control) = %s, want the control id %s", got, control)
	}
	sbxID, _ := planIDFor(clusterSbx, files)
	if sbxID == control {
		t.Errorf("planIDFor(gt-sbx) equals the control id for the same files")
	}
	if again, _ := planIDFor(clusterSbx, files); again != sbxID {
		t.Errorf("planIDFor(gt-sbx) is not stable: %s then %s", sbxID, again)
	}
}
