package land

import "testing"

// podEngineDoc is a pod-engine inventory with one row that cites its file by
// line, one row that names its file bare in the table cell, and prose that
// names a file without a line (gt-63t22c).
const podEngineDoc = "The inventory pins its sites.\n" +
	"`internal/daemon/dolt.go:205` starts the external container.\n" +
	"| File | Symbol | Evidence |\n" +
	"|---|---|---|\n" +
	"| `internal/cmd/dolt.go` | `execDoltLogContainerTail` | the tail |\n" +
	"The file internal/doltserver/host.go is named here without a line.\n"

// TestPodEnginePinned: a repository path is pinned when the doc cites it as
// path:line or names it bare in a table cell. A longer path that merely ends
// with it, the test file of a pinned file and the empty path are not pinned
// (gt-63t22c).
func TestPodEnginePinned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		path string
		want bool
	}{
		{"cited with a line number", podEngineDoc, "internal/daemon/dolt.go", true},
		{"bare path in a table cell", podEngineDoc, "internal/cmd/dolt.go", true},
		{"longer path ends with the pinned path", podEngineDoc, "x/internal/daemon/dolt.go", false},
		{"pinned path is a suffix of a longer cited path", "`other/internal/daemon/dolt.go:12`", "internal/daemon/dolt.go", false},
		{"longer path in a table cell", "| `x/internal/cmd/dolt.go` | s |", "internal/cmd/dolt.go", false},
		{"test file of a pinned file", podEngineDoc, "internal/daemon/dolt_test.go", false},
		{"pinned file of a cited test file", "`internal/daemon/dolt_test.go:9`", "internal/daemon/dolt.go", false},
		{"empty path", podEngineDoc, "", false},
		{"colon with no line number", "see internal/daemon/dolt.go: later", "internal/daemon/dolt.go", false},
		{"prose mention with no line", podEngineDoc, "internal/doltserver/host.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := podEnginePinned(tc.doc, tc.path); got != tc.want {
				t.Fatalf("podEnginePinned(doc, %q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
