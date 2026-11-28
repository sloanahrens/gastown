package presubmit

import (
	"slices"
	"strings"
	"testing"
)

// TestRedBasePassed holds one row per rule for naming the top-level tests that passed in `go test -json` output (gt-32afsw).
func TestRedBasePassed(t *testing.T) {
	t.Parallel()
	pass := `{"Action":"pass","Package":"p","Test":"TestA"}`
	long := `{"Action":"output","Package":"p","Output":"` + strings.Repeat("x", 70<<10) + `"}`
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"a top-level pass names its test", pass, []string{"TestA"}},
		{"a subtest pass names nothing", `{"Action":"pass","Package":"p","Test":"TestA/sub"}`, nil},
		{"a package pass names nothing", `{"Action":"pass","Package":"p"}`, nil},
		{"a fail names nothing", `{"Action":"fail","Package":"p","Test":"TestA"}`, nil},
		{"a skip names nothing", `{"Action":"skip","Package":"p","Test":"TestA"}`, nil},
		{"a non-JSON line names nothing", "# example.com/p\n./x.go:3:2: undefined: y", nil},
		{"a pass after a non-JSON line still names its test", "not json\n" + pass, []string{"TestA"}},
		{"a pass after a line longer than a scanner token still names its test", long + "\n" + pass, []string{"TestA"}},
		{"empty output names nothing", "", nil},
		{"a test that passes twice is named once, sorted", `{"Action":"pass","Test":"TestB"}` + "\n" + pass + "\n" + `{"Action":"pass","Test":"TestB"}`, []string{"TestA", "TestB"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RedBasePassed([]byte(tt.out))
			if !slices.Equal(got, tt.want) {
				t.Errorf("RedBasePassed = %v, want %v", got, tt.want)
			}
		})
	}
}
