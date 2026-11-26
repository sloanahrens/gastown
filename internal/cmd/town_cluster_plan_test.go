package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
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

// planTestKubeconfigs are the two flags a plan holding both phases takes.
var planTestKubeconfigs = planFlags{policyKubeconfig: "/kube/policy.yaml", applyKubeconfig: "/kube/apply.yaml"}

// TestClusterShapePlanKubeconfig: phase P reads --policy-kubeconfig and phase W
// --apply-kubeconfig, no other phase has a kubeconfig, and a phase whose flag is
// missing is refused naming it, since the plan takes no other cluster identity.
func TestClusterShapePlanKubeconfig(t *testing.T) {
	t.Parallel()
	const policy, apply = "/kube/policy.yaml", "/kube/apply.yaml"
	cases := []struct {
		phase   string
		f       planFlags
		want    string
		wantErr string
	}{
		{planPhaseStatic, planTestKubeconfigs, policy, ""},
		{planPhaseWorkloads, planTestKubeconfigs, apply, ""},
		{"X", planTestKubeconfigs, "", "is not a plan phase"},
		{planPhaseStatic, planFlags{applyKubeconfig: apply}, "", "--policy-kubeconfig"},
		{planPhaseWorkloads, planFlags{policyKubeconfig: policy}, "", "--apply-kubeconfig"},
	}
	for _, tc := range cases {
		got, err := phaseKubeconfig(tc.phase, tc.f)
		if tc.wantErr == "" {
			if err != nil || got != tc.want {
				t.Errorf("phaseKubeconfig(%q) = %q, %v; want %q", tc.phase, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
			t.Errorf("phaseKubeconfig(%q) = %q, %v; want a refusal naming %q and no kubeconfig", tc.phase, got, err, tc.wantErr)
		}
	}
}

// TestClusterShapePlanKubeconfigNeedsTheFlagOfEachPhaseWithFiles: a plan with phase
// P refuses without --policy-kubeconfig, one with W without --apply-kubeconfig, each
// naming the flag and writing no plan.json; dropping phase P needs none.
func TestClusterShapePlanKubeconfigNeedsTheFlagOfEachPhaseWithFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		flags   planFlags
		drifted bool
		wantErr string
	}{
		{planFlags{}, true, "--policy-kubeconfig"},
		{planFlags{policyKubeconfig: "/kube/policy.yaml"}, true, "--apply-kubeconfig"},
		{planFlags{applyKubeconfig: "/kube/apply.yaml"}, false, ""},
	}
	for _, tc := range cases {
		dir := planOut(t)
		static := planStaticLive(t)
		if tc.drifted {
			static = planStaticLiveOneDrifted(t)
		}
		kube := planKubectlReading(t, static, planRevisionAnswer(t))
		err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, &bytes.Buffer{}, dir, tc.flags, atomicfile.WriteJSON)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("flags %+v: running the plan: %v", tc.flags, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("flags %+v: runTownClusterPlan = %v, want an error naming %q", tc.flags, err, tc.wantErr)
		} else if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("flags %+v: plan.json exists (stat error %v) after a refusal, want none written", tc.flags, statErr)
		}
	}
}

// TestClusterShapePlanKubeconfigHelpSaysItDoesNotRunInTheDaemonPod: the help text
// says, beside the per-phase flags, that the command does not run inside the
// daemon pod, so an operator does not expect the plan to run there.
func TestClusterShapePlanKubeconfigHelpSaysItDoesNotRunInTheDaemonPod(t *testing.T) {
	t.Parallel()
	long := strings.Join(strings.Fields(townClusterPlanCmd.Long), " ")
	if !strings.Contains(long, "The command does not run inside the daemon pod.") {
		t.Errorf("Long text = %q, want it to say the command does not run inside the daemon pod", long)
	}
}

// TestClusterShapePlanIdPrintsTheShortId: the plan prints the phases line, then
// the 12-hex short form of planID over the files it rendered, as the last line.
func TestClusterShapePlanIdPrintsTheShortId(t *testing.T) {
	t.Parallel()
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), planKubectl(t), out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	_, short := planID(planFilesIn(t, dir))
	if got := out.String(); !strings.HasSuffix(got, "\n"+short+"\n") {
		t.Fatalf("the plan printed %q, want the short id %q as its last line", got, short)
	}
}

// planStaticDocs is the bytes of each embedded static document: the bytes the
// render writes for the static set, so a rendered file is phase P exactly when
// its bytes are one of them.
func planStaticDocs(t *testing.T) map[string]bool {
	t.Helper()
	static, err := k8s.Static()
	if err != nil {
		t.Fatalf("reading the embedded static set: %v", err)
	}
	docs := make(map[string]bool, len(static))
	for _, object := range static {
		docs[string(object.Doc)] = true
	}
	return docs
}

// planWantPhase is the phase plan.json records for a rendered file: P for an
// embedded static document, W for every other file.
func planWantPhase(static map[string]bool, f planFile) string {
	if static[string(f.Data)] {
		return planPhaseStatic
	}
	return planPhaseWorkloads
}

// TestClusterShapePlanPhases: plan.json gives every file its phase, P for the
// static set and W for the workloads; each embedded static document is one P
// file, and no Deployment is P. The output names P then W, in that order, then
// the one static difference, then the plan id. One static object differs
// (planStaticLiveOneDrifted), so both phases stay, in plan.json, in the id and in
// the output.
func TestClusterShapePlanPhases(t *testing.T) {
	t.Parallel()
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), planKubectlRendered(t, dir, planStaticLiveOneDrifted(t), nil), out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	files := planFilesIn(t, dir)
	if len(rec.Files) != len(files) {
		t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
	}
	static := planStaticDocs(t)
	counts := map[string]int{}
	for i, f := range files {
		want := planWantPhase(static, f)
		if rec.Files[i].Phase != want {
			t.Errorf("plan.json phase for %s = %q, want %q", f.Name, rec.Files[i].Phase, want)
		}
		if planWantDeployment(t, f) != "" && rec.Files[i].Phase != planPhaseWorkloads {
			t.Errorf("plan.json phase for the Deployment %s = %q, want %q", f.Name, rec.Files[i].Phase, planPhaseWorkloads)
		}
		counts[rec.Files[i].Phase]++
	}
	if counts[planPhaseStatic] != len(static) {
		t.Fatalf("plan.json holds %d phase P files, want the %d embedded static documents", counts[planPhaseStatic], len(static))
	}
	if counts[planPhaseWorkloads] == 0 {
		t.Fatal("plan.json holds no phase W file, so the workloads are missing from the plan")
	}
	_, short := planID(files)
	want := fmt.Sprintf("Phases in apply order: P (static, %d files), then W (workloads, %d files)\n%s\n%s\n",
		counts[planPhaseStatic], counts[planPhaseWorkloads], planPriorityClassLine(), short)
	if got := out.String(); got != want {
		t.Fatalf("the plan printed %q, want the phases in order P then W, the static difference and the short id:\n%s", got, want)
	}
}

