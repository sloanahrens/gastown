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
// entry. A Job is finished only when its conditions say so, so the Job with a
// Complete condition is finished and each other entry is unfinished.
const updateJobListing = `{"items":[
 {"metadata":{"name":"gt-update-aaaaaaaaaaaa"},"status":{}},
 {"metadata":{"name":"gt-update-bbbbbbbbbbbb"},"status":{"active":1}},
 {"metadata":{"name":"gt-update-cccccccccccc"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}},
 {"metadata":{"name":"gt-update-dddddddddddd"},"status":{"failed":1}},
 {"metadata":{"name":"gt-update-manual"},"status":{"active":1}},
 {"metadata":{"name":"unrelated-job"},"status":{}}
]}`

// updateJobFixtures are the Jobs the cluster can report, one per row, each with
// whether the roll must still hold on it. Pod counts never finish a Job: a failed
// pod with a Job still retrying holds, and only a True Complete or Failed
// condition releases it.
var updateJobFixtures = []struct {
	name       string
	job        string
	wantActive bool
}{
	{name: "a Complete condition is True: finished", wantActive: false,
		job: `{"metadata":{"name":"gt-update-fin1"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`},
	{name: "a Failed condition is True: finished", wantActive: false,
		job: `{"metadata":{"name":"gt-update-fin2"},"status":{"failed":3,"conditions":[{"type":"Failed","status":"True"}]}}`},
	{name: "a failed pod with an active pod and no terminal condition: holds", wantActive: true,
		job: `{"metadata":{"name":"gt-update-run1"},"status":{"active":1,"failed":1}}`},
	{name: "a failed pod between retries with no terminal condition: holds", wantActive: true,
		job: `{"metadata":{"name":"gt-update-run2"},"status":{"active":0,"failed":1}}`},
	{name: "no status at all, just created: holds", wantActive: true,
		job: `{"metadata":{"name":"gt-update-run3"},"status":{}}`},
	{name: "a Complete condition that is not True: holds", wantActive: true,
		job: `{"metadata":{"name":"gt-update-run4"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"False"}]}}`},
	{name: "a Job for another name prefix: ignored", wantActive: false,
		job: `{"metadata":{"name":"unrelated-job"},"status":{"active":1}}`},
}

// TestKubectlUpdateJobFixtureRows reads each Job the cluster can report on its
// own: an unfinished one is named, a finished or foreign one is not.
func TestKubectlUpdateJobFixtureRows(t *testing.T) {
	t.Parallel()
	for _, tt := range updateJobFixtures {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := func(context.Context, string, []string, string, ...string) (string, int, error) {
				return `{"items":[` + tt.job + `]}`, 0, nil
			}
			k := kubectlUpdateJob{dir: t.TempDir(), run: run}
			got, err := k.ActiveJobs(context.Background())
			require.NoError(t, err)
			if !tt.wantActive {
				assert.Empty(t, got, "a finished or foreign Job held the roll")
				return
			}
			assert.Len(t, got, 1, "an unfinished Job did not hold the roll")
		})
	}
}

// TestKubectlUpdateJobActiveJobs holds the active read to what a roll must hold
// on: a gt-update-* Job whose conditions do not yet say it finished. A list that
// cannot be run or read is an error, never an empty list.
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
			want: []string{"gt-update-aaaaaaaaaaaa", "gt-update-bbbbbbbbbbbb", "gt-update-dddddddddddd", "gt-update-manual"},
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
