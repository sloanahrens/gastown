package daemon

import (
	"reflect"
	"testing"
	"time"
)

// refusalAdd is one tick fed to a RefusalWindow in a test.
type refusalAdd struct {
	at     time.Time
	counts map[string]uint64
}

// refusalTestBase is a fixed tick time so every sample's age is exact.
var refusalTestBase = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

func TestRefusalWindowGrowth(t *testing.T) {
	t.Parallel()
	const (
		capReason  = "cap"
		busyReason = "busy"
	)

	tests := []struct {
		name string
		adds []refusalAdd
		now  time.Time
		want map[string]uint64
	}{
		{
			// The sample at the base is exactly an hour old and is still in
			// the window, so it anchors the growth.
			name: "growth over an hour of samples",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 1, busyReason: 4}},
				{refusalTestBase.Add(30 * time.Minute), map[string]uint64{capReason: 3, busyReason: 4}},
				{refusalTestBase.Add(time.Hour), map[string]uint64{capReason: 6, busyReason: 5}},
			},
			now:  refusalTestBase.Add(time.Hour),
			want: map[string]uint64{capReason: 5, busyReason: 1},
		},
		{
			name: "samples older than an hour are dropped",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 2}},
				{refusalTestBase.Add(30 * time.Minute), map[string]uint64{capReason: 5}},
				{refusalTestBase.Add(70 * time.Minute), map[string]uint64{capReason: 9}},
			},
			now:  refusalTestBase.Add(70 * time.Minute),
			want: map[string]uint64{capReason: 4},
		},
		{
			// Growth asks about now, so a sample that has aged out since the
			// last Add no longer anchors the window.
			name: "an aged-out anchor is excluded at read time",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 2}},
				{refusalTestBase.Add(30 * time.Minute), map[string]uint64{capReason: 5}},
			},
			now:  refusalTestBase.Add(70 * time.Minute),
			want: map[string]uint64{capReason: 0},
		},
		{
			name: "a reason first seen mid-window starts at zero",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 2}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 3, busyReason: 7}},
			},
			now:  refusalTestBase.Add(20 * time.Minute),
			want: map[string]uint64{capReason: 1, busyReason: 0},
		},
		{
			name: "a reason grows from its first sample in the window",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 2}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 3, busyReason: 7}},
				{refusalTestBase.Add(40 * time.Minute), map[string]uint64{capReason: 4, busyReason: 9}},
			},
			now:  refusalTestBase.Add(40 * time.Minute),
			want: map[string]uint64{capReason: 2, busyReason: 2},
		},
		{
			name: "no samples in the window is an empty map",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 2}},
			},
			now:  refusalTestBase.Add(2 * time.Hour),
			want: map[string]uint64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var window RefusalWindow
			for _, add := range tt.adds {
				window.Add(add.at, add.counts)
			}
			if got := window.Growth(tt.now); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Growth(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestRefusalWindowRebase(t *testing.T) {
	t.Parallel()
	const (
		capReason  = "cap"
		busyReason = "busy"
	)

	tests := []struct {
		name string
		adds []refusalAdd
		now  time.Time
		want map[string]uint64
	}{
		{
			name: "a dropped count re-bases that reason and leaves another alone",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 5, busyReason: 2}},
				{refusalTestBase.Add(10 * time.Minute), map[string]uint64{capReason: 8, busyReason: 4}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 1, busyReason: 4}},
			},
			now:  refusalTestBase.Add(20 * time.Minute),
			want: map[string]uint64{capReason: 0, busyReason: 2},
		},
		{
			// Without the re-base the oldest sample would leave a rise of 5;
			// the drop must still reset the reason.
			name: "a drop re-bases even when it still exceeds the oldest sample",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 10}},
				{refusalTestBase.Add(10 * time.Minute), map[string]uint64{capReason: 20}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 15}},
			},
			now:  refusalTestBase.Add(20 * time.Minute),
			want: map[string]uint64{capReason: 0},
		},
		{
			name: "growth resumes from the re-based count",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 5, busyReason: 2}},
				{refusalTestBase.Add(10 * time.Minute), map[string]uint64{capReason: 8, busyReason: 4}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 1, busyReason: 4}},
				{refusalTestBase.Add(30 * time.Minute), map[string]uint64{capReason: 3, busyReason: 4}},
			},
			now:  refusalTestBase.Add(30 * time.Minute),
			want: map[string]uint64{capReason: 2, busyReason: 2},
		},
		{
			name: "each further drop re-bases again",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 5}},
				{refusalTestBase.Add(10 * time.Minute), map[string]uint64{capReason: 3}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 2}},
				{refusalTestBase.Add(30 * time.Minute), map[string]uint64{capReason: 4}},
			},
			now:  refusalTestBase.Add(30 * time.Minute),
			want: map[string]uint64{capReason: 2},
		},
		{
			name: "a reason absent from a tick is not a drop",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 5}},
				{refusalTestBase.Add(10 * time.Minute), map[string]uint64{}},
				{refusalTestBase.Add(20 * time.Minute), map[string]uint64{capReason: 9}},
			},
			now:  refusalTestBase.Add(20 * time.Minute),
			want: map[string]uint64{capReason: 4},
		},
		{
			name: "a first sample is never a drop",
			adds: []refusalAdd{
				{refusalTestBase, map[string]uint64{capReason: 9}},
			},
			now:  refusalTestBase,
			want: map[string]uint64{capReason: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var window RefusalWindow
			for _, add := range tt.adds {
				window.Add(add.at, add.counts)
			}
			if got := window.Growth(tt.now); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Growth(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}
