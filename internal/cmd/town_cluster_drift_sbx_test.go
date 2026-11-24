package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/deploy/k8s/sbx"
	"github.com/steveyegge/gastown/internal/update"
)

// sbxDriftServiceDoc is a rendered Service in the openshell namespace.
const sbxDriftServiceDoc = `apiVersion: v1
kind: Service
metadata:
  name: dashboard
  namespace: openshell
spec:
  ports:
  - port: 8080
    protocol: TCP
`

// sbxDriftSet is the set the rows compare, with a runtime Service beside it.
func sbxDriftSet() sbx.Set {
	return sbx.Set{
		Objects: []k8s.StaticObject{{Kind: "Service", Name: "dashboard", Namespace: "openshell", Doc: []byte(sbxDriftServiceDoc)}},
		Runtime: []sbx.ObjectRef{{Kind: "Service", Namespace: "openshell", Name: "dashboard-runtime"}},
	}
}

// sbxDriftTokenDoc is a rendered Service whose label names a value the compare
// must never print.
const sbxDriftTokenDoc = `apiVersion: v1
kind: Service
metadata:
  name: dashboard
  namespace: openshell
  labels:
    gt/token: rendered-token-of-the-town
spec:
  ports:
  - port: 8080
`

// sbxDriftService is a live Service carrying the helm labels and annotations
// the chart stamps on it, which the rendered document does not name.
func sbxDriftService(name string, port int) string {
	return fmt.Sprintf(`{"apiVersion":"v1","kind":"Service","metadata":{"name":%q,"namespace":"openshell","labels":{"app.kubernetes.io/managed-by":"Helm"},"annotations":{"meta.helm.sh/release-name":"openshell"}},"spec":{"ports":[{"port":%d,"protocol":"TCP"}],"clusterIP":"10.43.0.5"}}`, name, port)
}

// sbxDriftKubectl is the read runner through the guard clusterKubectl returns,
// answering every read of the rendered Service's namespaced list with one
// output, exit and error.
func sbxDriftKubectl(t *testing.T, out string, exit int, err error) update.RunFunc {
	t.Helper()
	runner, guardErr := clusterKubectl("", "", func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if got := name + " " + strings.Join(args, " "); got != "kubectl -n openshell get service -o json" {
			t.Errorf("read %q, want the dashboard Service list through the guard", got)
		}
		return out, exit, err
	})
	if guardErr != nil {
		t.Fatalf("building the guarded read runner: %v", guardErr)
	}
	return runner
}

// sbxDriftRun runs the compare and returns its error and printed lines.
func sbxDriftRun(t *testing.T, set sbx.Set, runner update.RunFunc) (error, []string) {
	t.Helper()
	var out bytes.Buffer
	err := compareSbxDrift(context.Background(), clusterDriftDeps{out: &out}, set, runner)
	return err, driftLines(&out)
}

// TestClusterDriftSbxCompareReportsAServicePort holds a live port that differs
// from the rendered one to exit 1 naming service/dashboard and the path, a live
// object only Set.Runtime covers to no line, and an unrendered object to its
// own extra line (gt-82a9fz).
func TestClusterDriftSbxCompareReportsAServicePort(t *testing.T) {
	t.Parallel()
	live := `{"kind":"List","items":[` + sbxDriftService("dashboard", 8081) + "," + sbxDriftService("dashboard-runtime", 8080) + "," + sbxDriftService("sandbox-extra", 8080) + `]}`
	err, lines := sbxDriftRun(t, sbxDriftSet(), sbxDriftKubectl(t, live, 0, nil))
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%q", got, err, lines)
	}
	want := []string{
		"service/dashboard: " + driftWantDifference(t, "spec.ports[0].port", 8080, float64(8081)),
		"service/sandbox-extra: extra: the rendered set names no object of that name",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("printed\n%q\nwant\n%q", lines, want)
	}
}

