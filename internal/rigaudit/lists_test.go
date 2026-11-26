package rigaudit

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRigLists decodes each JSON document and checks the lists RigLists finds
// in it.
func TestRigLists(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		json string
		want []NamedList
	}{
		{"nested object list", `{"daemon":{"patrols":{"patrol_scan":{"rigs":["a","b"]}}}}`, []NamedList{{"daemon.patrols.patrol_scan.rigs", []string{"a", "b"}}}},
		{"list inside a list of objects", `{"a":{"b":[{},{},{"rigs":["x"]}]}}`, []NamedList{{"a.b[2].rigs", []string{"x"}}}},
		{"mixed list skipped, its objects walked", `{"m":["a",{"rigs":["x"]}]}`, []NamedList{{"m[1].rigs", []string{"x"}}}},
		{"empty list skipped", `{"e":[],"f":{}}`, nil},
		{"scalar config has no lists", `"x"`, nil},
		{"nil config has no lists", `null`, nil},
		// Index 2 is walked before index 10, but "[10]" sorts before "[2]" as a string.
		{"two lists come back sorted by path", `[null,null,["a"],null,null,null,null,null,null,null,["b"]]`, []NamedList{{"[10]", []string{"b"}}, {"[2]", []string{"a"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var config any
			if err := json.Unmarshal([]byte(tc.json), &config); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := RigLists(config); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("RigLists(%s) = %+v, want %+v", tc.json, got, tc.want)
			}
		})
	}
}

// TestCheckLists checks the gaps CheckLists reports for a config document, the
// unparked rigs and the known rigs.
func TestCheckLists(t *testing.T) {
	t.Parallel()
	full := `"daemon":{"patrols":{"landing_worker":{"rigs":["a","b"]},"patrol_scan":{"rigs":["a","b"],"worktree_cleanup":{"rigs":["a","b"]}}}}`
	tests := []struct {
		name string
		json string
		want []Problem
	}{
		{"required list missing a rig", `{"daemon":{"patrols":{"landing_worker":{"rigs":["a"]},"patrol_scan":{"rigs":["a","b"],"worktree_cleanup":{"rigs":["a","b"]}}}}}`, []Problem{{"required", "daemon.patrols.landing_worker.rigs lacks b"}}},
		{"info list missing a rig", `{` + full + `,"extra":{"rigs":["a"]}}`, []Problem{{"info", "extra.rigs lacks b"}}},
		{"list naming no known rig is ignored", `{` + full + `,"other":{"rigs":["x"]}}`, nil},
		{"required path absent", `{"daemon":{"patrols":{"landing_worker":{"rigs":["a","b"]},"patrol_scan":{"rigs":["a","b"]}}}}`, []Problem{{"required", "daemon.patrols.patrol_scan.worktree_cleanup.rigs is absent or names no known rig"}}},
		{"clean config", `{` + full + `}`, nil},
		{"nested index list", `{` + full + `,"x":[{"rigs":["a"]}]}`, []Problem{{"info", "x[0].rigs lacks b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var config any
			if err := json.Unmarshal([]byte(tc.json), &config); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := CheckLists(config, []string{"a", "b"}, []string{"a", "b"}); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CheckLists(%s) = %+v, want %+v", tc.json, got, tc.want)
			}
		})
	}
}
