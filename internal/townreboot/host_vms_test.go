package townreboot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// vmListing is a Runner that prints out for every call.
func vmListing(out string) Runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(out), nil
	}
}

// vmRow runs the VMs check over run and returns its row, the form the report
// shows.
func vmRow(run Runner) Row {
	return RunAll(context.Background(), []Check{VMCheck(run)})[0]
}

// TestRebootCheckHostVMsRows pins the VMs check's findings: OK when gt-control
// and gt-sbx are Running, a not-OK row with the start fix when one is not, and
// a not-OK row when the output cannot be read, never an OK from nothing
// (gt-qrc7hp).
func TestRebootCheckHostVMsRows(t *testing.T) {
	t.Parallel()
	const fix = "gt town vms start"
	tests := []struct {
		name             string
		run              Runner
		wantOK           bool
		wantDetail       string
		wantDetailPrefix string
		wantFix          string
	}{
		{
			name: "both running as one array",
			run: vmListing(`[{"name":"gt-control","status":"Running"},` +
				`{"name":"gt-sbx","status":"Running"}]`),
			wantOK:     true,
			wantDetail: "gt-control, gt-sbx running",
		},
		{
			name: "both running as one object per line",
			run: vmListing("{\"name\":\"gt-sbx\",\"status\":\"Running\"}\n" +
				"{\"name\":\"gt-control\",\"status\":\"Running\"}\n"),
			wantOK:     true,
			wantDetail: "gt-control, gt-sbx running",
		},
		{
			name: "one stopped",
			run: vmListing(`[{"name":"gt-control","status":"Running"},` +
				`{"name":"gt-sbx","status":"Stopped"}]`),
			wantDetail: "not running: gt-sbx (Stopped)",
			wantFix:    fix,
		},
		{
			name:       "one missing from the list",
			run:        vmListing(`[{"name":"gt-control","status":"Running"}]`),
			wantDetail: "not running: gt-sbx (not listed)",
			wantFix:    fix,
		},
		{
			name:       "an empty list names both VMs",
			run:        vmListing(`[]`),
			wantDetail: "not running: gt-control (not listed), gt-sbx (not listed)",
			wantFix:    fix,
		},
		{
			name:             "empty output",
			run:              vmListing(""),
			wantDetailPrefix: "read colima list output: ",
		},
		{
			name:             "output that is not JSON",
			run:              vmListing("gt-control  Running\n"),
			wantDetailPrefix: "read colima list output: ",
		},
		{
			name:             "an array that stops before its closing bracket",
			run:              vmListing(`[{"name":"gt-control","status":"Running"}`),
			wantDetailPrefix: "read colima list output: ",
		},
		{
			name:             "a JSON value that is not a list of VMs",
			run:              vmListing(`"gt-control"`),
			wantDetailPrefix: "read colima list output: ",
		},
		{
			name:             "a valid line followed by a broken one",
			run:              vmListing("{\"name\":\"gt-control\",\"status\":\"Running\"}\n{"),
			wantDetailPrefix: "read colima list output: ",
		},
		{
			name:             "a line with no name is not a VM",
			run:              vmListing(`{"status":"Running"}`),
			wantDetailPrefix: "read colima list output: record 1 has no name",
		},
		{
			name:             "a bare null is not a VM",
			run:              vmListing("null\n"),
			wantDetailPrefix: "read colima list output: record 1 has no name",
		},
		{
			name:             "a null element in the array is not a VM",
			run:              vmListing(`[{"name":"gt-control","status":"Running"},null]`),
			wantDetailPrefix: "read colima list output: record 2 has no name",
		},
		{
			name: "a failed call",
			run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("colima daemon is unreachable")
			},
			wantDetail: "list VMs: colima daemon is unreachable",
		},
		{
			name:       "no runner",
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := vmRow(tt.run)
			if row.Name != "vms" {
				t.Errorf("row name = %q, want vms", row.Name)
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if tt.wantDetail != "" && row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if tt.wantDetailPrefix != "" && !strings.HasPrefix(row.Detail, tt.wantDetailPrefix) {
				t.Errorf("Detail = %q, want prefix %q", row.Detail, tt.wantDetailPrefix)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// TestRebootCheckHostVMsReadOnly pins that the check only reads: the Runner
// sees one list argv and never a start command, whatever the VMs' state
// (gt-qrc7hp).
func TestRebootCheckHostVMsReadOnly(t *testing.T) {
	t.Parallel()
	wantCall := []string{"colima", "list", "--json"}
	tests := []struct {
		name string
		out  string
		err  error
	}{
		{name: "both running", out: `[{"name":"gt-control","status":"Running"},{"name":"gt-sbx","status":"Running"}]`},
		{name: "one stopped", out: `[{"name":"gt-control","status":"Running"},{"name":"gt-sbx","status":"Stopped"}]`},
		{name: "output that cannot be read", out: "not json"},
		{name: "a failed call", err: errors.New("exit status 1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tt.out), tt.err
			}
			vmRow(run)
			if want := [][]string{wantCall}; !reflect.DeepEqual(calls, want) {
				t.Errorf("runner calls = %q, want only %q", calls, want)
			}
		})
	}
}
