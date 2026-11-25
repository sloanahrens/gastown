package specdispatch

import (
	"maps"
	"testing"
	"time"
)

func TestEffectivePriorities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		own       map[string]int
		blockedBy map[string][]string
		want      map[string]Inherited
	}{
		{
			name:      "ready blocker with no own priority takes the P0 it blocks",
			own:       map[string]int{"blocked": 0},
			blockedBy: map[string][]string{"blocked": {"ready"}},
			want:      map[string]Inherited{"ready": {Priority: 0, From: "blocked"}},
		},
		{
			name:      "blocker with own P2 of a P0 takes the P0",
			own:       map[string]int{"blocker": 2, "blocked": 0},
			blockedBy: map[string][]string{"blocked": {"blocker"}},
			want:      map[string]Inherited{"blocker": {Priority: 0, From: "blocked"}},
		},
		{
			name:      "blocker with own P0 of a P2 gets no entry",
			own:       map[string]int{"blocker": 0, "blocked": 2},
			blockedBy: map[string][]string{"blocked": {"blocker"}},
			want:      map[string]Inherited{},
		},
		{
			name:      "chain A blocks B blocks C gives A and B the P0 of C",
			own:       map[string]int{"c": 0},
			blockedBy: map[string][]string{"c": {"b"}, "b": {"a"}},
			want: map[string]Inherited{
				"a": {Priority: 0, From: "c"},
				"b": {Priority: 0, From: "c"},
			},
		},
		{
			name:      "diamond takes the best of both paths",
			own:       map[string]int{"b": 2, "c": 1, "d": 0},
			blockedBy: map[string][]string{"b": {"a"}, "c": {"a"}, "d": {"b", "c"}},
			want: map[string]Inherited{
				"a": {Priority: 0, From: "d"},
				"b": {Priority: 0, From: "d"},
				"c": {Priority: 0, From: "d"},
			},
		},
		{
			name:      "tie between two blocked beads goes to the smallest id",
			own:       map[string]int{"x": 0, "y": 0},
			blockedBy: map[string][]string{"x": {"b"}, "y": {"b"}},
			want:      map[string]Inherited{"b": {Priority: 0, From: "x"}},
		},
		{
			name:      "a bead that blocks nothing is absent",
			own:       map[string]int{"loner": 1, "solo": 0},
			blockedBy: nil,
			want:      map[string]Inherited{},
		},
		{
			name:      "a blocker whose blocked bead has no known priority gains nothing",
			own:       map[string]int{},
			blockedBy: map[string][]string{"x": {"y"}},
			want:      map[string]Inherited{},
		},
	}
	for _, tc := range cases {
		got := EffectivePriorities(tc.own, tc.blockedBy)
		if !maps.Equal(got, tc.want) {
			t.Errorf("%s: EffectivePriorities = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEffectivePrioritiesCycleTerminates(t *testing.T) {
	t.Parallel()
	own := map[string]int{"a": 2, "b": 0}
	blockedBy := map[string][]string{"a": {"b"}, "b": {"a"}}
	done := make(chan map[string]Inherited)
	go func() { done <- EffectivePriorities(own, blockedBy) }()
	want := map[string]Inherited{"a": {Priority: 0, From: "b"}}
	select {
	case got := <-done:
		if !maps.Equal(got, want) {
			t.Errorf("cycle: EffectivePriorities = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard only: the return is the event waited for, and a cycle that never returns fails here rather than hanging the suite
		t.Fatal("cycle: EffectivePriorities did not return; a cycle must terminate")
	}
}
