package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestClusterShapePlanFlagsRefuses: each refusal names its cause and returns no
// flags and no town root, so a plan cannot be built from a refused record.
func TestClusterShapePlanFlagsRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		townErr  error
		seed     func(t *testing.T, root string)
		wantText string
	}{
		{
			name:     "town not found",
			townErr:  errors.New("no town above the cwd"),
			seed:     func(t *testing.T, root string) {},
			wantText: "town not found",
		},
		{
			name: "refused record",
			seed: func(t *testing.T, root string) {
				writeRecordedFlagsFile(t, root, "{not json")
			},
			wantText: "are refused",
		},
		{
			name:     "no record",
			seed:     func(t *testing.T, root string) {},
			wantText: "no render has recorded its flags",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.seed(t, root)
			findTown := func() (string, error) { return root, tc.townErr }
			flags, gotRoot, err := planRecordedFlags(findTown, "/plan/out")
			if err == nil {
				t.Fatalf("planRecordedFlags returned no error, want one naming %q", tc.wantText)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantText)
			}
			if !reflect.DeepEqual(flags, clusterRenderFlags{}) {
				t.Errorf("flags = %+v, want none returned on refusal", flags)
			}
			if gotRoot != "" {
				t.Errorf("town root = %q, want none returned on refusal", gotRoot)
			}
		})
	}
}

// TestClusterShapePlanFlagsForcesStaticAndOut: a recorded set comes back with
// the static set included and the plan's own --out, while every other flag and
// the town root come from the record and the town.
func TestClusterShapePlanFlagsForcesStaticAndOut(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	recorded := renderFlagsFixture()
	recorded.includeStatic = false
	if err := saveRenderFlags(renderFlagsPathIn(root), recorded); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}

	flags, gotRoot, err := planRecordedFlags(func() (string, error) { return root, nil }, "/plan/out")
	if err != nil {
		t.Fatalf("planRecordedFlags: %v", err)
	}
	if !flags.includeStatic {
		t.Error("includeStatic = false, want true: a plan always renders the static set")
	}
	if flags.out != "/plan/out" {
		t.Errorf("out = %q, want the plan's --out /plan/out, not the recorded %q", flags.out, recorded.out)
	}
	if flags.dir != recorded.dir {
		t.Errorf("dir = %q, want the recorded %q", flags.dir, recorded.dir)
	}
	if gotRoot != root {
		t.Errorf("town root = %q, want %q", gotRoot, root)
	}
}

// writeRecordedFlagsFile puts content where the render records its flags, so a
// test reads the file the production path reads.
func writeRecordedFlagsFile(t *testing.T, root, content string) {
	t.Helper()
	path := renderFlagsPathIn(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the flags directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the flags file: %v", err)
	}
}