// TestClusterShapePlanPhasesPOmitted: when no static object differs from the
// cluster's, plan.json lists no phase P file, though the render wrote the static
// documents beside it. The id and the short id printed cover exactly the listed W
// files in apply order, and the output names only W.
func TestClusterShapePlanPhasesPOmitted(t *testing.T) {
	t.Parallel()
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), planKubectlRendered(t, dir, planStaticLive(t), nil), out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	for _, rf := range rec.Files {
		if rf.Phase != planPhaseWorkloads {
			t.Errorf("plan.json lists %s as phase %q, want no phase P file when no static object differs", rf.Name, rf.Phase)
		}
	}
	listed := planListedFiles(t, dir)
	static := planStaticDocs(t)
	var workloads []planFile
	for _, f := range planFilesIn(t, dir) {
		if planWantPhase(static, f) == planPhaseWorkloads {
			workloads = append(workloads, f)
		}
	}
	if written := len(planFilesIn(t, dir)) - len(workloads); written != len(static) {
		t.Fatalf("the render wrote %d static files, want the %d embedded static documents it must still write", written, len(static))
	}
	if len(listed) != len(workloads) {
		t.Fatalf("plan.json lists %d files, want the %d workload files the render wrote", len(listed), len(workloads))
	}
	for i := range listed {
		if listed[i].Name != workloads[i].Name {
			t.Fatalf("plan.json file %d = %s, want %s in apply order", i, listed[i].Name, workloads[i].Name)
		}
	}
	full, short := planID(listed)
	if rec.ID != full {
		t.Fatalf("plan.json id = %q, want the id of the listed files %q", rec.ID, full)
	}
	want := fmt.Sprintf("Phases in apply order: W (workloads, %d files)\n%s\n", len(listed), short)
	if got := out.String(); got != want {
		t.Fatalf("the plan printed %q, want only the W phase and the short id:\n%s", got, want)
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

// planListedFiles reads the files plan.json lists in dir, in the order it lists
// them: the files the plan id names. A phase P file the plan omits is on disk
// beside them but not listed.
func planListedFiles(t *testing.T, dir string) []planFile {
	t.Helper()
	rec, _ := readPlanRecord(t, dir)
	files := make([]planFile, 0, len(rec.Files))
	for _, rf := range rec.Files {
		body, err := os.ReadFile(filepath.Join(dir, rf.Name))
		if err != nil {
			t.Fatalf("reading the listed file %s: %v", rf.Name, err)
		}
		files = append(files, planFile{Name: rf.Name, Data: body})
	}
	return files
}

// TestClusterShapePlanIdWritesTheFullIdAndPrintsTheShortOne: plan.json holds the
// full id and each file's name and SHA-256 in apply order, the output prints the
// 12-hex short form of that id, and the same inputs write the same plan.json.
func TestClusterShapePlanIdWritesTheFullIdAndPrintsTheShortOne(t *testing.T) {
	t.Parallel()
	town := planTown(t)
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	files := planFilesIn(t, dir)
	full, _ := planID(files)
	rec, body := readPlanRecord(t, dir)
	if rec.ID != full || len(rec.ID) != 64 {
		t.Fatalf("plan.json id = %q, want the full 64-hex id %q", rec.ID, full)
	}
	if got := out.String(); !strings.HasSuffix(got, "\n"+rec.ID[:12]+"\n") {
		t.Fatalf("the plan printed %q, want the 12-hex short id %q as its last line", got, rec.ID[:12])
	}
	if len(rec.Files) != len(files) {
		t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
	}
	static := planStaticDocs(t)
	for i, f := range files {
		sum := sha256.Sum256(f.Data)
		want := planRecordFile{Name: f.Name, Phase: planWantPhase(static, f), SHA256: hex.EncodeToString(sum[:]), Revision: planWantRevision(t, f), ResourceVersion: planWantResourceVersion(t, f)}
		if rec.Files[i] != want {
			t.Errorf("plan.json file %d = %+v, want %+v", i, rec.Files[i], want)
		}
	}
	again := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, again, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
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
			err := runTownClusterPlan(context.Background(), d, planKubectl(t), out, dir, planTestKubeconfigs, writeJSON)
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
	if err := runTownClusterPlan(context.Background(), d, planKubectl(t), &bytes.Buffer{}, planOut(t), planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
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
			err := runTownClusterPlan(context.Background(), d, planKubectl(t), out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
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
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, planOut(t), planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
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
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), &bytes.Buffer{}, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
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
	err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, town, io.Discard), planKubectl(t), out, "", planTestKubeconfigs, atomicfile.WriteJSON)
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

// TestClusterShapePlanStaticRefuses: a static object the plan cannot build into a
// target refuses the plan, which prints no id and writes no plan.json.
func TestClusterShapePlanStaticRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (clusterRenderDeps, update.RunFunc)
		want  string
	}{
		{"a static document that names another object", func(t *testing.T) (clusterRenderDeps, update.RunFunc) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			build := d.build
			d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				m, err := build(v)
				if err != nil {
					return m, err
				}
				for i := range m.Static {
					if m.Static[i].Kind == namespaceKind {
						m.Static[i].Name = "planted"
						break
					}
				}
				return m, nil
			}
			return d, planKubectl(t)
		}, "holds a document named"},
		{"a Secret document that names another object", func(t *testing.T) (clusterRenderDeps, update.RunFunc) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			build := d.build
			d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				m, err := build(v)
				if err != nil {
					return m, err
				}
				for i := range m.Static {
					if m.Static[i].Kind == secretKind {
						m.Static[i].Name = "planted"
						break
					}
				}
				return m, nil
			}
			return d, planKubectl(t)
		}, "holds a document named"},
		{"a static Secret that carries data", func(t *testing.T) (clusterRenderDeps, update.RunFunc) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			build := d.build
			d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				m, err := build(v)
				if err != nil {
					return m, err
				}
				for i := range m.Static {
					if m.Static[i].Kind == secretKind {
						m.Static[i].SecretNoData = false
						break
					}
				}
				return m, nil
			}
			// Any cluster read fails, so a plan that read the Secret before
			// refusing would report the read's cause instead of the guard's.
			return d, func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 1, nil
			}
		}, "carries data and was not read"},
		{"a static set that holds one object twice", func(t *testing.T) (clusterRenderDeps, update.RunFunc) {
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			build := d.build
			d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				m, err := build(v)
				if err != nil {
					return m, err
				}
				m.Static = append(m.Static, m.Static[0])
				return m, nil
			}
			// Any cluster read fails, so a plan that read the objects before
			// refusing would report the read's cause instead of the guard's.
			return d, func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 1, nil
			}
		}, "compared twice"},
		{"a static object the cluster cannot be read", func(t *testing.T) (clusterRenderDeps, update.RunFunc) {
			kube := planKubectl(t)
			return clusterRenderDepsAt(t, planTown(t), io.Discard), func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				if resource, _, ok := planGetOf(name, args); ok && !strings.HasPrefix(resource, "deployment/") {
					return "", 1, nil
				}
				return kube(ctx, dir, env, name, args...)
			}
		}, "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, kube := tc.setup(t)
			dir, out := planOut(t), &bytes.Buffer{}
			err := runTownClusterPlan(context.Background(), d, kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan = %v, want an error holding %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
			if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("plan.json exists or cannot be checked (%v), want none written", statErr)
			}
		})
	}
}

