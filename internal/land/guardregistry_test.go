package land

import (
	"slices"
	"testing"
)

func TestGuardRegistryFailures(t *testing.T) {
	t.Parallel()
	registry := []treeWideGuard{{pkg: "internal/a", tests: []string{"TestOne"}}}
	addLine := func(pkg, name string) string {
		return pkg + " " + name + " scans the source tree but is not in treeWideGuards (internal/land/changed.go): add it to the entry for " + pkg + " with the paths it reads as judges, or, if it only walks a fixture, list it in the exemption file with a reason"
	}
	for _, tc := range []struct {
		name   string
		found  []ScanningTest
		exempt map[string]string
		want   []string
	}{
		{name: "an unregistered scanner names the test and both remedies", found: []ScanningTest{{Pkg: "internal/b", Name: "TestScan"}}, want: []string{addLine("internal/b", "TestScan")}},
		{name: "a registered scanner yields nothing", found: []ScanningTest{{Pkg: "internal/a", Name: "TestOne"}}},
		{name: "a test its package's entry does not list is reported", found: []ScanningTest{{Pkg: "internal/a", Name: "TestTwo"}}, want: []string{addLine("internal/a", "TestTwo")}},
		{name: "an exempt scanner yields nothing", found: []ScanningTest{{Pkg: "internal/b", Name: "TestScan"}}, exempt: map[string]string{"internal/b TestScan": "walks testdata"}},
		{name: "an exempt scanner with an empty reason is still exempt", found: []ScanningTest{{Pkg: "internal/b", Name: "TestScan"}}, exempt: map[string]string{"internal/b TestScan": ""}},
		{name: "an exempt test that is registered now gets a delete line", found: []ScanningTest{{Pkg: "internal/a", Name: "TestOne"}}, exempt: map[string]string{"internal/a TestOne": "old reason"}, want: []string{"exemption internal/a TestOne is registered now: delete its line"}},
		{name: "an exempt test that no longer scans gets a delete line", exempt: map[string]string{"internal/b TestGone": "old reason"}, want: []string{"exemption internal/b TestGone names a test that no longer scans the tree: delete its line"}},
		{
			name:   "lines are sorted",
			found:  []ScanningTest{{Pkg: "internal/z", Name: "TestZ"}, {Pkg: "internal/b", Name: "TestB"}},
			exempt: map[string]string{"internal/a TestGone": "old reason"},
			want: []string{
				"exemption internal/a TestGone names a test that no longer scans the tree: delete its line",
				addLine("internal/b", "TestB"),
				addLine("internal/z", "TestZ"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := guardRegistryFailures(tc.found, registry, tc.exempt); !slices.Equal(got, tc.want) {
				t.Errorf("guardRegistryFailures() = %q, want %q", got, tc.want)
			}
		})
	}
}
