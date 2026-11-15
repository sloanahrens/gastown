package git

import (
	"slices"
	"strings"
	"testing"
)

// OperationInProgress names the merge or rebase the handle's runner answers for:
// a marker holding a commit is the operation, a marker printing nothing is
// absent, and a call that fails is an error rather than an absent marker.
func TestOperationInProgress(t *testing.T) {
	t.Parallel()

	const (
		mergeHead      = "rev-parse --revs-only MERGE_HEAD"
		rebaseMergeOn  = "rev-parse --revs-only rebase-merge/onto"
		rebaseApplyOn  = "rev-parse --revs-only rebase-apply/onto"
		markerCommitID = "0123456789abcdef0123456789abcdef01234567"
	)
	tests := []struct {
		name    string
		answers map[string]reply
		want    string
		wantErr bool
		// wantCalls are the probes made, in order: the probe stops at the first
		// marker that answers.
		wantCalls []string
	}{
		{
			name:      "no operation in progress",
			answers:   map[string]reply{mergeHead: ok(""), rebaseMergeOn: ok(""), rebaseApplyOn: ok("")},
			wantCalls: []string{mergeHead, rebaseMergeOn, rebaseApplyOn},
		},
		{
			name:      "a merge in progress",
			answers:   map[string]reply{mergeHead: ok(markerCommitID + "\n")},
			want:      "merge in progress",
			wantCalls: []string{mergeHead},
		},
		{
			name:      "a merge-backed rebase in progress",
			answers:   map[string]reply{mergeHead: ok(""), rebaseMergeOn: ok(markerCommitID + "\n")},
			want:      "rebase in progress",
			wantCalls: []string{mergeHead, rebaseMergeOn},
		},
		{
			name:      "an apply-backed rebase in progress",
			answers:   map[string]reply{mergeHead: ok(""), rebaseMergeOn: ok(""), rebaseApplyOn: ok(markerCommitID + "\n")},
			want:      "rebase in progress",
			wantCalls: []string{mergeHead, rebaseMergeOn, rebaseApplyOn},
		},
		{
			name:      "the first probe fails",
			answers:   map[string]reply{mergeHead: fail(128, "fatal: not a git repository")},
			wantErr:   true,
			wantCalls: []string{mergeHead},
		},
		{
			name:      "a later probe fails",
			answers:   map[string]reply{mergeHead: ok(""), rebaseMergeOn: fail(128, "fatal: not a git repository")},
			wantErr:   true,
			wantCalls: []string{mergeHead, rebaseMergeOn},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(tt.answers)
			g := &Git{workDir: t.TempDir(), exec: s.run}

			got, err := g.OperationInProgress()

			if (err != nil) != tt.wantErr {
				t.Fatalf("OperationInProgress error = %v, want an error: %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("OperationInProgress = %q, want %q", got, tt.want)
			}
			if len(s.unscripted) != 0 {
				t.Errorf("calls the table does not model = %q", s.unscripted)
			}
			var calls []string
			for _, c := range s.calls {
				calls = append(calls, strings.Join(c.args, " "))
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("probes = %q, want %q", calls, tt.wantCalls)
			}
		})
	}
}