// TestClusterShapePlanStaticGetsOnce: with every static object equal to the
// cluster's, the plan makes one get of each static object, and prints the short id.
func TestClusterShapePlanStaticGetsOnce(t *testing.T) {
	t.Parallel()
	want := map[string]int{}
	for key := range planStaticLive(t) {
		want[key] = 1
	}
	got := map[string]int{}
	kube := planKubectlStaticEqual(t)
	counting := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if resource, namespace, ok := planGetOf(name, args); ok && !strings.HasPrefix(resource, "deployment/") {
			got[planStaticResourceKey(resource, namespace)]++
		}
		return kube(ctx, dir, env, name, args...)
	}
	dir, out := planOut(t), &bytes.Buffer{}
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), counting, out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan's static gets = %v, want %v", got, want)
	}
	_, short := planID(planListedFiles(t, dir))
	if printed := out.String(); !strings.HasSuffix(printed, "\n"+short+"\n") {
		t.Fatalf("the plan printed %q, want the short id %q last", printed, short)
	}
}

// planLiveAnswer is the live Deployment kubectl prints, with revision annotated
// and resourceVersion set to rv-<revision>, which planWantResourceVersion expects.
func planLiveAnswer(t *testing.T, resource, revision string) string {
	t.Helper()
	name := strings.TrimPrefix(resource, "deployment/")
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"name":            name,
		"resourceVersion": "rv-" + revision,
		"annotations":     map[string]string{"deployment.kubernetes.io/revision": revision},
	}})
	if err != nil {
		t.Fatalf("encoding the live answer: %v", err)
	}
	return string(body)
}

// planStaticKey is how the fake cluster keys the answer to a static object's get:
// its resource, kind in lower case then /name, and its namespace, empty for a
// cluster-scoped kind.
func planStaticKey(kind, name, namespace string) string {
	return planStaticResourceKey(strings.ToLower(kind)+"/"+name, namespace)
}

// planStaticResourceKey is a static read's resource, kind/name as the drift read
// names it, with its namespace, empty for a cluster-scoped kind.
func planStaticResourceKey(resource, namespace string) string {
	return resource + " " + namespace
}

// planObjectField is doc's field as an object, failing the test when the document
// holds something else there.
func planObjectField(t planFailer, doc map[string]any, key string) map[string]any {
	t.Helper()
	field, ok := doc[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", key, doc[key])
		return nil
	}
	return field
}

// planStaticLiveOneDrifted is planStaticLive with the live value of the gt-ci
// PriorityClass changed from its rendered 100, so exactly one static object differs.
func planStaticLiveOneDrifted(t *testing.T) map[string]string {
	t.Helper()
	live := planStaticLive(t)
	key := planStaticKey(priorityClassKind, "gt-ci", "")
	var doc map[string]any
	if err := json.Unmarshal([]byte(live[key]), &doc); err != nil {
		t.Fatalf("decoding the live %s: %v", key, err)
	}
	doc["value"] = 101
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding the live %s: %v", key, err)
	}
	live[key] = string(body)
	return live
}

// planKubectlAnswering is the plan's cluster read: a get of one Deployment gives
// answer's output and exit, and a get of one static object gives its document as
// planStaticLiveOneDrifted holds it, so one static object differs and the plan
// keeps phase P.
func planKubectlAnswering(t *testing.T, answer func(resource string) (string, int, error)) update.RunFunc {
	t.Helper()
	return planKubectlReading(t, planStaticLiveOneDrifted(t), answer)
}

// planFailer is the part of *testing.T the shared plan fake refuses a non-get
// call through, so a row can drive that refusal with a recording reporter.
type planFailer interface {
	Helper()
	Fatalf(format string, args ...any)
}