// TestClusterDriftSbxCompareIgnoresHelmMetadata holds a matching live Service,
// with the helm labels and annotations the chart stamps on it, to exit 0 and no
// line: the walk visits the rendered keys only (gt-82a9fz).
func TestClusterDriftSbxCompareIgnoresHelmMetadata(t *testing.T) {
	t.Parallel()
	live := `{"kind":"List","items":[` + sbxDriftService("dashboard", 8080) + `]}`
	err, lines := sbxDriftRun(t, sbxDriftSet(), sbxDriftKubectl(t, live, 0, nil))
	if got := driftExit(err); got != 0 {
		t.Fatalf("exit = %d, want 0 (err: %v); printed:\n%q", got, err, lines)
	}
	if lines != nil {
		t.Errorf("printed %q, want no line", lines)
	}
}

// TestClusterDriftSbxCompareReportsAMissingObject holds a set object the cluster
// lacks to exit 1 with its rendered fields reported absent, under the object's
// kind and name (gt-82a9fz).
func TestClusterDriftSbxCompareReportsAMissingObject(t *testing.T) {
	t.Parallel()
	err, lines := sbxDriftRun(t, sbxDriftSet(), sbxDriftKubectl(t, `{"kind":"List","items":[]}`, 0, nil))
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v)", got, err)
	}
	if want := "service/dashboard: " + driftWantAbsent(t, "kind", "Service"); !slices.Contains(lines, want) {
		t.Errorf("printed %q, want %q among them", lines, want)
	}
}

// TestClusterDriftSbxCompareNamesAnUnreadableObject holds a read the cluster
// fails to exit 2 naming the rendered object it could not compare, never 0 or 1,
// with no line printed (gt-fl4i45).
func TestClusterDriftSbxCompareNamesAnUnreadableObject(t *testing.T) {
	t.Parallel()
	err, lines := sbxDriftRun(t, sbxDriftSet(), sbxDriftKubectl(t, "", 1, nil))
	if got := driftExit(err); got != driftExitUnreadable {
		t.Fatalf("exit = %d, want %d (err: %v)", got, driftExitUnreadable, err)
	}
	if want := "service/dashboard"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
	if lines != nil {
		t.Errorf("printed %q beside the refusal, want nothing", lines)
	}
}

// TestClusterDriftSbxComparePrintsNoValue holds a rendered label that differs
// from the live one to one line naming the path and a fingerprint of each side,
// with neither value in the output or the returned error (gt-fl4i45).
func TestClusterDriftSbxComparePrintsNoValue(t *testing.T) {
	t.Parallel()
	set := sbx.Set{Objects: []k8s.StaticObject{{Kind: "Service", Name: "dashboard", Namespace: "openshell", Doc: []byte(sbxDriftTokenDoc)}}}
	live := `{"kind":"List","items":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"dashboard","namespace":"openshell","labels":{"gt/token":"live-token-of-the-town"}},"spec":{"ports":[{"port":8080}]}}]}`
	err, lines := sbxDriftRun(t, set, sbxDriftKubectl(t, live, 0, nil))
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%q", got, err, lines)
	}
	want := []string{"service/dashboard: " + driftWantDifference(t, "metadata.labels.gt/token", "rendered-token-of-the-town", "live-token-of-the-town")}
	if !slices.Equal(lines, want) {
		t.Errorf("printed\n%q\nwant\n%q", lines, want)
	}
	for _, value := range []string{"rendered-token-of-the-town", "live-token-of-the-town"} {
		if strings.Contains(strings.Join(lines, "\n"), value) {
			t.Errorf("printed the value %q, want paths and fingerprints only", value)
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Errorf("the returned error %q holds the value %q, want paths and fingerprints only", err, value)
		}
	}
}

