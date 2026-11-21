package landworker

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPostLandOrphanMark(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		ends   []RunEnd
		marked []bool
		at     time.Time
		run    string
	}{
		{"an ended answer marks once", []RunEnd{{Run: 11205, Status: "cancelled", Ended: true}}, []bool{true}, t0, "11205 cancelled"},
		{"a second ended answer keeps the first", []RunEnd{{Run: 11205, Status: "cancelled", Ended: true}, {Run: 11206, Status: "failure", Ended: true}}, []bool{true, false}, t0, "11205 cancelled"},
		{"an unended answer marks nothing", []RunEnd{{Run: 11205, Status: "running"}}, []bool{false}, time.Time{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var e PendingPostLand
			for i, end := range tt.ends {
				if got := e.markOrphan(t0.Add(time.Duration(i)*time.Minute), end); got != tt.marked[i] {
					t.Fatalf("answer %d marked = %v, want %v", i+1, got, tt.marked[i])
				}
			}
			if !e.OrphanedAt.Equal(tt.at) || e.OrphanRun != tt.run {
				t.Errorf("marked at %v as %q, want %v as %q", e.OrphanedAt, e.OrphanRun, tt.at, tt.run)
			}
		})
	}
}

func TestPostLandOrphanFieldsSurviveJSON(t *testing.T) {
	t.Parallel()
	e := PendingPostLand{Land: PostLand{Commit: "abc1234"}, OrphanedAt: time.Date(2026, 11, 21, 12, 10, 0, 0, time.UTC), OrphanRun: "11205 cancelled"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PendingPostLand
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OrphanedAt.Equal(e.OrphanedAt) || got.OrphanRun != e.OrphanRun {
		t.Errorf("after a round trip: OrphanedAt %v, OrphanRun %q; want %v and %q", got.OrphanedAt, got.OrphanRun, e.OrphanedAt, e.OrphanRun)
	}
}
