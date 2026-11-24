package land

import "testing"

func TestFollowupCutKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "source file is its own key", path: "internal/cmd/spec.go", want: "internal/cmd/spec.go"},
		{name: "test file pairs with its source", path: "internal/cmd/spec_test.go", want: "internal/cmd/spec.go"},
		{name: "root test file pairs with root source", path: "spec_test.go", want: "spec.go"},
		{name: "test file in testdata pairs in its own directory", path: "internal/cmd/testdata/spec_test.go", want: "internal/cmd/testdata/spec.go"},
		{name: "suffix match is not a test file", path: "internal/cmd/spec_test.txt", want: "internal/cmd/spec_test.txt"},
		{name: "empty path has no key", path: "", want: ""},
		{name: "blank path has no key", path: "   ", want: ""},
		{name: "dot path has no key", path: ".", want: ""},
		{name: "unclean path is cleaned", path: "./internal//land/../land/followup_cut.go", want: "internal/land/followup_cut.go"},
		{name: "surrounding space is trimmed", path: " internal/land/followup_cut.go ", want: "internal/land/followup_cut.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := followupCutKey(tc.path); got != tc.want {
				t.Errorf("followupCutKey(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestFollowupCutKeyPairsTestWithSource(t *testing.T) {
	t.Parallel()
	if followupCutKey("internal/cmd/spec_test.go") != followupCutKey("internal/cmd/spec.go") {
		t.Errorf("test file and its source have different keys: %q, %q",
			followupCutKey("internal/cmd/spec_test.go"), followupCutKey("internal/cmd/spec.go"))
	}
}
