package townreboot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The replicas fixtures are the Deployments list as kubectl prints it, written
// out here so the check's JSON tags are read against kubectl's field names
// (gt-fvna6b).
const (
	daemonReplicasFix    = "kubectl --context colima-gt-control -n gt scale deploy/daemon --replicas=1"
	dashboardReplicasFix = "kubectl --context colima-gt-control -n gt scale deploy/dashboard --replicas=1"

	replicasGetCall = `kubectl --context colima-gt-control -n gt get deploy daemon dashboard -o json`
)

// deploymentJSON is one Deployment as kubectl prints it, carrying the desired
// replicas and the ready count the check reads.
func deploymentJSON(name string, replicas, ready int) string {
	return fmt.Sprintf(
		`{"metadata":{"name":%q},"spec":{"replicas":%d},"status":{"readyReplicas":%d}}`,
		name, replicas, ready)
}

// deploymentsJSON is the items array kubectl get deploy prints.
func deploymentsJSON(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// replicaListing is a Runner that prints out for every call.
func replicaListing(out string) Runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(out), nil
	}
}

// replicasRow runs the replicas check over run and returns its row, the form
// the report shows.
func replicasRow(run Runner) Row {
	return RunAll(context.Background(), []Check{ReplicasCheck(run)})[0]
}

// TestRebootCheckClusterReplicasRows pins the replicas check's findings: OK when
// the daemon and dashboard each run their desired replicas ready, a not-OK row
// saying a drained stop explains zero replicas and carrying that Deployment's
// scale fix, a not-OK row for desired replicas that are not yet ready, and a
// not-OK row for every read that cannot be made or parsed, which is never read
// as OK (gt-fvna6b).
func TestRebootCheckClusterReplicasRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		out            string
		err            error
		noRunner       bool
		wantOK         bool
		wantDetail     string
		detailIsPrefix bool
		wantFix        string
	}{
		{
			name:       "both deployments ready",
			out:        deploymentsJSON(deploymentJSON("daemon", 1, 1), deploymentJSON("dashboard", 1, 1)),
			wantOK:     true,
			wantDetail: "daemon 1/1 ready, dashboard 1/1 ready",
		},
		{
			name:       "daemon scaled to zero",
			out:        deploymentsJSON(deploymentJSON("daemon", 0, 0), deploymentJSON("dashboard", 1, 1)),
			wantDetail: "daemon has 0 replicas: expected after a drained stop, and nothing is running",
			wantFix:    daemonReplicasFix,
		},
		{
			name:       "dashboard scaled to zero",
			out:        deploymentsJSON(deploymentJSON("daemon", 1, 1), deploymentJSON("dashboard", 0, 0)),
			wantDetail: "dashboard has 0 replicas: expected after a drained stop, and nothing is running",
			wantFix:    dashboardReplicasFix,
		},
		{
			name:       "both scaled to zero",
			out:        deploymentsJSON(deploymentJSON("daemon", 0, 0), deploymentJSON("dashboard", 0, 0)),
			wantDetail: "daemon has 0 replicas: expected after a drained stop, and nothing is running; dashboard has 0 replicas: expected after a drained stop, and nothing is running",
			wantFix:    daemonReplicasFix + " && " + dashboardReplicasFix,
		},
		{
			name:       "the daemon desired replicas are not ready",
			out:        deploymentsJSON(deploymentJSON("daemon", 1, 0), deploymentJSON("dashboard", 1, 1)),
			wantDetail: "daemon 0/1 ready",
		},
		{
			name:       "a deployment is missing from the list",
			out:        deploymentsJSON(deploymentJSON("daemon", 1, 1)),
			wantDetail: "dashboard is not in the deployment list",
		},
		{
			name:       "an empty list",
			out:        deploymentsJSON(),
			wantDetail: "daemon is not in the deployment list; dashboard is not in the deployment list",
		},
		{
			name:       "the Deployment read fails",
			err:        errors.New("kubectl could not reach colima-gt-control"),
			wantDetail: "read deployments: kubectl could not reach colima-gt-control",
		},
		{
			name:           "an unparseable list",
			out:            "not json",
			wantDetail:     "parse deployments: ",
			detailIsPrefix: true,
		},
		{
			name:       "no runner",
			noRunner:   true,
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := replicaListing(tt.out)
			if tt.err != nil {
				run = func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tt.out), tt.err
				}
			}
			if tt.noRunner {
				run = nil
			}
			row := replicasRow(run)
			if row.Name != "6 replicas" {
				t.Errorf("row name = %q, want %q", row.Name, "6 replicas")
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if tt.detailIsPrefix {
				if !strings.HasPrefix(row.Detail, tt.wantDetail) {
					t.Errorf("Detail = %q, want prefix %q", row.Detail, tt.wantDetail)
				}
			} else if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// TestRebootCheckClusterReplicasCommands pins that the check only reads: the
// Runner sees the one kubectl get of both Deployments and no scale, patch or
// other write verb, whether the replicas are up or not (gt-fvna6b).
func TestRebootCheckClusterReplicasCommands(t *testing.T) {
	t.Parallel()
	getCall := []string{"kubectl", "--context", "colima-gt-control", "-n", "gt",
		"get", "deploy", "daemon", "dashboard", "-o", "json"}
	if got := strings.Join(getCall, " "); got != replicasGetCall {
		t.Fatalf("test fixture command = %q, want %q", got, replicasGetCall)
	}
	tests := []struct {
		name string
		out  string
	}{
		{name: "both deployments ready", out: deploymentsJSON(deploymentJSON("daemon", 1, 1), deploymentJSON("dashboard", 1, 1))},
		{name: "daemon scaled to zero", out: deploymentsJSON(deploymentJSON("daemon", 0, 0), deploymentJSON("dashboard", 1, 1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tt.out), nil
			}
			replicasRow(run)
			if want := [][]string{getCall}; !reflect.DeepEqual(calls, want) {
				t.Errorf("runner calls = %q, want only %q", calls, want)
			}
		})
	}
}

