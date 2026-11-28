package presubmit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestRunLandedIntegration holds one row per rule for the flags and the selection in front of the landed integration run: the default base HEAD^1 and a given base reach list, its packages and not-run files reach the run, a list error runs nothing, and an empty base or unknown flag calls nothing (gt-d4usdc).
func TestRunLandedIntegration(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	run := []string{"test", "-tags", "integration", "-count=1", "-run", "^(TestA)$", "./pkg"}
	tests := []struct {
		name       string
		args       []string
		listErr    error
		goErr      error
		want       int
		wantLists  int
		wantBase   string
		wantCalls  [][]string
		wantOut    string
		wantStderr string
	}{
		{name: "the default base is HEAD^1 and the packages and not-run files reach the run", want: 0, wantLists: 1, wantBase: "HEAD^1", wantCalls: [][]string{run}, wantOut: "not run: x_test.go\n"},
		{name: "a given base reaches list", args: []string{"-base", "main"}, want: 0, wantLists: 1, wantBase: "main", wantCalls: [][]string{run}, wantOut: "not run: x_test.go\n"},
		{name: "a failed integration run is the run's own 1", goErr: boom, want: 1, wantLists: 1, wantBase: "HEAD^1", wantCalls: [][]string{run}, wantOut: "not run: x_test.go\n", wantStderr: "integration tests of pkg failed: boom\n"},
		{name: "a list error is 1, printed, and runs no go", listErr: boom, want: 1, wantLists: 1, wantBase: "HEAD^1", wantStderr: "landed: boom\n"},
		{name: "an empty base is 2 and calls nothing", args: []string{"-base", ""}, want: 2, wantStderr: "-base must not be empty"},
		{name: "an unknown flag is 2 and calls nothing", args: []string{"-nope"}, want: 2, wantStderr: "not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var lists int
			var gotBase string
			var calls [][]string
			var out, errOut strings.Builder
			list := func(base string) ([]RedBasePackage, []string, error) {
				lists++
				gotBase = base
				// Packages come back with the error, so ignoring it would run them.
				return []RedBasePackage{{Dir: "pkg", Tests: []RedBaseTest{{Name: "TestA"}}}}, []string{"x_test.go"}, tt.listErr
			}
			goTest := func(args []string) error { calls = append(calls, args); return tt.goErr }
			got := RunLandedIntegration(tt.args, &out, &errOut, list, goTest)
			if got != tt.want {
				t.Errorf("exit = %d, want %d", got, tt.want)
			}
			if lists != tt.wantLists || (lists > 0 && gotBase != tt.wantBase) {
				t.Errorf("list calls = %d, base %q; want %d, base %q", lists, gotBase, tt.wantLists, tt.wantBase)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) || out.String() != tt.wantOut {
				t.Errorf("go test calls %q, stdout %q; want %q, %q", calls, out.String(), tt.wantCalls, tt.wantOut)
			}
			if !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want one containing %q", errOut.String(), tt.wantStderr)
			}
		})
	}
}
