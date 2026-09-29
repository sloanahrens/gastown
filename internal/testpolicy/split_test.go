package testpolicy

import (
	"reflect"
	"testing"
)

// TestSplitTestArgs checks that the budget runner finds the package list in
// go test's arguments the way go test does: the first run of non-flag
// arguments, skipping the values of flags that take one.
func TestSplitTestArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		args                []string
		before, pkgs, after []string
	}{
		{"make test", []string{"-timeout", "20m", "./..."},
			[]string{"-timeout", "20m"}, []string{"./..."}, nil},
		{"value with equals", []string{"-timeout=20m", "-count=1", "./a", "./b"},
			[]string{"-timeout=20m", "-count=1"}, []string{"./a", "./b"}, nil},
		{"boolean flags take no value", []string{"-v", "-race", "-short", "./a"},
			[]string{"-v", "-race", "-short"}, []string{"./a"}, nil},
		{"double dash and test. prefix", []string{"--run", "TestX", "-test.count", "2", "./a"},
			[]string{"--run", "TestX", "-test.count", "2"}, []string{"./a"}, nil},
		{"flags after the package list", []string{"./a", "./b", "-run", "TestX", "-v"},
			nil, []string{"./a", "./b"}, []string{"-run", "TestX", "-v"}},
		{"a second run of non-flags belongs to the binary", []string{"./a", "-run", "X", "extra"},
			nil, []string{"./a"}, []string{"-run", "X", "extra"}},
		{"no packages", []string{"-count=1"},
			[]string{"-count=1"}, nil, nil},
		{"-args ends go test's own arguments", []string{"-args", "./a"},
			nil, nil, []string{"-args", "./a"}},
		{"an unknown flag closes the package list", []string{"-custom", "./a"},
			nil, nil, []string{"-custom", "./a"}},
		{"-tags and -p take a value", []string{"-tags", "integration", "-p", "4", "./a"},
			[]string{"-tags", "integration", "-p", "4"}, []string{"./a"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before, pkgs, after := SplitTestArgs(tc.args)
			if !reflect.DeepEqual(before, tc.before) || !reflect.DeepEqual(pkgs, tc.pkgs) || !reflect.DeepEqual(after, tc.after) {
				t.Fatalf("SplitTestArgs(%q) = %q, %q, %q; want %q, %q, %q", tc.args, before, pkgs, after, tc.before, tc.pkgs, tc.after)
			}
		})
	}
}

// TestPartitionPackages checks that packages in unconverted.txt go to the
// cached half and every other package, including ones outside the module,
// is judged by the budget.
func TestPartitionPackages(t *testing.T) {
	t.Parallel()
	exempt := map[string]bool{"internal/cmd": true, "internal/beads": true}
	judged, cached := PartitionPackages([]string{
		"m/internal/beads", "m/internal/beads/beadsfake", "m/internal/cmd", "m/internal/git", "other.org/x",
	}, "m", exempt)
	if want := []string{"m/internal/beads/beadsfake", "m/internal/git", "other.org/x"}; !reflect.DeepEqual(judged, want) {
		t.Errorf("judged = %q, want %q", judged, want)
	}
	if want := []string{"m/internal/beads", "m/internal/cmd"}; !reflect.DeepEqual(cached, want) {
		t.Errorf("cached = %q, want %q", cached, want)
	}
}

// TestTagsArgs checks that the build tags go test was given reach go list,
// so both commands see the same package set.
func TestTagsArgs(t *testing.T) {
	t.Parallel()
	got := TagsArgs([]string{"-timeout", "20m", "-tags", "a,b", "--tags=c", "-v"})
	if want := []string{"-tags", "a,b", "--tags=c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TagsArgs = %q, want %q", got, want)
	}
}
