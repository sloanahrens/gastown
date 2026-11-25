package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// planTown is a temporary town holding the fixture's recorded render flags.
func planTown(t *testing.T) string {
	t.Helper()
	town := clusterRenderTestTown(t)
	if err := saveRenderFlags(renderFlagsPathIn(town), clusterRenderFixture(t)); err != nil {
		t.Fatalf("recording the render flags: %v", err)
	}
	return town
}

// planOut is a fresh plan directory under a temporary parent.
func planOut(t *testing.T) string { return filepath.Join(t.TempDir(), "plan") }

// TestClusterShapePlanIdPrintsTheShortId: the plan prints the 12-hex short form
// of planID over the files it rendered, in apply order, and nothing else.
func TestClusterShapePlanIdPrintsTheShortId(t *testing.T) {
	t.Parallel()
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(clusterRenderDepsAt(t, planTown(t), io.Discard), out, dir); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the plan directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names) // the render numbers its files in apply order
	files := make([]planFile, 0, len(names))
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading the planned file %s: %v", name, err)
		}
		files = append(files, planFile{Name: name, Data: body})
	}
	_, short := planID(files)
	if got := out.String(); got != short+"\n" {
		t.Fatalf("the plan printed %q, want the short id %q and a newline", got, short)
	}
}

// TestClusterShapePlanIdRefusesAndFailsClosed: the refusals the plan makes, the
// render's among them, each end non-zero and print no id, so an id on the screen
// always names a rendered set.
func TestClusterShapePlanIdRefusesAndFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (clusterRenderDeps, string)
		want  string
	}{
		{"the town cannot be found", func(t *testing.T) (clusterRenderDeps, string) {
			d := clusterRenderDepsAt(t, t.TempDir(), io.Discard)
			d.findTown = func() (string, error) { return "", errors.New("no workspace") }
			return d, planOut(t)
		}, "town not found"},
		{"the --out holds another apply set", func(t *testing.T) (clusterRenderDeps, string) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "taken"), nil, 0o644); err != nil {
				t.Fatalf("filling --out: %v", err)
			}
			return clusterRenderDepsAt(t, planTown(t), io.Discard), dir
		}, "is not empty"},
		{"a manifest cannot be written", func(t *testing.T) (clusterRenderDeps, string) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			d.writeFile = func(string, []byte) error { return errors.New("disk full") }
			return d, planOut(t)
		}, "disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, dir := tc.setup(t)
			out := &bytes.Buffer{}
			err := runTownClusterPlan(d, out, dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan(%q) = %v, want an error holding %q", dir, err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
		})
	}
}
