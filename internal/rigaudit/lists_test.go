package rigaudit

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRigLists decodes each JSON document and checks the lists RigLists finds
// in it. Each call repeats because the walk visits map keys in random order.
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
		{"two lists come back sorted", `{"z":["1"],"a":{"b":["2"]}}`, []NamedList{{"a.b", []string{"2"}}, {"z", []string{"1"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var config any
			if err := json.Unmarshal([]byte(tc.json), &config); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			for range 20 {
				if got := RigLists(config); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("RigLists(%s) = %+v, want %+v", tc.json, got, tc.want)
				}
			}
		})
	}
}
