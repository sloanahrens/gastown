package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/update"
)

// updateJobListing is a `kubectl get jobs -o json` answer with one Job per
// entry: a Job with no status has no pod that finished yet, so it is active.
const updateJobListing = `{"items":[
 {"metadata":{"name":"gt-update-aaaaaaaaaaaa"},"status":{}},
 {"metadata":{"name":"gt-update-bbbbbbbbbbbb"},"status":{"active":1}},
 {"metadata":{"name":"gt-update-cccccccccccc"},"status":{"succeeded":1}},
 {"metadata":{"name":"gt-update-dddddddddddd"},"status":{"failed":1}},
 {"metadata":{"name":"gt-update-manual"},"status":{"active":1}},
 {"metadata":{"name":"unrelated-job"},"status":{}}
]}`

// TestKubectlUpdateJobActiveJobs holds the active read to what a roll must hold
// on: a gt-update-* Job that has neither succeeded nor failed. A list that cannot
// be run or read is an error, never an empty list.
func TestKubectlUpdateJobActiveJobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		run        update.RunFunc
		want       []string
		wantErrSub string
	}{
		{
			name: "the unfinished update Jobs are named, the finished and foreign ones are not",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return updateJobListing, 0, nil
			},
			want: []string{"gt-update-aaaaaaaaaaaa", "gt-update-bbbbbbbbbbbb", "gt-update-manual"},
		},
		{
			name: "a list that exits nonzero is an error that names no output",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "secret-bearing output", 1, nil
			},
			wantErrSub: "kubectl get jobs exited 1",
		},
		{
			name: "a list that cannot run is an error",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 0, errors.New("kubectl not found")
			},
			wantErrSub: "running kubectl get jobs",
		},
		{
			name: "an answer that is not a Job list is an error",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "not json", 0, nil
			},
			wantErrSub: "is not a Job list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k := kubectlUpdateJob{dir: t.TempDir(), run: tt.run}
			got, err := k.ActiveJobs(context.Background())
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				assert.NotContains(t, err.Error(), "secret-bearing output", "the error must not echo the command's output")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