// planKubectlReading is the plan's cluster read with static as the answer to each
// static object's get, keyed as planStaticLive keys it.
func planKubectlReading(t planFailer, static map[string]string, answer func(resource string) (string, int, error)) update.RunFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		resource, namespace, ok := planGetOf(name, args)
		if !ok {
			t.Fatalf("the plan ran %s %q, want only a get", name, args)
			return "", 0, nil // a recording reporter returns where *testing.T does not
		}
		if strings.HasPrefix(resource, "deployment/") {
			return answer(resource)
		}
		return static[planStaticResourceKey(resource, namespace)], 0, nil
	}
}

// planGetOf reads a call as the drift read's get: its resource and namespace, empty
// when cluster-scoped. ok is false for any other call, which the fake fails on.
func planGetOf(name string, args []string) (resource, namespace string, ok bool) {
	if name != "kubectl" || len(args) < 2 || args[0] != "get" {
		return "", "", false
	}
	cluster, known := planKindScope(args[1])
	switch {
	case known && cluster && len(args) == 5 && args[2] == "--ignore-not-found" && args[3] == "-o" && args[4] == "json":
		return args[1], "", true
	case known && !cluster && len(args) == 7 && args[2] == "-n" && args[4] == "--ignore-not-found" && args[5] == "-o" && args[6] == "json":
		return args[1], args[3], true
	}
	return "", "", false
}

// planKindScope is whether a kind/name resource is cluster-scoped, and whether its kind is one the reads name.
func planKindScope(resource string) (cluster, known bool) {
	kind, _, named := strings.Cut(resource, "/")
	// A Secret is namespaced, and the plan reads it although the drift compare's
	// static kinds do not name it.
	if named && kind == strings.ToLower(secretKind) {
		return false, true
	}
	for k, scoped := range staticTargetKinds {
		if named && strings.ToLower(k) == kind {
			return scoped, true
		}
	}
	// A Deployment is the drift read's other get, besides the static objects: it
	// is namespaced, and the fake answers it from its own answer, not the static set.
	return false, named && kind == "deployment"
}

// TestClusterShapePlanStaticDiffersKeepsPhasePFakeShape: the fake fails any call but the drift read's get.
func TestClusterShapePlanStaticDiffersKeepsPhasePFakeShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		args      []string
		ok        bool
		namespace string
	}{
		{"a cluster-scoped get", []string{"get", "priorityclass/gt-ci", "--ignore-not-found", "-o", "json"}, true, ""},
		{"a namespaced get", []string{"get", "role/gt-update", "-n", "gt", "--ignore-not-found", "-o", "json"}, true, "gt"},
		{"a namespaced deployment get", []string{"get", "deployment/daemon", "-n", "gt", "--ignore-not-found", "-o", "json"}, true, "gt"},
		{"a kind the compare does not read", []string{"get", "pod/x", "-n", "gt", "--ignore-not-found", "-o", "json"}, false, ""},
		{"a namespaced get with --namespace", []string{"get", "role/gt-update", "--namespace", "gt", "--ignore-not-found", "-o", "json"}, false, ""},
		{"a namespaced get with no namespace", []string{"get", "role/gt-update", "--ignore-not-found", "-o", "json"}, false, ""},
		{"a cluster-scoped get with a namespace", []string{"get", "priorityclass/gt-ci", "-n", "gt", "--ignore-not-found", "-o", "json"}, false, ""},
		{"a get with yaml output", []string{"get", "priorityclass/gt-ci", "--ignore-not-found", "-o", "yaml"}, false, ""},
	}
	for _, tc := range cases {
		_, namespace, ok := planGetOf("kubectl", tc.args)
		if ok != tc.ok || namespace != tc.namespace {
			t.Errorf("%s: planGetOf = namespace %q, ok %v; want namespace %q, ok %v", tc.name, namespace, ok, tc.namespace, tc.ok)
		}
	}
}

// planFatalRecorder records the fake's refusal instead of ending the test.
type planFatalRecorder struct{ msg string }

func (*planFatalRecorder) Helper() {}

func (r *planFatalRecorder) Fatalf(format string, args ...any) { r.msg = fmt.Sprintf(format, args...) }

// TestClusterShapePlanStaticDiffersKeepsPhasePFakeRefusesNonGet: the shared fake
// fails a call that is not the drift read's get, so a plan reaching the cluster
// any other way cannot pass the tests built on it.
func TestClusterShapePlanStaticDiffersKeepsPhasePFakeRefusesNonGet(t *testing.T) {
	t.Parallel()
	rec := &planFatalRecorder{}
	kube := planKubectlReading(rec, planStaticLive(t), planRevisionAnswer(t))
	out, code, err := kube(context.Background(), "", nil, "kubectl", "apply", "-f", "-")
	if want := `the plan ran kubectl ["apply" "-f" "-"], want only a get`; rec.msg != want {
		t.Fatalf("the fake refused with %q, want %q", rec.msg, want)
	}
	if out != "" || code != 0 || err != nil {
		t.Fatalf("a refused call answered %q, exit %d, err %v; want no answer", out, code, err)
	}
}

// TestClusterShapePlanStaticDiffersKeepsPhasePObjectField: a row that changes a
// live object fails the test, not panics, when the field it changes is no object.
func TestClusterShapePlanStaticDiffersKeepsPhasePObjectField(t *testing.T) {
	t.Parallel()
	rec := &planFatalRecorder{}
	if got, want := planObjectField(rec, map[string]any{"metadata": "planted"}, "metadata"), `"metadata" is string, want an object`; got != nil || rec.msg != want {
		t.Fatalf("a string field = %v, refused with %q; want nil and %q", got, rec.msg, want)
	}
	rec.msg = ""
	if got, want := planObjectField(rec, map[string]any{}, "metadata"), `"metadata" is <nil>, want an object`; got != nil || rec.msg != want {
		t.Fatalf("a missing field = %v, refused with %q; want nil and %q", got, rec.msg, want)
	}
}

