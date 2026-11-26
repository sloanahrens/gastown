package rigaudit

import (
	"errors"
	"reflect"
	"testing"
)

// TestCheckSettings checks the problems CheckSettings reports for one rig's
// settings, or for a load error, with the loader injected.
func TestCheckSettings(t *testing.T) {
	t.Parallel()
	missingForgejo := []Problem{{"settings", "a: merge_queue.forgejo is missing or empty"}}
	tests := []struct {
		name     string
		settings map[string]any
		loadErr  error
		want     []Problem
	}{
		{"both keys present", map[string]any{"merge_queue": map[string]any{"forgejo": "x", "post_land_command": "y"}}, nil, nil},
		{"forgejo missing", map[string]any{"merge_queue": map[string]any{"post_land_command": "y"}}, nil, missingForgejo},
		{"post_land_command empty string", map[string]any{"merge_queue": map[string]any{"forgejo": "x", "post_land_command": ""}}, nil, []Problem{{"settings", "a: merge_queue.post_land_command is missing or empty"}}},
		{"post_land_command not a string", map[string]any{"merge_queue": map[string]any{"forgejo": "x", "post_land_command": 3}}, nil, []Problem{{"settings", "a: merge_queue.post_land_command is missing or empty"}}},
		{"merge_queue not a map", map[string]any{"merge_queue": "forgejo"}, nil, []Problem{{"settings", "a: merge_queue.forgejo is missing or empty"}, {"settings", "a: merge_queue.post_land_command is missing or empty"}}},
		{"merge_queue absent", map[string]any{}, nil, []Problem{{"settings", "a: merge_queue.forgejo is missing or empty"}, {"settings", "a: merge_queue.post_land_command is missing or empty"}}},
		{"loader error", nil, errors.New("no such file"), []Problem{{"settings", "a: cannot read its settings: no such file"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			load := func(string) (map[string]any, error) { return tc.settings, tc.loadErr }
			if got := CheckSettings([]string{"a"}, load); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CheckSettings(settings=%v, err=%v) = %+v, want %+v", tc.settings, tc.loadErr, got, tc.want)
			}
		})
	}
}
