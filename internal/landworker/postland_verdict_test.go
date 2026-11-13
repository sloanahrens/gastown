package landworker

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verdictTestStore is a main state that answers one read with st or err.
type verdictTestStore struct {
	st  MainState
	err error
}

func (s verdictTestStore) Load() (MainState, error) { return s.st, s.err }
func (s verdictTestStore) Save(MainState) error     { return nil }

// TestVerdictOfReadsTheDurableMainState holds the read-only accessor to the verdict
// the rig's main state records for a commit: pending while its entry is on the
// list, green or red once a run reached a verdict at it, unknown otherwise. An
// unreadable store is an error, never a verdict.
func TestVerdictOfReadsTheDurableMainState(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		st     MainState
		commit string
		want   CommitVerdict
	}{
		{
			name:   "a pending entry for the commit is pending, from its recorded time",
			st:     MainState{PostLandPending: []PendingPostLand{{Land: PostLand{Commit: "tip"}, Since: since}}},
			commit: "tip",
			want:   CommitVerdict{State: CommitVerdictPending, Since: since},
		},
		{
			name: "the oldest of several pending entries for the commit sets the age",
			st: MainState{PostLandPending: []PendingPostLand{
				{Land: PostLand{Commit: "tip"}, Since: since.Add(time.Hour)},
				{Land: PostLand{Commit: "tip"}, Since: since},
			}},
			commit: "tip",
			want:   CommitVerdict{State: CommitVerdictPending, Since: since},
		},
		{
			name: "a pending entry wins over an earlier verdict at the same commit",
			st: MainState{
				LastRun:         "tip",
				LastGreen:       "tip",
				PostLandPending: []PendingPostLand{{Land: PostLand{Commit: "tip"}, Since: since}},
			},
			commit: "tip",
			want:   CommitVerdict{State: CommitVerdictPending, Since: since},
		},
		{
			name:   "the newest run at the commit passed: green",
			st:     MainState{LastRun: "tip", LastGreen: "tip"},
			commit: "tip",
			want:   CommitVerdict{State: CommitVerdictGreen},
		},
		{
			name:   "the newest run at the commit failed: red",
			st:     MainState{LastRun: "tip", LastGreen: "older"},
			commit: "tip",
			want:   CommitVerdict{State: CommitVerdictRed},
		},
		{
			name:   "a commit no run has reached is unknown, even when another commit is green",
			st:     MainState{LastRun: "other", LastGreen: "other"},
			commit: "tip",
			want:   CommitVerdict{},
		},
		{
			name:   "a commit pending on another tip is not this commit's verdict",
			st:     MainState{PostLandPending: []PendingPostLand{{Land: PostLand{Commit: "older"}, Since: since}}},
			commit: "tip",
			want:   CommitVerdict{},
		},
		{
			name:   "an empty state has no verdict for any commit",
			st:     MainState{},
			commit: "tip",
			want:   CommitVerdict{},
		},
		{
			name:   "an empty commit is no verdict, not the empty LastRun of a new state",
			st:     MainState{},
			commit: "",
			want:   CommitVerdict{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &PostLandRunner{Rig: "gastown", State: verdictTestStore{st: tt.st}}
			got, err := p.VerdictOf(tt.commit)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestVerdictOfRefusesWhatItCannotRead holds the accessor to its two refusals: a
// store that fails to load, and a runner with no store. Either is an error the
// caller reads as no verdict.
func TestVerdictOfRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		state      MainStateStore
		wantErrSub string
	}{
		{
			name:       "a store that cannot be loaded",
			state:      verdictTestStore{err: errors.New("disk gone")},
			wantErrSub: "reading the main state: disk gone",
		},
		{
			name:       "no store is wired",
			state:      nil,
			wantErrSub: "no main state store is wired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &PostLandRunner{Rig: "gastown", State: tt.state}
			_, err := p.VerdictOf("tip")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrSub)
		})
	}
}