// TestClusterShapePlanStaticDiffersKeepsPhaseP: a static object that differs from the
// cluster's, of any kind the compare reads, or one it does not hold, keeps phase P.
func TestClusterShapePlanStaticDiffersKeepsPhaseP(t *testing.T) {
	t.Parallel()
	plantedRules := func(_ *testing.T, doc map[string]any) {
		doc["rules"] = []any{map[string]any{"verbs": []any{"planted"}}}
	}
	cases := []struct {
		name, kind, object, namespace string
		change                        func(t *testing.T, doc map[string]any) // nil: the cluster does not hold the object
	}{
		{"a PriorityClass value", priorityClassKind, "gt-ci", "", func(_ *testing.T, doc map[string]any) { doc["value"] = 101 }},
		{"a Namespace name", namespaceKind, "gt-sweeps", "", func(t *testing.T, doc map[string]any) { planObjectField(t, doc, "metadata")["name"] = "planted" }},
		{"a Role's rules", roleKind, "gt-update", "gt", plantedRules},
		{"a ClusterRole's rules", clusterRoleKind, "gt-drift-reader", "", plantedRules},
		{"a ValidatingAdmissionPolicy's spec", validatingPolicyKind, "gt-update-jobs", "", func(t *testing.T, doc map[string]any) { planObjectField(t, doc, "spec")["failurePolicy"] = "Ignore" }},
		{"a ServiceAccount's name", serviceAccountKind, "gt-update", "gt", func(t *testing.T, doc map[string]any) { planObjectField(t, doc, "metadata")["name"] = "planted" }},
		{"a Secret's type", secretKind, "sweep-runner-token", "gt-sweeps", func(_ *testing.T, doc map[string]any) { doc["type"] = "Opaque" }},
		{"a Secret's declared annotation", secretKind, "sweep-runner-token", "gt-sweeps", func(t *testing.T, doc map[string]any) {
			planObjectField(t, planObjectField(t, doc, "metadata"), "annotations")["kubernetes.io/service-account.uid"] = "planted"
		}},
		{"a Secret the cluster does not hold", secretKind, "sweep-runner-token", "gt-sweeps", nil},
		{"a PriorityClass the cluster does not hold", priorityClassKind, "gt-ci", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := planStaticKey(tc.kind, tc.object, tc.namespace)
			live := planStaticLive(t)
			if tc.change == nil {
				delete(live, key)
			} else {
				var doc map[string]any
				if err := json.Unmarshal([]byte(live[key]), &doc); err != nil {
					t.Fatalf("decoding the live %s: %v", key, err)
				}
				tc.change(t, doc)
				body, err := json.Marshal(doc)
				if err != nil {
					t.Fatalf("encoding the live %s: %v", key, err)
				}
				live[key] = string(body)
			}
			dir, out := planOut(t), &bytes.Buffer{}
			kube := planKubectlRendered(t, dir, live, nil)
			if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
				t.Fatalf("running the plan: %v", err)
			}
			rec, _ := readPlanRecord(t, dir)
			files := planFilesIn(t, dir)
			if len(rec.Files) != len(files) {
				t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
			}
			static, counts := planStaticDocs(t), map[string]int{}
			for i, f := range files {
				want := planWantPhase(static, f)
				counts[want]++
				if rec.Files[i].Name != f.Name || rec.Files[i].Phase != want {
					t.Errorf("plan.json file %d = %s in phase %q, want %s in phase %q", i, rec.Files[i].Name, rec.Files[i].Phase, f.Name, want)
				}
			}
			_, short := planID(files)
			got, resource := out.String(), strings.ToLower(tc.kind)+"/"+tc.object
			if phases := fmt.Sprintf("Phases in apply order: P (static, %d files), then W (workloads, %d files)\n", counts[planPhaseStatic], counts[planPhaseWorkloads]); !strings.HasPrefix(got, phases) || !strings.HasSuffix(got, "\n"+short+"\n") || !strings.Contains(got, resource+": ") {
				t.Fatalf("the plan printed %q, want both phases, the compare's line for the changed %s, then the short id %q", got, resource, short)
			}
		})
	}
}

// TestClusterShapePlanDiff: the plan prints the drift compare's line for a
// rendered Deployment field the live object carries differently, after the phases
// and before the short id. spec.replicas is on the compare's reviewed print list,
// so the line carries each side's value.
func TestClusterShapePlanDiff(t *testing.T) {
	t.Parallel()
	dir, out := planOut(t), &bytes.Buffer{}
	kube := planKubectlRendered(t, dir, planStaticLive(t), func(t *testing.T, doc map[string]any) {
		planObjectField(t, doc, "spec")["replicas"] = 3
	})
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	_, short := planID(planListedFiles(t, dir))
	want := fmt.Sprintf("deployment/%s: spec.replicas: rendered 1, observed 3", k8sdaemon.Name)
	if got := out.String(); !strings.HasPrefix(got, "Phases in apply order: ") || !strings.HasSuffix(got, "\n"+short+"\n") || !strings.Contains(got, "\n"+want+"\n") {
		t.Fatalf("the plan printed %q, want the phases line, then the compare's line %q, then the short id %q", got, want, short)
	}
}

// TestClusterShapePlanDiffRefuses: a get that fails for a Deployment or for a
// static object refuses the plan, naming the object it could not read, printing
// nothing and writing no plan.json.
func TestClusterShapePlanDiffRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fails func(resource string) bool
	}{
		{"a Deployment's read fails", func(resource string) bool { return strings.HasPrefix(resource, "deployment/") }},
		{"a static object's read fails", func(resource string) bool { return !strings.HasPrefix(resource, "deployment/") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, out, failed := planOut(t), &bytes.Buffer{}, ""
			rendered := planKubectlRendered(t, dir, planStaticLive(t), nil)
			kube := func(ctx context.Context, name string, env []string, binary string, args ...string) (string, int, error) {
				if resource, _, ok := planGetOf(binary, args); ok && tc.fails(resource) {
					if failed == "" {
						failed = resource
					}
					return "", 1, nil
				}
				return rendered(ctx, name, env, binary, args...)
			}
			err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
			if err == nil || failed == "" || !strings.Contains(err.Error(), failed) {
				t.Fatalf("runTownClusterPlan = %v, want an error naming the unread %s", err, failed)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
			if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("plan.json exists or cannot be checked (%v), want none written", statErr)
			}
		})
	}
}

