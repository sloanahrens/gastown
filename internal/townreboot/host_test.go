package townreboot

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// dockerListing is a Runner that prints out for every call.
func dockerListing(out string) Runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(out), nil
	}
}

// databaseRow runs the databases check over run and returns its row, the form
// the report shows.
func databaseRow(run Runner) Row {
	return RunAll(context.Background(), []Check{DatabaseCheck(run)})[0]
}

// TestRebootCheckHostDatabasesRows pins the databases check's findings: OK when
// all three containers are listed, a not-OK row with the start fix when one is
// missing, and a not-OK row naming the error when the call fails (gt-k3bf8x).
func TestRebootCheckHostDatabasesRows(t *testing.T) {
	t.Parallel()
	const fix = "docker --context desktop-linux start gt-dolt hm-postgres forgejo-db-1"
	tests := []struct {
		name       string
		run        Runner
		wantOK     bool
		wantDetail string
		wantFix    string
	}{
		{
			name:       "all three running",
			run:        dockerListing("gt-dolt\nhm-postgres\nforgejo-db-1\n"),
			wantOK:     true,
			wantDetail: "gt-dolt, hm-postgres, forgejo-db-1 running",
		},
		{
			name:       "one missing",
			run:        dockerListing("gt-dolt\nhm-postgres\n"),
			wantDetail: "not running: forgejo-db-1",
			wantFix:    fix,
		},
		{
			name:       "a name that contains a database name is not a match",
			run:        dockerListing("gt-dolt\nhm-postgres\nforgejo-db-1-old\n"),
			wantDetail: "not running: forgejo-db-1",
			wantFix:    fix,
		},
		{
			name: "a failed call",
			run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("docker daemon is unreachable")
			},
			wantDetail: "list containers: docker daemon is unreachable",
		},
		{
			name:       "no runner",
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := databaseRow(tt.run)
			if row.Name != "databases" {
				t.Errorf("row name = %q, want databases", row.Name)
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

// TestRebootCheckHostDatabasesReadOnly pins that the check only reads: the
// Runner sees the one ps call and never the start fix, whether or not a
// database is missing (gt-k3bf8x).
func TestRebootCheckHostDatabasesReadOnly(t *testing.T) {
	t.Parallel()
	psCall := []string{"docker", "--context", "desktop-linux", "ps", "--format", "{{.Names}}"}
	tests := []struct {
		name string
		out  string
	}{
		{name: "all running", out: "gt-dolt\nhm-postgres\nforgejo-db-1\n"},
		{name: "one missing", out: "gt-dolt\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tt.out), nil
			}
			databaseRow(run)
			if want := [][]string{psCall}; !reflect.DeepEqual(calls, want) {
				t.Errorf("runner calls = %q, want only %q", calls, want)
			}
		})
	}
}
