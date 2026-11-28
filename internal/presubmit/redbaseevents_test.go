package presubmit

import (
	"reflect"
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

// TestRedBaseRan holds one row per rule for the top-level tests that ran and the package result (gt-gxnlab).
func TestRedBaseRan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want RedBaseEvents
	}{
		{"a pass, a fail and a skip each name a top-level test", `{"Action":"pass","Test":"TestA"}` + "\n" + `{"Action":"fail","Test":"TestC"}` + "\n" + `{"Action":"skip","Test":"TestB"}`,
			RedBaseEvents{Passed: []string{"TestA"}, Ran: []string{"TestA", "TestB", "TestC"}}},
		{"a subtest event names no top-level test", `{"Action":"pass","Test":"TestA/sub"}`, RedBaseEvents{}},
		{"a test-level fail is not the package result", `{"Action":"fail","Test":"TestA"}`, RedBaseEvents{Ran: []string{"TestA"}}},
		{"a package-level fail is the package result", `{"Action":"fail","Package":"p"}`, RedBaseEvents{FailedBuild: true}},
		{"a package-level pass is not a fail", `{"Action":"pass","Package":"p"}`, RedBaseEvents{}},
		{"a package-level skip is not a fail", `{"Action":"skip","Package":"p"}`, RedBaseEvents{}},
		{"a non-JSON line names nothing", "# example.com/p\n./x.go:3:2: undefined: y", RedBaseEvents{}},
		{"empty output names nothing", "", RedBaseEvents{}},
		{"a test named twice is named once, sorted", `{"Action":"fail","Test":"TestB"}` + "\n" + `{"Action":"pass","Test":"TestA"}` + "\n" + `{"Action":"fail","Test":"TestB"}`,
			RedBaseEvents{Passed: []string{"TestA"}, Ran: []string{"TestA", "TestB"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RedBaseRan([]byte(tt.out))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RedBaseRan = %+v, want %+v", got, tt.want)
			}
		})
	}
}