// TestClusterShapePlanSecretDataIsNotCompared: a Secret whose document carries
// data the cluster does not hold, and whose every other declared field is in
// step, does not differ, so the plan lists no phase P file. The embedded Secret
// carries no data, so the row plants it to exercise the strip. The Secret is
// still read once, so its declared fields were compared and its data was not.
func TestClusterShapePlanSecretDataIsNotCompared(t *testing.T) {
	t.Parallel()
	key := planStaticKey(secretKind, "sweep-runner-token", "gt-sweeps")
	reads := 0
	kube := planKubectlReading(t, planStaticLive(t), planRevisionAnswer(t))
	counting := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if resource, namespace, ok := planGetOf(name, args); ok && planStaticResourceKey(resource, namespace) == key {
			reads++
		}
		return kube(ctx, dir, env, name, args...)
	}
	d := clusterRenderDepsAt(t, planTown(t), io.Discard)
	build := d.build
	d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
		m, err := build(v)
		if err != nil {
			return m, err
		}
		for i := range m.Static {
			if m.Static[i].Kind == secretKind {
				m.Static[i].Doc = append(append([]byte{}, m.Static[i].Doc...), "\ndata:\n  token: ZGVjbGFyZWQ=\n"...)
			}
		}
		return m, nil
	}
	dir := planOut(t)
	if err := runTownClusterPlan(context.Background(), d, counting, &bytes.Buffer{}, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	if reads != 1 {
		t.Fatalf("the plan read the Secret %d times, want its declared fields compared once", reads)
	}
	rec, _ := readPlanRecord(t, dir)
	for _, rf := range rec.Files {
		if rf.Phase != planPhaseWorkloads {
			t.Errorf("plan.json lists %s as phase %q, want no phase P file when only the Secret's data differs", rf.Name, rf.Phase)
		}
	}
}

// planStaticLive is the answer the fake cluster gives each static object, keyed by
// resource and namespace: the object's document as JSON, every declared field in
// step with the render. The Secret is answered too, as its document alone.
func planStaticLive(t *testing.T) map[string]string {
	t.Helper()
	static, err := k8s.Static()
	if err != nil {
		t.Fatalf("reading the embedded static set: %v", err)
	}
	live := map[string]string{}
	for _, object := range static {
		var doc map[string]any
		if err := yaml.Unmarshal(object.Doc, &doc); err != nil {
			t.Fatalf("decoding the static %s: %v", object.Name, err)
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("encoding the static %s: %v", object.Name, err)
		}
		live[planStaticKey(object.Kind, object.Name, object.Namespace)] = string(body)
	}
	return live
}

// planRevisionOf is the revision the fake cluster holds for a Deployment: a positive
// whole number, as the controller stamps, with a value of its own for each name.
func planRevisionOf(name string) string {
	return strconv.Itoa(int(crc32.ChecksumIEEE([]byte(name))%1000) + 1)
}

// planKubectl answers each Deployment with the revision planRevisionOf gives its
// name, which planWantRevision expects for the rendered Deployment. One static
// object differs, so the plan keeps phase P.
func planKubectl(t *testing.T) update.RunFunc {
	t.Helper()
	return planKubectlAnswering(t, planRevisionAnswer(t))
}

// planKubectlStaticEqual is planKubectl over a cluster whose static objects all
// equal the rendered set, so no static object differs and the plan omits phase P.
func planKubectlStaticEqual(t *testing.T) update.RunFunc {
	t.Helper()
	return planKubectlReading(t, planStaticLive(t), planRevisionAnswer(t))
}

// planKubectlRendered answers each Deployment's read with the Deployment the plan
// rendered into dir, as the cluster reports it: the rendered fields, the revision
// planRevisionOf stamps and rv-<revision>, so the compare finds nothing. Each
// static object's read is answered from static, and mutate changes what a row
// needs to differ.
func planKubectlRendered(t *testing.T, dir string, static map[string]string, mutate func(t *testing.T, doc map[string]any)) update.RunFunc {
	t.Helper()
	return planKubectlReading(t, static, func(resource string) (string, int, error) {
		for _, f := range planFilesIn(t, dir) {
			if planWantDeployment(t, f) != resource {
				continue
			}
			var doc map[string]any
			if err := yaml.Unmarshal(f.Data, &doc); err != nil {
				t.Fatalf("decoding the planned Deployment %s: %v", f.Name, err)
			}
			metadata := planObjectField(t, doc, "metadata")
			revision := planRevisionOf(strings.TrimPrefix(resource, "deployment/"))
			metadata["resourceVersion"] = "rv-" + revision
			if annotations, ok := metadata["annotations"].(map[string]any); ok {
				annotations[planRevisionAnnotation] = revision
			} else {
				metadata["annotations"] = map[string]any{planRevisionAnnotation: revision}
			}
			if mutate != nil {
				mutate(t, doc)
			}
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("encoding the live %s: %v", resource, err)
			}
			return string(body), 0, nil
		}
		t.Fatalf("the plan rendered no %s", resource)
		return "", 0, nil
	})
}

// planPriorityClassLine is the line the compare prints for the fake's one static
// difference: the gt-ci PriorityClass's value, 101 live against the render's 100.
// The path is not on the compare's reviewed print list, so each side prints as the
// first twelve hex digits of the SHA-256 of its JSON and the line says the values
// are hidden.
func planPriorityClassLine() string {
	return "priorityclass/gt-ci: value: rendered sha256:ad5736686512, observed sha256:16dc368a89b4 (values hidden: path not on the reviewed list)"
}

// planRevisionAnswer answers a Deployment's get with the revision planRevisionOf
// gives its name.
func planRevisionAnswer(t *testing.T) func(resource string) (string, int, error) {
	return func(resource string) (string, int, error) {
		return planLiveAnswer(t, resource, planRevisionOf(strings.TrimPrefix(resource, "deployment/"))), 0, nil
	}
}

