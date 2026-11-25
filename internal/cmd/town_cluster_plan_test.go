package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/update"
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
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), planKubectl(t), out, dir, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	_, short := planID(planFilesIn(t, dir))
	if got := out.String(); got != short+"\n" {
		t.Fatalf("the plan printed %q, want the short id %q and a newline", got, short)
	}
}

// planFilesIn reads the files of the plan in dir, in apply order. plan.json is
// left out: it records the plan, and the plan id does not name it.
func planFilesIn(t *testing.T, dir string) []planFile {
	t.Helper()
	entries, err := os.ReadDir(dir) // sorted by name, and the render numbers its files in apply order
	if err != nil {
		t.Fatalf("reading the plan directory: %v", err)
	}
	files := make([]planFile, 0, len(entries))
	for _, e := range entries {
		if e.Name() == planFileName {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading the planned file %s: %v", e.Name(), err)
		}
		files = append(files, planFile{Name: e.Name(), Data: body})
	}
	return files
}

// readPlanRecord decodes the plan.json in dir and returns it with its bytes.
func readPlanRecord(t *testing.T, dir string) (planRecord, []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, planFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", planFileName, err)
	}
	var rec planRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decoding %s: %v", planFileName, err)
	}
	return rec, body
}

// TestClusterShapePlanIdWritesTheFullIdAndPrintsTheShortOne: plan.json holds the
// full id and each file's name and SHA-256 in apply order, the output prints the
// 12-hex short form of that id, and the same inputs write the same plan.json.
func TestClusterShapePlanIdWritesTheFullIdAndPrintsTheShortOne(t *testing.T) {
	t.Parallel()
	town := planTown(t)
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), out, dir, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	files := planFilesIn(t, dir)
	full, _ := planID(files)
	rec, body := readPlanRecord(t, dir)
	if rec.ID != full || len(rec.ID) != 64 {
		t.Fatalf("plan.json id = %q, want the full 64-hex id %q", rec.ID, full)
	}
	if got := out.String(); got != rec.ID[:12]+"\n" {
		t.Fatalf("the plan printed %q, want the 12-hex short id %q and a newline", got, rec.ID[:12])
	}
	if len(rec.Files) != len(files) {
		t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
	}
	for i, f := range files {
		sum := sha256.Sum256(f.Data)
		want := planRecordFile{Name: f.Name, SHA256: hex.EncodeToString(sum[:]), Revision: planWantRevision(t, f)}
		if rec.Files[i] != want {
			t.Errorf("plan.json file %d = %+v, want %+v", i, rec.Files[i], want)
		}
	}
	again := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, again, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan again: %v", err)
	}
	if _, againBody := readPlanRecord(t, again); !bytes.Equal(body, againBody) {
		t.Fatalf("the same inputs wrote a different plan.json:\n%s\nwant:\n%s", againBody, body)
	}
}

// TestClusterShapePlanIdWritesNoPlanJSONOnARefusalOrRenderError: a refusal, a
// render error and a failed plan.json write each return an error, print no id
// and leave no plan.json, so the file's presence always means the plan is whole.
func TestClusterShapePlanIdWritesNoPlanJSONOnARefusalOrRenderError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (clusterRenderDeps, string, func(string, any) error)
		want  string
	}{
		{"the town cannot be found", func(t *testing.T) (clusterRenderDeps, string, func(string, any) error) {
			d := clusterRenderDepsAt(t, t.TempDir(), io.Discard)
			d.findTown = func() (string, error) { return "", errors.New("no workspace") }
			return d, planOut(t), atomicfile.WriteJSON
		}, "town not found"},
		{"the --out holds another apply set", func(t *testing.T) (clusterRenderDeps, string, func(string, any) error) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "taken"), nil, 0o644); err != nil {
				t.Fatalf("filling --out: %v", err)
			}
			return clusterRenderDepsAt(t, planTown(t), io.Discard), dir, atomicfile.WriteJSON
		}, "is not empty"},
		{"a file fails after another was written", func(t *testing.T) (clusterRenderDeps, string, func(string, any) error) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			writes := 0
			d.writeFile = func(path string, data []byte) error {
				writes++
				if writes == 2 {
					return errors.New("disk full")
				}
				return os.WriteFile(path, data, 0o644)
			}
			return d, planOut(t), atomicfile.WriteJSON
		}, "disk full"},
		{"the set cannot be built", func(t *testing.T) (clusterRenderDeps, string, func(string, any) error) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			d.build = func(k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				return k8sdaemon.Manifests{}, errors.New("planted build error")
			}
			return d, planOut(t), atomicfile.WriteJSON
		}, "planted build error"},
		{"plan.json cannot be written", func(t *testing.T) (clusterRenderDeps, string, func(string, any) error) {
			writeJSON := func(string, any) error { return errors.New("plan.json: disk full") }
			return clusterRenderDepsAt(t, planTown(t), io.Discard), planOut(t), writeJSON
		}, "disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, dir, writeJSON := tc.setup(t)
			out := &bytes.Buffer{}
			err := runTownClusterPlan(context.Background(), d, planKubectl(t), out, dir, writeJSON)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan(%q) = %v, want an error holding %q", dir, err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a failed plan printed %q, want nothing", out.String())
			}
			if _, err := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("plan.json exists or cannot be checked (%v), want none written", err)
			}
		})
	}
}

