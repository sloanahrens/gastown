package rigaudit

import (
	"slices"
	"testing"
)

// TestKnownRigs checks KnownRigs returns the unparked rigs then the parked
// ones, in a slice that changing does not change the inputs.
func TestKnownRigs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		unparked, parked []string
		want             []string
	}{
		{"unparked then parked", []string{"a", "c"}, []string{"b"}, []string{"a", "c", "b"}},
		{"no parked rigs", []string{"a"}, nil, []string{"a"}},
		{"only parked rigs", nil, []string{"b"}, []string{"b"}},
		{"no rigs", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Spare capacity in Unparked catches a result that reuses its backing array.
			in := Input{Unparked: append(make([]string, 0, 4), tc.unparked...), Parked: slices.Clone(tc.parked)}
			got := in.KnownRigs()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("KnownRigs() = %v, want %v", got, tc.want)
			}
			if len(got) > 0 {
				got[0] = "changed"
			}
			if !slices.Equal(in.Unparked, tc.unparked) || !slices.Equal(in.Parked, tc.parked) {
				t.Fatalf("KnownRigs changed its inputs to %v and %v", in.Unparked, in.Parked)
			}
		})
	}
}

func loader(settings map[string]any) func(string) (map[string]any, error) {
	return func(string) (map[string]any, error) { return settings, nil }
}

// daemonConfig nests the daemon's three required rig lists in the town config's shape.
func daemonConfig(landing, scan, cleanup []any) map[string]any {
	return map[string]any{"daemon": map[string]any{"patrols": map[string]any{
		"landing_worker": map[string]any{"rigs": landing},
		"patrol_scan": map[string]any{"rigs": scan,
			"worktree_cleanup": map[string]any{"rigs": cleanup}},
	}}}
}

// TestProblems checks the problems Problems reports for rigs, a config and a
// settings loader, including their order across both checks.
func TestProblems(t *testing.T) {
	t.Parallel()
	good := map[string]any{"merge_queue": map[string]any{"forgejo": "x", "post_land_command": "y"}}
	noForgejo := map[string]any{"merge_queue": map[string]any{"post_land_command": "y"}}
	for _, tc := range []struct {
		name string
		in   Input
		want []Problem
	}{
		{"clean input", Input{Unparked: []string{"a"}, Parked: []string{"b"}, Config: daemonConfig([]any{"a", "b"}, []any{"a", "b"}, []any{"a", "b"}), LoadSettings: loader(good)}, nil},
		{"unparked rig missing from a required list", Input{Unparked: []string{"a", "b"}, Config: daemonConfig([]any{"a"}, []any{"a", "b"}, []any{"a", "b"}), LoadSettings: loader(good)}, []Problem{{"required", "daemon.patrols.landing_worker.rigs lacks b"}}},
		{"parked rig missing from every list", Input{Unparked: []string{"a"}, Parked: []string{"b"}, Config: daemonConfig([]any{"a"}, []any{"a"}, []any{"a"}), LoadSettings: loader(good)}, nil},
		{"required list naming only the parked rig counts", Input{Unparked: []string{"a"}, Parked: []string{"b"}, Config: daemonConfig([]any{"b"}, []any{"a", "b"}, []any{"a", "b"}), LoadSettings: loader(good)}, []Problem{{"required", "daemon.patrols.landing_worker.rigs lacks a"}}},
		{"settings lacking forgejo", Input{Unparked: []string{"a"}, Config: daemonConfig([]any{"a"}, []any{"a"}, []any{"a"}), LoadSettings: loader(noForgejo)}, []Problem{{"settings", "a: merge_queue.forgejo is missing or empty"}}},
		{"both kinds come back sorted", Input{Unparked: []string{"b", "a"}, Config: daemonConfig([]any{"b"}, []any{"b", "a"}, []any{"b", "a"}), LoadSettings: loader(noForgejo)}, []Problem{{"required", "daemon.patrols.landing_worker.rigs lacks a"}, {"settings", "a: merge_queue.forgejo is missing or empty"}, {"settings", "b: merge_queue.forgejo is missing or empty"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Problems(tc.in); !slices.Equal(got, tc.want) {
				t.Fatalf("Problems() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