// planWantDeployment is the resource a rendered file names when it is a Deployment,
// and none for any other kind.
func planWantDeployment(t *testing.T, f planFile) string {
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
	return "deployment/" + head.Metadata.Name
}

// planWantRevision is the revision plan.json records for a rendered file: the
// revision planRevisionOf gives a Deployment's name, and none for any other kind.
func planWantRevision(t *testing.T, f planFile) string {
	t.Helper()
	resource := planWantDeployment(t, f)
	if resource == "" {
		return ""
	}
	return planRevisionOf(strings.TrimPrefix(resource, "deployment/"))
}

// planWantResourceVersion is the resourceVersion plan.json records for a rendered
// file: rv-<revision> for a Deployment, the revision planWantRevision gives it.
func planWantResourceVersion(t *testing.T, f planFile) string {
	t.Helper()
	rev := planWantRevision(t, f)
	if rev == "" {
		return ""
	}
	return "rv-" + rev
}

// TestClusterShapePlanLiveResourceVersion: plan.json records each rendered
// Deployment's resourceVersion beside its revision, and a non-Deployment file
// records none.
func TestClusterShapePlanLiveResourceVersion(t *testing.T) {
	t.Parallel()
	dir := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), planKubectl(t), &bytes.Buffer{}, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
		t.Fatalf("running the plan: %v", err)
	}
	rec, _ := readPlanRecord(t, dir)
	files := planFilesIn(t, dir)
	if len(rec.Files) != len(files) {
		t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
	}
	recorded := 0
	for i, f := range files {
		want := planWantResourceVersion(t, f)
		if rec.Files[i].ResourceVersion != want {
			t.Errorf("plan.json resourceVersion for %s = %q, want %q", f.Name, rec.Files[i].ResourceVersion, want)
		}
		if want != "" {
			recorded++
			if rec.Files[i].Revision == "" {
				t.Errorf("plan.json records resourceVersion %q for %s with no revision beside it", want, f.Name)
			}
		}
	}
	if recorded == 0 {
		t.Fatal("the plan rendered no Deployment, so no resourceVersion was recorded")
	}
}

// TestClusterShapePlanLiveRevisions: plan.json records each rendered Deployment's
// revision as the cluster reads it, one get per Deployment and no other call.
func TestClusterShapePlanLiveRevisions(t *testing.T) {
	t.Parallel()
	var reads []string
	kube := planKubectlAnswering(t, func(resource string) (string, int, error) {
		reads = append(reads, resource)
		return planLiveAnswer(t, resource, planRevisionOf(strings.TrimPrefix(resource, "deployment/"))), 0, nil
	})
	dir := planOut(t)
	if err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, &bytes.Buffer{}, dir, planTestKubeconfigs, atomicfile.WriteJSON); err != nil {
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
			deployments = append(deployments, planWantDeployment(t, f))
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
		{"the live answer is an empty object", func(string) (string, int, error) { return `{}`, 0, nil }, "has no fields, so it was not read"},
		{"the live object has no revision", func(string) (string, int, error) { return `{"metadata":{}}`, 0, nil }, "has no revision annotation"},
		{"the live object has no resourceVersion", func(string) (string, int, error) {
			return `{"metadata":{"annotations":{"deployment.kubernetes.io/revision":"1"}}}`, 0, nil
		}, "has no resourceVersion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := planOut(t)
			out := &bytes.Buffer{}
			kube := planKubectlAnswering(t, tc.answer)
			err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
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
		_, _, _, _, err := planLiveVersions(context.Background(), planKubectl(t), []byte("kind: ["))
		if err == nil || !strings.Contains(err.Error(), "cannot read a rendered file's kind") {
			t.Fatalf("planLiveVersions = %v, want the unreadable-kind refusal", err)
		}
	})
	t.Run("a rendered file's kind is not a string", func(t *testing.T) {
		t.Parallel()
		_, _, _, _, err := planLiveVersions(context.Background(), planKubectl(t), []byte("kind: {a: b}"))
		if err == nil || !strings.Contains(err.Error(), "has no kind to read") {
			t.Fatalf("planLiveVersions = %v, want the unreadable-kind refusal", err)
		}
	})
}

// TestClusterShapePlanRevisionRefuses: a live Deployment whose revision annotation
// is zero, negative, not a whole number, or missing refuses the plan. The refusal
// names the Deployment the plan read and the value, prints no id and writes no
// plan.json.
func TestClusterShapePlanRevisionRefuses(t *testing.T) {
	t.Parallel()
	annotated := func(revision string) func(string) (string, int, error) {
		return func(resource string) (string, int, error) { return planLiveAnswer(t, resource, revision), 0, nil }
	}
	cases := []struct {
		name   string
		answer func(resource string) (string, int, error)
		want   string
	}{
		{"the revision is zero", annotated("0"), `revision "0", which is not a positive whole number`},
		{"the revision is negative", annotated("-3"), `revision "-3", which is not a positive whole number`},
		{"the revision is not a number", annotated("three"), `revision "three", which is not a positive whole number`},
		{"the revision is a fraction", annotated("1.5"), `revision "1.5", which is not a positive whole number`},
		{"the annotation is missing", func(string) (string, int, error) {
			return `{"metadata":{"resourceVersion":"rv-1"}}`, 0, nil
		}, "has no revision annotation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reads []string
			kube := planKubectlAnswering(t, func(resource string) (string, int, error) {
				reads = append(reads, resource)
				return tc.answer(resource)
			})
			dir := planOut(t)
			out := &bytes.Buffer{}
			err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
			if len(reads) == 0 {
				t.Fatal("the plan read no Deployment, so the refusal was never reached")
			}
			if err == nil || !strings.Contains(err.Error(), reads[0]) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownClusterPlan = %v, want an error naming %s and holding %q", err, reads[0], tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused plan printed %q, want nothing", out.String())
			}
			if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("plan.json exists or cannot be checked (%v), want none written", statErr)
			}
		})
	}
}