// TestClusterShapePlanIdMakesOnlyReadCalls: a runner that fails every helm call
// but `helm template`, the render's one read, never fails the plan, because the
// plan makes no other call. Every call the runner sees is a read.
func TestClusterShapePlanIdMakesOnlyReadCalls(t *testing.T) {
	t.Parallel()
	var calls []string
	d := clusterRenderDepsAt(t, planTown(t), io.Discard)
	d.runHelm = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if len(args) == 0 || args[0] != "template" {
			return nil, errors.New("planted non-read call: " + call)
		}
		return []byte(sbxEngineHelmOutput), nil
	}
	if err := runTownClusterPlan(context.Background(), d, planKubectl(t), &bytes.Buffer{}, planOut(t), atomicfile.WriteJSON); err != nil {
		t.Fatalf("the plan failed under a runner that fails every non-read call: %v", err)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "helm template ") {
			t.Errorf("the plan ran %q, want only the read helm template", c)
		}
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
			err := runTownClusterPlan(context.Background(), d, planKubectl(t), out, dir, atomicfile.WriteJSON)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan(%q) = %v, want an error holding %q", dir, err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
		})
	}
}

// townSnapshot maps every entry under root, by its path relative to root, to its
// kind and, for a regular file, the hex SHA-256 of its bytes. Two snapshots are
// equal only if nothing under root was created, removed or rewritten between them.
func townSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			snap[rel] = d.Type().String()
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		snap[rel] = "file " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting the town %s: %v", root, err)
	}
	return snap
}

// TestClusterShapePlanIdWritesNothingOutsideOut: a plan writes into --out and
// nothing else, so every entry and file hash under the town is the same after it,
// and the running values file the live render writes is not touched.
func TestClusterShapePlanIdWritesNothingOutsideOut(t *testing.T) {
	t.Parallel()
	town := planTown(t)
	before := townSnapshot(t, town)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, planOut(t), atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	if after := townSnapshot(t, town); !reflect.DeepEqual(after, before) {
		t.Fatalf("the plan changed the town outside --out:\nbefore %v\nafter  %v", before, after)
	}
}

// TestClusterShapePlanIdIncludesTheStaticSet: the recorded run left the static
// set out, and the plan still renders it, so one of the planned files is the
// PriorityClass document. The kind is read from each file's YAML, not its name.
func TestClusterShapePlanIdIncludesTheStaticSet(t *testing.T) {
	t.Parallel()
	town := clusterRenderTestTown(t)
	recorded := clusterRenderFixture(t)
	recorded.includeStatic = false
	if err := saveRenderFlags(renderFlagsPathIn(town), recorded); err != nil {
		t.Fatalf("recording the render flags: %v", err)
	}
	dir := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, dir, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	for _, f := range planFilesIn(t, dir) {
		var head struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal(f.Data, &head); err != nil {
			t.Fatalf("decoding the planned file %s: %v", f.Name, err)
		}
		if head.Kind == "PriorityClass" {
			return
		}
	}
	t.Fatalf("the plan wrote no PriorityClass document, so the static set is missing from it")
}

// TestClusterShapePlanIdRefusesAnEmptyOut: a plan given no --out is refused by
// the render's --out check before any write, so the error names the missing
// --out, no id is printed, and the town is as it was.
func TestClusterShapePlanIdRefusesAnEmptyOut(t *testing.T) {
	t.Parallel()
	town := planTown(t)
	before := townSnapshot(t, town)
	out := &bytes.Buffer{}
	err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), out, "", atomicfile.WriteJSON)
	if err == nil || !strings.Contains(err.Error(), "--out is required") {
		t.Fatalf("runTownClusterPlan with an empty --out = %v, want the --out is required refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused plan printed %q, want nothing", out.String())
	}
	if after := townSnapshot(t, town); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused plan changed the town:\nbefore %v\nafter  %v", before, after)
	}
}

// planLiveAnswer is the live Deployment kubectl prints, with revision annotated.
func planLiveAnswer(t *testing.T, resource, revision string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"name":        strings.TrimPrefix(resource, "deployment/"),
		"annotations": map[string]string{"deployment.kubernetes.io/revision": revision},
	}})
	if err != nil {
		t.Fatalf("encoding the live answer: %v", err)
	}
	return string(body)
}

