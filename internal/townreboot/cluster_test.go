package townreboot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The fixtures are the pods list as kubectl prints it, written out here so the
// check's JSON tags are read against kubectl's field names (gt-9deo22).
const (
	readyPodsJSON = `{"items":[
		{"metadata":{"name":"forgejo-0"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"forgejo-runner-abc"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
	]}`
	crashLoopPodsJSON = `{"items":[
		{"metadata":{"name":"forgejo-0"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"forgejo-runner-abc"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}
	]}`
	noReadyConditionJSON = `{"items":[
		{"metadata":{"name":"forgejo-0"},"status":{"conditions":[{"type":"PodScheduled","status":"True"}]}}
	]}`
	emptyPodsJSON = `{"items":[]}`
)

// wantForgejoFix is the Fix text the operations doc gives, written out rather
// than read from the package so a silent edit to the constant is caught.
const wantForgejoFix = "delete a pod that is waiting out its backoff; " +
	"if the pod log says lookup host.lima.internal, run gt town vms start"

// podListing is a Runner that prints out for every call.
func podListing(out string) Runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(out), nil
	}
}

// forgejoRow runs the Forgejo check over run and returns its row, the form the
// report shows.
func forgejoRow(run Runner) Row {
	return RunAll(context.Background(), []Check{ForgejoCheck(run)})[0]
}

// TestRebootCheckClusterForgejoRows pins the Forgejo check's findings: OK when
// every pod is Ready, a not-OK row naming the pod that is not Ready and carrying
// the DNS-hint fix, a not-OK row with no pods, and a failed call or parse
// reported as not OK naming the error (gt-9deo22).
func TestRebootCheckClusterForgejoRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		run            Runner
		wantOK         bool
		wantDetail     string
		detailIsPrefix bool
		wantFix        string
	}{
		{
			name:       "every pod ready",
			run:        podListing(readyPodsJSON),
			wantOK:     true,
			wantDetail: "2 pods ready",
		},
		{
			name:       "a pod crash-looping",
			run:        podListing(crashLoopPodsJSON),
			wantDetail: "not ready: forgejo-runner-abc",
			wantFix:    wantForgejoFix,
		},
		{
			name:       "a pod with no Ready condition yet",
			run:        podListing(noReadyConditionJSON),
			wantDetail: "not ready: forgejo-0",
			wantFix:    wantForgejoFix,
		},
		{
			name:       "no pods deployed",
			run:        podListing(emptyPodsJSON),
			wantDetail: "no pods in namespace forgejo",
		},
		{
			name: "a failed call",
			run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("kubectl could not reach colima-gt-control")
			},
			wantDetail: "list pods: kubectl could not reach colima-gt-control",
		},
		{
			name:           "an unparseable list",
			run:            podListing("not json"),
			wantDetail:     "parse pods: ",
			detailIsPrefix: true,
		},
		{
			name:       "no runner",
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := forgejoRow(tt.run)
			if row.Name != "5 forgejo" {
				t.Errorf("row name = %q, want %q", row.Name, "5 forgejo")
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

// TestRebootCheckClusterForgejoReadOnly pins that the check only reads: the
// Runner sees the one kubectl get call and nothing else, whether every pod is
// Ready or one is not, so the fix is never run (gt-9deo22).
func TestRebootCheckClusterForgejoReadOnly(t *testing.T) {
	t.Parallel()
	getCall := []string{"kubectl", "--context", "colima-gt-control", "-n", "forgejo", "get", "pods", "-o", "json"}
	tests := []struct {
		name string
		out  string
	}{
		{name: "every pod ready", out: readyPodsJSON},
		{name: "a pod crash-looping", out: crashLoopPodsJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tt.out), nil
			}
			forgejoRow(run)
			if want := [][]string{getCall}; !reflect.DeepEqual(calls, want) {
				t.Errorf("runner calls = %q, want only %q", calls, want)
			}
		})
	}
}