// TestClusterShapePlanLiveAbsent: a Deployment the cluster does not hold is a normal
// plan, recorded absent with no revision and no resourceVersion. The read answers
// absent with no output at exit 0; a get that exits nonzero or errors with no
// output is not absent and still refuses.
func TestClusterShapePlanLiveAbsent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		answer  func(resource string) (string, int, error)
		wantErr string
	}{
		{"every Deployment is absent", func(string) (string, int, error) { return "", 0, nil }, ""},
		{"the get exits nonzero with no output", func(string) (string, int, error) { return "", 1, nil }, "could not be read"},
		{"the get errors with no output", func(string) (string, int, error) { return "", 0, errors.New("connection refused") }, "reading the live"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := planOut(t)
			out := &bytes.Buffer{}
			kube := planKubectlAnswering(t, tc.answer)
			err := runTownClusterPlan(context.Background(), clusterRenderDepsAt(t, planTown(t), io.Discard), kube, out, dir, planTestKubeconfigs, atomicfile.WriteJSON)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("runTownClusterPlan = %v, want an error holding %q", err, tc.wantErr)
				}
				if out.Len() != 0 {
					t.Fatalf("a refused plan printed %q, want nothing", out.String())
				}
				if _, statErr := os.Stat(filepath.Join(dir, planFileName)); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("plan.json exists or cannot be checked (%v), want none written", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("running the plan: %v", err)
			}
			if strings.TrimSpace(out.String()) == "" {
				t.Fatal("the plan printed no id, want the short id")
			}
			rec, _ := readPlanRecord(t, dir)
			files := planFilesIn(t, dir)
			if len(rec.Files) != len(files) {
				t.Fatalf("plan.json lists %d files, want the %d the plan rendered", len(rec.Files), len(files))
			}
			absent := 0
			for i, f := range files {
				got := rec.Files[i]
				if planWantRevision(t, f) == "" {
					if got.Absent || got.Revision != "" || got.ResourceVersion != "" {
						t.Errorf("plan.json for non-Deployment %s = %+v, want no absent mark and no live versions", f.Name, got)
					}
					continue
				}
				absent++
				if !got.Absent || got.Revision != "" || got.ResourceVersion != "" {
					t.Errorf("plan.json for absent Deployment %s = %+v, want absent with no revision and no resourceVersion", f.Name, got)
				}
			}
			if absent == 0 {
				t.Fatal("the plan rendered no Deployment, so no absent record was checked")
			}
		})
	}
}

// TestClusterShapePlanLiveGetOnly: every cluster call the plan makes is a get. The
// runner records each kubectl call and fails any that is not a get, so an apply,
// patch, delete or dry run would be caught. A Deployment present, absent or
// unreadable makes no other call, and each ends with the outcome its read names;
// the static objects are read with gets of their own.
func TestClusterShapePlanLiveGetOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		answer  func(resource string) (string, int, error)
		wantErr string
	}{
		{"the Deployment is present", func(resource string) (string, int, error) {
			return planLiveAnswer(t, resource, planRevisionOf(strings.TrimPrefix(resource, "deployment/"))), 0, nil
		}, ""},
		{"the Deployment is absent", func(string) (string, int, error) { return "", 0, nil }, ""},
		{"the Deployment is unreadable", func(string) (string, int, error) { return "", 1, nil }, "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			kube := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				if name != "kubectl" || len(args) < 2 || args[0] != "get" {
					return "", 1, errors.New("planted non-get call: " + name + " " + strings.Join(args, " "))
				}
				return tc.answer(args[1])
			}
			d := clusterRenderDepsAt(t, planTown(t), io.Discard)
			d.runHelm = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if len(args) == 0 || args[0] != "template" {
					return nil, errors.New("planted non-read helm call: " + name + " " + strings.Join(args, " "))
				}
				return []byte(sbxEngineHelmOutput), nil
			}
			err := runTownClusterPlan(context.Background(), d, kube, &bytes.Buffer{}, planOut(t), planTestKubeconfigs, atomicfile.WriteJSON)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("running the plan: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("runTownClusterPlan = %v, want an error holding %q", err, tc.wantErr)
			}
			if len(calls) == 0 {
				t.Fatal("the plan made no cluster call, so the read was never exercised")
			}
			for _, c := range calls {
				if !strings.HasPrefix(c, "kubectl get ") {
					t.Errorf("the plan made cluster call %q, want only a get", c)
				}
			}
		})
	}
}

// TestClusterShapePlanLongText: the command's help says the plan reads the
// cluster's Deployments and the static objects with get only and writes nothing
// to any cluster, says it prints the drift compare's lines after the phases, says
// a phase P file is listed only when a static object differs, says a Secret is
// compared on its declared fields and never on its data, and no longer says that
// no cluster is contacted.
func TestClusterShapePlanLongText(t *testing.T) {
	t.Parallel()
	long := strings.Join(strings.Fields(townClusterPlanCmd.Long), " ")
	if !strings.Contains(long, "reads the cluster's Deployments and the static objects with get only and writes nothing to any cluster") {
		t.Errorf("Long text = %q, want it to say the plan reads the cluster's Deployments and the static objects with get only and writes nothing to any cluster", long)
	}
	if !strings.Contains(long, "After the phases, the plan prints, in the drift command's form, the compare's lines for the Deployments and static objects that differ from the live cluster") {
		t.Errorf("Long text = %q, want it to say the plan prints the compare's lines for what differs after the phases", long)
	}
	if strings.Contains(long, "never either value") {
		t.Errorf("Long text = %q, says no value is printed, but the compare prints a reviewed path's value", long)
	}
	if !strings.Contains(long, "phase P file is listed only when a static object differs") {
		t.Errorf("Long text = %q, want it to say a phase P file is listed only when a static object differs", long)
	}
	if !strings.Contains(long, "Secret is compared on the fields its document declares and never on its data, which the cluster fills") {
		t.Errorf("Long text = %q, want it to say a Secret is compared on the fields its document declares and never on its data, which the cluster fills", long)
	}
	if strings.Contains(long, "no cluster is contacted") {
		t.Errorf("Long text = %q, still says no cluster is contacted", long)
	}
}