// TestClusterDriftSbxCompareRefusesWhatItCannotRead holds every unreadable
// answer, every set the compare cannot group or place its Secrets in, and every
// Secret read that fails for anything but NotFound to exit 2 and to no line; a
// set row's runner fails the row if it reads at all, and every row whose read
// fails names the object it stood for (gt-82a9fz, gt-fl4i45, gt-eq3891).
func TestClusterDriftSbxCompareRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		set      sbx.Set
		runner   func(*testing.T) update.RunFunc
		wantName string
	}{
		{"an exec that failed", sbxDriftSet(), func(t *testing.T) update.RunFunc { return sbxDriftKubectl(t, "", 0, errors.New("docker exec failed")) }, "service/dashboard"},
		{"a nonzero exit", sbxDriftSet(), func(t *testing.T) update.RunFunc { return sbxDriftKubectl(t, "", 1, nil) }, "service/dashboard"},
		{"an answer that is not JSON", sbxDriftSet(), func(t *testing.T) update.RunFunc { return sbxDriftKubectl(t, "not json", 0, nil) }, "service/dashboard"},
		{"an answer that is not a List", sbxDriftSet(), func(t *testing.T) update.RunFunc { return sbxDriftKubectl(t, `{"kind":"Service"}`, 0, nil) }, "service/dashboard"},
		{"an item that names no object", sbxDriftSet(), func(t *testing.T) update.RunFunc {
			return sbxDriftKubectl(t, `{"kind":"List","items":[{"metadata":{}}]}`, 0, nil)
		}, "service/dashboard"},
		{"an item whose name carries a control character", sbxDriftSet(), func(t *testing.T) update.RunFunc {
			return sbxDriftKubectl(t, `{"kind":"List","items":[{"metadata":{"name":"bad\nname"}}]}`, 0, nil)
		}, "service/dashboard"},
		{"no read runner", sbxDriftSet(), func(*testing.T) update.RunFunc { return nil }, ""},
		{"no rendered object", sbx.Set{}, driftNoRead, ""},
		{"a cluster-scoped object in a namespace", sbx.Set{Objects: []k8s.StaticObject{{Kind: "Service", Name: "dashboard", Namespace: "openshell", Cluster: true, Doc: []byte(sbxDriftServiceDoc)}}}, driftNoRead, ""},
		{"a document naming another object", sbx.Set{Objects: []k8s.StaticObject{{Kind: "Service", Name: "other", Namespace: "openshell", Doc: []byte(sbxDriftServiceDoc)}}}, driftNoRead, ""},
		{"a Secret whose namespace no StatefulSet names", sbx.Set{Objects: []k8s.StaticObject{{Kind: "Service", Name: "dashboard", Namespace: "openshell", Doc: []byte(sbxDriftServiceDoc)}}, Secrets: []string{"openshell-server-tls"}}, driftNoRead, "namespace"},
		{"a Secret beside two StatefulSets", sbx.Set{Objects: []k8s.StaticObject{{Kind: "StatefulSet", Name: "openshell", Namespace: "openshell", Doc: []byte(sbxDriftGatewayDoc)}, {Kind: "StatefulSet", Name: "cache", Namespace: "cache", Doc: []byte(strings.ReplaceAll(sbxDriftGatewayDoc, "openshell", "cache"))}}, Secrets: []string{"openshell-server-tls"}}, driftNoRead, "namespace"},
		{"a Secret read that failed", sbxDriftSecretsSet("openshell-server-tls"), func(t *testing.T) update.RunFunc { return sbxSecretsReadFails(t, "", 0, errors.New("the exec failed")) }, "openshell-server-tls"},
		{"a Secret read that is not NotFound", sbxDriftSecretsSet("openshell-server-tls"), func(t *testing.T) update.RunFunc {
			return sbxSecretsReadFails(t, "Error from server (Forbidden)", 1, nil)
		}, "openshell-server-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err, lines := sbxDriftRun(t, tc.set, tc.runner(t))
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v)", got, driftExitUnreadable, err)
			}
			if tc.wantName != "" && !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q does not name %q", err, tc.wantName)
			}
			if lines != nil {
				t.Errorf("printed %q beside the refusal, want nothing", lines)
			}
		})
	}
}