// forwardProbeCall is the one read the forward check makes: curl prints the
// dashboard's status code and nothing else.
const forwardProbeCall = `curl --max-time 8 -s -o /dev/null -w %{http_code} http://127.0.0.1:8787/api/state`

// wantForwardFix is the Fix text the row carries, written out rather than read
// from the package's constant so a changed Fix fails here.
const wantForwardFix = "gt town forward plist"

// forwardRow runs the forward check over run and returns its row, the form the
// report shows.
func forwardRow(run Runner) Row {
	return RunAll(context.Background(), []Check{ForwardCheck(run)})[0]
}

// TestRebootCheckClusterForwardRows pins the forward check's findings: OK only
// when the dashboard answers 200; any other status, a failing probe and an
// answer with no status are not-OK rows carrying the forward plist fix
// (gt-nxv4vr).
func TestRebootCheckClusterForwardRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		out        string
		err        error
		noRunner   bool
		wantOK     bool
		wantDetail string
		wantFix    string
	}{
		{
			name:       "the dashboard answers 200",
			out:        "200",
			wantOK:     true,
			wantDetail: "the dashboard answered 200 on http://127.0.0.1:8787/api/state",
		},
		{
			name:       "the dashboard answers 503",
			out:        "503",
			wantDetail: "the dashboard answered 503 on http://127.0.0.1:8787/api/state",
			wantFix:    wantForwardFix,
		},
		{
			name:       "curl cannot reach the forward",
			out:        "000",
			err:        exitStatus(7),
			wantDetail: "probe http://127.0.0.1:8787/api/state: exit status 7",
			wantFix:    wantForwardFix,
		},
		{
			name:       "curl times out with no answer",
			err:        exitStatus(28),
			wantDetail: "probe http://127.0.0.1:8787/api/state: exit status 28",
			wantFix:    wantForwardFix,
		},
		{
			name:       "the probe answers no status",
			wantDetail: "the forward answered no status code",
			wantFix:    wantForwardFix,
		},
		{
			name:       "no runner",
			noRunner:   true,
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := replicaListing(tt.out)
			if tt.err != nil {
				run = func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tt.out), tt.err
				}
			}
			if tt.noRunner {
				run = nil
			}
			row := forwardRow(run)
			if row.Name != "9 forward" {
				t.Errorf("row name = %q, want %q", row.Name, "9 forward")
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// TestRebootCheckClusterForwardCommands pins that the check only reads the
// dashboard: the Runner sees the one curl probe and nothing else, whether the
// dashboard answers or not (gt-nxv4vr).
func TestRebootCheckClusterForwardCommands(t *testing.T) {
	t.Parallel()
	want := []string{"curl", "--max-time", "8", "-s", "-o", "/dev/null",
		"-w", "%{http_code}", "http://127.0.0.1:8787/api/state"}
	if got := strings.Join(want, " "); got != forwardProbeCall {
		t.Fatalf("test fixture command = %q, want %q", got, forwardProbeCall)
	}
	tests := []struct {
		name string
		out  string
	}{
		{name: "the dashboard answers", out: "200"},
		{name: "the dashboard answers 503", out: "503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tt.out), nil
			}
			forwardRow(run)
			if !reflect.DeepEqual(calls, [][]string{want}) {
				t.Errorf("runner calls = %q, want only %q", calls, want)
			}
		})
	}
}

// clusterFake is a Runner that answers every read in ClusterChecks with a
// healthy cluster and records every argv.
func clusterFake(calls *[][]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		switch {
		case name == "curl":
			return []byte("200"), nil
		case strings.Contains(strings.Join(args, " "), "get pods"):
			return []byte(readyPodsJSON), nil
		case name == "kubectl":
			return []byte(deploymentsJSON(deploymentJSON("daemon", 1, 1),
				deploymentJSON("dashboard", 1, 1))), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}
}

// TestRebootCheckClusterOrder pins that ClusterChecks returns the Forgejo,
// replicas and forward checks in the operations doc's order, and that on a
// healthy cluster the Runner ran exactly those three read commands and nothing
// else (gt-ia7aq7).
func TestRebootCheckClusterOrder(t *testing.T) {
	t.Parallel()
	var calls [][]string
	checks := ClusterChecks(clusterFake(&calls))
	rows := RunAll(context.Background(), checks)
	wantNames := []string{"5 forgejo", "6 replicas", "9 forward"}
	if len(rows) != len(wantNames) {
		t.Fatalf("rows = %d, want %d", len(rows), len(wantNames))
	}
	for i, row := range rows {
		if row.Name != wantNames[i] {
			t.Errorf("rows[%d].Name = %q, want %q", i, row.Name, wantNames[i])
		}
		if !row.OK {
			t.Errorf("row %q not OK on a healthy cluster: %q", row.Name, row.Detail)
		}
	}
	wantCalls := [][]string{
		{"kubectl", "--context", "colima-gt-control", "-n", "forgejo", "get", "pods", "-o", "json"},
		{"kubectl", "--context", "colima-gt-control", "-n", "gt",
			"get", "deploy", "daemon", "dashboard", "-o", "json"},
		{"curl", "--max-time", "8", "-s", "-o", "/dev/null",
			"-w", "%{http_code}", "http://127.0.0.1:8787/api/state"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("runner calls = %q, want only %q", calls, wantCalls)
	}
}
