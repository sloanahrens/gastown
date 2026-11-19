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