// sbxDriftGatewayDoc is the gateway StatefulSet, which names the Secrets' namespace.
const sbxDriftGatewayDoc = `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"openshell","namespace":"openshell"},"spec":{"replicas":1}}`

// sbxDriftSecretsSet is the set the Secret rows compare.
func sbxDriftSecretsSet(secrets ...string) sbx.Set {
	return sbx.Set{Objects: []k8s.StaticObject{{Kind: "StatefulSet", Name: "openshell", Namespace: "openshell", Doc: []byte(sbxDriftGatewayDoc)}}, Secrets: secrets}
}

// sbxSecretsReadFails answers every read as given; the Secret check reads first.
func sbxSecretsReadFails(t *testing.T, out string, exit int, err error) update.RunFunc {
	t.Helper()
	runner, guardErr := clusterKubectl("", "", func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
		return out, exit, err
	})
	if guardErr != nil {
		t.Fatalf("building the guarded read runner: %v", guardErr)
	}
	return runner
}

// sbxSecretsKubectl is the read runner through the guard clusterKubectl
// returns: the gateway StatefulSet list, and each Secret get answered with the
// table for a held name and kubectl's NotFound for any other. Any other read
// fails the test, so a --context pair or an output flag on a Secret is caught.
func sbxSecretsKubectl(t *testing.T, held ...string) update.RunFunc {
	t.Helper()
	runner, err := clusterKubectl("", "", func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		read := name + " " + strings.Join(args, " ")
		if read == "kubectl -n openshell get statefulset -o json" {
			return `{"kind":"List","items":[` + sbxDriftGatewayDoc + `]}`, 0, nil
		}
		if secret, ok := strings.CutPrefix(read, "kubectl -n openshell get secret "); ok {
			if slices.Contains(held, secret) {
				return "NAME   TYPE   DATA   AGE\n" + secret + "   kubernetes.io/tls   2   5d\n", 0, nil
			}
			return "Error from server (NotFound): secrets " + secret + " not found", 1, nil
		}
		t.Errorf("read %q, want the gateway list or one Secret get", read)
		return "", 0, nil
	})
	if err != nil {
		t.Fatalf("building the guarded read runner: %v", err)
	}
	return runner
}

// TestClusterDriftSbxSecretsReportTheNamesTheClusterLacks holds one named
// Secret the fake cluster holds, which adds nothing, and one it lacks, which
// exits 1 with the line naming it (gt-eq3891).
func TestClusterDriftSbxSecretsReportTheNamesTheClusterLacks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		secrets []string
		want    int
		lines   []string
	}{
		{"the held name adds nothing", []string{"openshell-server-tls"}, 0, nil},
		{"the absent name is reported", []string{"openshell-server-tls", "openshell-client-tls"}, 1, []string{"secret/openshell-client-tls: missing: the rendered set names it and the cluster does not hold it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err, lines := sbxDriftRun(t, sbxDriftSecretsSet(tc.secrets...), sbxSecretsKubectl(t, "openshell-server-tls"))
			if got := driftExit(err); got != tc.want {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%q", got, tc.want, err, lines)
			}
			if !slices.Equal(lines, tc.lines) {
				t.Errorf("printed\n%q\nwant\n%q", lines, tc.lines)
			}
		})
	}
}

// TestReadSbxLiveRefusesNoNamedObject holds the guard that keeps an empty read
// from formatting a refusal naming nothing, and from reading the kind blind.
// compareSbxDrift only asks for a group it built from at least one object, so
// this is the one seam that drives the guard (gt-fl4i45).
func TestReadSbxLiveRefusesNoNamedObject(t *testing.T) {
	t.Parallel()
	_, err := readSbxLive(context.Background(), driftNoRead(t), sbxDriftGroup{kind: "Service", namespace: "openshell"}, nil)
	if err == nil {
		t.Fatal("read with no named object, want a refusal")
	}
	if strings.Contains(err.Error(), "the live  in") {
		t.Errorf("refusal %q names nothing, want a well-formed message", err)
	}
}