// planKubectlAnswering is the plan's cluster read: each call must be the drift
// read's get of one Deployment, and answer gives its output and exit.
func planKubectlAnswering(t *testing.T, answer func(resource string) (string, int, error)) update.RunFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		get := len(args) == 7 && args[0] == "get" && strings.HasPrefix(args[1], "deployment/") && args[2] == "-n" && args[4] == "--ignore-not-found" && args[5] == "-o" && args[6] == "json"
		if name != "kubectl" || !get {
			t.Fatalf("the plan ran %s %q, want only a get of one Deployment", name, args)
		}
		return answer(args[1])
	}
}

// planKubectl answers each Deployment with the revision rev-<name>, which
// planWantRevision expects for the rendered Deployment.
func planKubectl(t *testing.T) update.RunFunc {
	t.Helper()
	return planKubectlAnswering(t, func(resource string) (string, int, error) {
		return planLiveAnswer(t, resource, "rev-"+strings.TrimPrefix(resource, "deployment/")), 0, nil
	})
}

// planWantRevision is the revision plan.json records for a rendered file: rev-<name>
// for a Deployment, and none for any other kind.
func planWantRevision(t *testing.T, f planFile) string {
	t.Helper()
	var head struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(f.Data, &head); err != nil {
		t.Fatalf("decoding the planned file %s: %v", f.Name, err)
	}
	if head.Kind != "Deployment" {
		return ""
	}
	return "rev-" + head.Metadata.Name
}

// TestClusterShapePlanLiveRevisions: plan.json records each rendered Deployment's
// revision as the cluster reads it, one get per Deployment and no other call.
func TestClusterShapePlanLiveRevisions(t *testing.T) {
	t.Parallel()
	var reads []string
	kube := planKubectlAnswering(t, func(resource string) (string, int, error) {
		reads = append(reads, resource)
		return planLiveAnswer(t, resource, "rev-"+strings.TrimPrefix(resource, "deployment/")), 0, nil
	})
	dir := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, &bytes.Buffer{}, dir, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	files := planFilesIn(t, dir)
	if len(rec.Files) != len(files) {
		t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
	}
	var deployments []string
	for i, f := range files {
		want := planWantRevision(t, f)
		if rec.Files[i].Revision != want {
			t.Errorf("plan.json revision for %s = %q, want %q", f.Name, rec.Files[i].Revision, want)
		}
		if want != "" {
			deployments = append(deployments, "deployment/"+strings.TrimPrefix(want, "rev-"))
		}
	}
	if len(deployments) == 0 {
		t.Fatal("the plan rendered no Deployment, so no revision was recorded")
	}
	if !reflect.DeepEqual(reads, deployments) {
		t.Fatalf("the plan read %v from the cluster, want one get per rendered Deployment %v", reads, deployments)
	}
}

// TestClusterShapePlanLiveRefusesAndWritesNoPlan: each unreadable, absent or
// revisionless Deployment refuses the plan, which prints no id and writes no plan.json.
func TestClusterShapePlanLiveRefusesAndWritesNoPlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		answer func(resource string) (string, int, error)
		want   string
	}{
		{"the live read exits nonzero", func(string) (string, int, error) { return "", 1, nil }, "could not be read"},
		{"the cluster does not hold it", func(string) (string, int, error) { return "", 0, nil }, "does not hold"},
		{"the live object has no revision", func(string) (string, int, error) { return `{"metadata":{}}`, 0, nil }, "has no revision annotation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := planOut(t)
			out := &bytes.Buffer{}
			kube := planKubectlAnswering(t, tc.answer)
			err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, atomicfile.WriteJSON)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan = %v, want an error holding %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
			if _, err := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("plan.json exists or cannot be checked (%v), want none written", err)
			}
		})
	}
	t.Run("a rendered file has no readable kind", func(t *testing.T) {
		t.Parallel()
		_, err := planLiveRevision(context.Background(), planKubectl(t), []byte("kind: ["))
		if err == nil || !strings.Contains(err.Error(), "cannot read a rendered file's kind") {
			t.Fatalf("planLiveRevision = %v, want the unreadable-kind refusal", err)
		}
	})
}

// TestClusterShapePlanLongText: the command's help says the plan reads the
// cluster's Deployments with get only and writes nothing to any cluster, and no
// longer says that no cluster is contacted.
func TestClusterShapePlanLongText(t *testing.T) {
	t.Parallel()
	long := strings.Join(strings.Fields(townClusterPlanCmd.Long), " ")
	if !strings.Contains(long, "reads the cluster's Deployments with get only and writes nothing to any cluster") {
		t.Errorf("Long text = %q, want it to say the plan reads the cluster's Deployments with get only and writes nothing to any cluster", long)
	}
	if strings.Contains(long, "no cluster is contacted") {
		t.Errorf("Long text = %q, still says no cluster is contacted", long)
	}
}
