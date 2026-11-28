package presubmit

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// TestRunLandedPackages holds one row per rule for running the integration tests a landing changed (gt-42bk5a).
func TestRunLandedPackages(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name       string
		packages   []RedBasePackage
		notRun     []string
		fail       map[int]error // the call index goTest fails on
		outErr     error         // the first stdout write fails
		want       int
		wantCalls  [][]string
		wantOut    string
		wantStderr string
		wantAtCall []string // stdout seen when each package runs
	}{
		{
			name:     "a failed first package still runs the second, naming it",
			packages: []RedBasePackage{{".", nil, []RedBaseTest{{Name: "TestA"}}}, {"pkg", nil, []RedBaseTest{{Name: "TestB"}, {Name: "TestC"}}}},
			fail:     map[int]error{0: boom},
			want:     1,
			wantCalls: [][]string{
				{"test", "-tags", "integration", "-count=1", "-run", "^(TestA)$", "."},
				{"test", "-tags", "integration", "-count=1", "-run", "^(TestB|TestC)$", "./pkg"},
			},
			wantStderr: "integration tests of . failed: boom\n",
			wantAtCall: []string{"", ""},
		},
		{"no package prints one line and runs no go", nil, nil, nil, nil, 0, nil, "no integration test changed\n", "", nil},
		{
			name:       "not-run files print before any package runs and do not change a pass",
			packages:   []RedBasePackage{{"pkg", nil, []RedBaseTest{{Name: "TestA"}}}},
			notRun:     []string{"a/x_test.go", "pkg/y_plan9_test.go"},
			wantCalls:  [][]string{{"test", "-tags", "integration", "-count=1", "-run", "^(TestA)$", "./pkg"}},
			wantOut:    "not run: a/x_test.go\nnot run: pkg/y_plan9_test.go\n",
			wantAtCall: []string{"not run: a/x_test.go\nnot run: pkg/y_plan9_test.go\n"},
		},
		{"not-run files print with no package, which is 0", nil, []string{"plugins/x_test.go"}, nil, nil, 0, nil, "not run: plugins/x_test.go\nno integration test changed\n", "", nil},
		{"a failed not-run line is 1, not a clean run", nil, []string{"plugins/x_test.go"}, nil, boom, 1, nil, "", "not run: plugins/x_test.go: boom\n", nil},
		{"a failed no-package line is 1, not a clean run", nil, nil, nil, boom, 1, nil, "", "no integration test changed: boom\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut strings.Builder
			var stdout io.Writer = &out
			if tt.outErr != nil {
				stdout = &failOnce{err: tt.outErr}
			}
			var calls [][]string
			var atCall []string
			goTest := func(args []string) error {
				calls, atCall = append(calls, args), append(atCall, out.String())
				return tt.fail[len(calls)-1]
			}
			got := RunLandedPackages(tt.packages, tt.notRun, stdout, &errOut, goTest)
			if got != tt.want || !reflect.DeepEqual(calls, tt.wantCalls) || out.String() != tt.wantOut ||
				errOut.String() != tt.wantStderr || !reflect.DeepEqual(atCall, tt.wantAtCall) {
				t.Errorf("got %d, calls %q, stdout %q, stderr %q, stdout at each call %q; want %d, %q, %q, %q, %q", got, calls, out.String(), errOut.String(), atCall, tt.want, tt.wantCalls, tt.wantOut, tt.wantStderr, tt.wantAtCall)
			}
		})
	}
}
