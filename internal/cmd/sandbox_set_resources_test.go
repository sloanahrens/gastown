package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// sandboxResourcesSettings holds a kubernetes profile with requests and limits,
// a kubernetes profile with only a top-level cpu, and a docker profile, plus the
// keys gt sandbox set-resources does not own.
const sandboxResourcesSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}},
  "role_agents": {"crew": "claude-sonnet"},
  "sandboxes": {
    "polecat-k8s": {
      "driver": "kubernetes",
      "gateway": "k8s",
      "image": "ghcr.io/example/polecat:1",
      "policy": "policies/polecat.yaml",
      "requests": {"cpu": "1", "memory": "3Gi"},
      "limits": {"cpu": "2", "memory": "4Gi"}
    },
    "polecat-other": {
      "driver": "kubernetes",
      "gateway": "k8s",
      "image": "ghcr.io/example/other:1",
      "policy": "policies/other.yaml",
      "cpu": "4"
    },
    "polecat-docker": {
      "driver": "docker",
      "gateway": "k8s",
      "image": "ghcr.io/example/docker:1",
      "policy": "policies/docker.yaml"
    }
  }
}
`

// sandboxResourcesBackups lists the copies kept beside the town's settings file.
func sandboxResourcesBackups(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(config.TownSettingsPath(root) + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// sandboxResourcesTree decodes the settings file as a generic JSON tree, so a
// test can compare keys it did not write without depending on byte layout.
func sandboxResourcesTree(t *testing.T, raw string) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		t.Fatalf("decoding settings: %v", err)
	}
	return tree
}

// sandboxResourcesEdits parses the flag values the way the command does, and
// runs the change. The error is the parse's or the run's, whichever refused.
func sandboxResourcesEdits(t *testing.T, root, profile, block, spec string, dryRun bool, out *bytes.Buffer) error {
	t.Helper()
	edits, err := parseSandboxResourceFlag(block, spec)
	if err != nil {
		return err
	}
	return runSandboxSetResources(out, root, profile, edits, dryRun)
}

// Setting requests changes only the named fields of the named profile: its
// limits, the other profile, and every key the command does not own stay as
// they were. The file is replaced with a .bak- copy of the old one beside it.
func TestSandboxSetResourcesChangesOnlyTheNamedFields(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	before := poolTestRawSettings(t, root)
	var out bytes.Buffer

	if err := sandboxResourcesEdits(t, root, "polecat-k8s", "requests", "cpu=500m,memory=1536Mi", false, &out); err != nil {
		t.Fatalf("set-resources: %v", err)
	}

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	k8s := ts.Sandboxes["polecat-k8s"]
	if k8s.Requests == nil || *k8s.Requests != (config.SandboxResources{CPU: "500m", Memory: "1536Mi"}) {
		t.Errorf("polecat-k8s requests = %+v, want cpu 500m and memory 1536Mi", k8s.Requests)
	}
	if k8s.Limits == nil || *k8s.Limits != (config.SandboxResources{CPU: "2", Memory: "4Gi"}) {
		t.Errorf("polecat-k8s limits = %+v, want them untouched at cpu 2 and memory 4Gi", k8s.Limits)
	}

	after := poolTestRawSettings(t, root)
	oldTree, newTree := sandboxResourcesTree(t, before), sandboxResourcesTree(t, after)
	oldSandboxes, newSandboxes := oldTree["sandboxes"].(map[string]any), newTree["sandboxes"].(map[string]any)
	for _, name := range []string{"polecat-other", "polecat-docker"} {
		if !reflect.DeepEqual(oldSandboxes[name], newSandboxes[name]) {
			t.Errorf("profile %s changed: %v -> %v", name, oldSandboxes[name], newSandboxes[name])
		}
	}
	for key := range oldTree {
		if key == "sandboxes" {
			continue
		}
		if !reflect.DeepEqual(oldTree[key], newTree[key]) {
			t.Errorf("settings key %q changed: %v -> %v", key, oldTree[key], newTree[key])
		}
	}

	for _, want := range []string{
		"sandboxes.polecat-k8s.requests.cpu: 1 -> 500m",
		"sandboxes.polecat-k8s.requests.memory: 3Gi -> 1536Mi",
		"sandboxes.polecat-k8s.limits.cpu: 2 -> 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out.String(), want)
		}
	}

	backups := sandboxResourcesBackups(t, root)
	if len(backups) != 1 {
		t.Fatalf("backups beside the settings file = %v, want exactly one .bak- copy", backups)
	}
	if got, err := os.ReadFile(backups[0]); err != nil || string(got) != before {
		t.Errorf("backup %s holds %q (%v), want the file as it was before the write", backups[0], got, err)
	}
	stray, err := filepath.Glob(config.TownSettingsPath(root) + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 0 {
		t.Errorf("temporary files left beside the settings file: %v", stray)
	}
}

// A profile with no limits gets them: a field that was unset prints as unset in
// the before column, and the requests it does not name stay absent.
func TestSandboxSetResourcesAddsLimitsToAProfileWithNone(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	var out bytes.Buffer

	if err := sandboxResourcesEdits(t, root, "polecat-other", "limits", "cpu=4,memory=8Gi", false, &out); err != nil {
		t.Fatalf("set-resources: %v", err)
	}

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	other := ts.Sandboxes["polecat-other"]
	if other.Limits == nil || *other.Limits != (config.SandboxResources{CPU: "4", Memory: "8Gi"}) {
		t.Errorf("polecat-other limits = %+v, want cpu 4 and memory 8Gi", other.Limits)
	}
	if other.Requests != nil {
		t.Errorf("polecat-other requests = %+v, want none: the change named only limits", other.Requests)
	}
	if !strings.Contains(out.String(), "sandboxes.polecat-other.limits.memory: (unset) -> 8Gi") {
		t.Errorf("output = %q, want the unset memory limit shown as (unset)", out.String())
	}
}

// --dry-run prints the change and writes nothing: the file keeps its bytes and
// no backup is made.
func TestSandboxSetResourcesDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	before := poolTestRawSettings(t, root)
	var out bytes.Buffer

	if err := sandboxResourcesEdits(t, root, "polecat-k8s", "requests", "cpu=500m", true, &out); err != nil {
		t.Fatalf("set-resources --dry-run: %v", err)
	}

	if !strings.Contains(out.String(), "sandboxes.polecat-k8s.requests.cpu: 1 -> 500m") {
		t.Errorf("dry-run output = %q, want the change it would make", out.String())
	}
	if !strings.Contains(out.String(), "dry run: nothing written") {
		t.Errorf("dry-run output = %q, want it to say nothing was written", out.String())
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a dry run rewrote the settings file")
	}
	if backups := sandboxResourcesBackups(t, root); len(backups) != 0 {
		t.Errorf("a dry run made backups: %v", backups)
	}
}

// Each refusal names what is wrong and leaves the file byte for byte as it was,
// with no backup made. Parse refusals come from the flag value; the rest from
// the whole profile the change would leave.
func TestSandboxSetResourcesRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		profile string
		block   string
		spec    string
		want    string
	}{
		{"an unknown profile", "polecat-missing", "requests", "cpu=500m", `"polecat-missing" is not a sandbox profile`},
		{"an unparsable memory quantity", "polecat-k8s", "requests", "memory=3GB", "want a k8s memory quantity"},
		{"an unparsable cpu quantity", "polecat-k8s", "requests", "cpu=0.5.5", "want a k8s CPU quantity"},
		{"requests above limits", "polecat-k8s", "requests", "cpu=3", "want no more than limits.cpu"},
		{"a docker profile", "polecat-docker", "requests", "cpu=1", "only a kubernetes driver sends pod placement and resources"},
		{"a change that is already in place", "polecat-k8s", "requests", "cpu=1,memory=3Gi", "nothing to change"},
		{"an empty value", "polecat-k8s", "requests", "cpu=", "empty quantity"},
		{"an empty flag", "polecat-k8s", "requests", "  ", "empty; want cpu="},
		{"an unknown resource", "polecat-k8s", "requests", "gpu=1", "unknown resource \"gpu\""},
		{"a resource named twice", "polecat-k8s", "limits", "cpu=1,cpu=2", "names cpu twice"},
		{"a pair with no equals sign", "polecat-k8s", "limits", "cpu", "want cpu=<quantity> or memory=<quantity>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, sandboxResourcesSettings, "")
			before := poolTestRawSettings(t, root)
			var out bytes.Buffer

			err := sandboxResourcesEdits(t, root, tc.profile, tc.block, tc.spec, false, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("set-resources = %v, want a refusal containing %q", err, tc.want)
			}
			if after := poolTestRawSettings(t, root); after != before {
				t.Error("a refused change rewrote the settings file")
			}
			if backups := sandboxResourcesBackups(t, root); len(backups) != 0 {
				t.Errorf("a refused change made backups: %v", backups)
			}
		})
	}
}

// A profile a seat runs is checked with its defaults applied, so one that names
// no gateway is written; the same profile with no seat is refused for its
// missing gateway, as the loader refuses it.
func TestSandboxSetResourcesChecksASeatedProfileWithItsDefaults(t *testing.T) {
	t.Parallel()
	const bare = `"polecat-bare": {"driver": "kubernetes", "image": "ghcr.io/example/bare:1", "policy": "policies/bare.yaml"}`
	seated := `{"type": "town-settings", "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 1, "backend": "openshell", "sandbox": "polecat-bare"}}},
  "sandboxes": {` + bare + `}}`
	unseated := `{"type": "town-settings", "version": 1, "sandboxes": {` + bare + `}}`
	edits, err := parseSandboxResourceFlag("requests", "cpu=500m")
	if err != nil {
		t.Fatal(err)
	}

	root := poolTestTown(t, seated, "")
	if err := runSandboxSetResources(&bytes.Buffer{}, root, "polecat-bare", edits, false); err != nil {
		t.Errorf("set-resources on a seated profile with no gateway: %v, want it written with the gateway it runs on", err)
	}

	root = poolTestTown(t, unseated, "")
	err = runSandboxSetResources(&bytes.Buffer{}, root, "polecat-bare", edits, false)
	if err == nil || !strings.Contains(err.Error(), "gateway: required") {
		t.Errorf("set-resources on an unseated profile with no gateway = %v, want the gateway refusal", err)
	}
}

// A run that names no field at all is refused before the file is touched.
func TestSandboxSetResourcesRefusalsWhenNothingIsNamed(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	before := poolTestRawSettings(t, root)

	err := runSandboxSetResources(&bytes.Buffer{}, root, "polecat-k8s", nil, false)
	if err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("set-resources with no fields = %v, want a nothing-to-change refusal", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a run with no fields rewrote the settings file")
	}
}

// A copy that cannot be made stops the write before the profile changes, so the
// file keeps its bytes and the error names the copy that failed.
func TestSandboxSetResourcesWriteFailureChangesNothing(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	before := poolTestRawSettings(t, root)
	edits, err := parseSandboxResourceFlag("requests", "cpu=500m")
	if err != nil {
		t.Fatal(err)
	}
	refuseCopy := func(string, string) (string, error) { return "", errors.New("copy refused") }

	_, _, err = writeSandboxResources(config.TownSettingsPath(root), "20260101T000000Z", "polecat-k8s", edits, refuseCopy)
	if err == nil || !strings.Contains(err.Error(), "copy refused") {
		t.Fatalf("write = %v, want the copy's refusal", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a write whose copy failed rewrote the settings file")
	}
}

// A settings file that does not parse is refused by a dry run and by a write
// alike, and the file is left as it was.
func TestSandboxSetResourcesRefusesACorruptSettingsFile(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{true, false} {
		name := "write"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const corrupt = `{"type": "town-settings", "sandboxes": `
			root := poolTestTown(t, corrupt, "")
			edits, err := parseSandboxResourceFlag("requests", "cpu=500m")
			if err != nil {
				t.Fatal(err)
			}

			if err := runSandboxSetResources(&bytes.Buffer{}, root, "polecat-k8s", edits, dryRun); err == nil {
				t.Fatal("set-resources on a corrupt settings file: want a refusal")
			}
			if after := poolTestRawSettings(t, root); after != corrupt {
				t.Errorf("settings file = %q, want it left as it was", after)
			}
			if backups := sandboxResourcesBackups(t, root); len(backups) != 0 {
				t.Errorf("a refused change made backups: %v", backups)
			}
		})
	}
}

// A dry run refuses a change a write would refuse, with the same reason, and
// writes nothing.
func TestSandboxSetResourcesDryRunRefusesWhatAWriteWould(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	before := poolTestRawSettings(t, root)
	edits, err := parseSandboxResourceFlag("requests", "cpu=500m")
	if err != nil {
		t.Fatal(err)
	}

	err = runSandboxSetResources(&bytes.Buffer{}, root, "polecat-missing", edits, true)
	if err == nil || !strings.Contains(err.Error(), `"polecat-missing" is not a sandbox profile`) {
		t.Fatalf("dry run of an unknown profile = %v, want the unknown-profile refusal", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a refused dry run rewrote the settings file")
	}
}

// The command reads its flags before it looks for the town, so a bad flag value
// is what it reports, and a town it cannot find is reported only for flags that
// are good.
func TestSandboxSetResourcesCommandReadsFlagsBeforeTheTown(t *testing.T) {
	t.Parallel()
	noTown := func() (string, error) { return "", errors.New("no town here") }
	for _, tc := range []struct {
		name  string
		flags sandboxSetResourcesFlags
		want  string
	}{
		{"a bad requests value", sandboxSetResourcesFlags{requests: "cpu=", requestsSet: true}, "empty quantity"},
		{"a bad limits value", sandboxSetResourcesFlags{limits: "memory=", limitsSet: true}, "empty quantity"},
		{"an empty memory value", sandboxSetResourcesFlags{requests: "memory=", requestsSet: true}, `"1536Mi"`},
		{"good flags and no town", sandboxSetResourcesFlags{requests: "cpu=500m", requestsSet: true}, "no town here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runSandboxSetResourcesCmd(&bytes.Buffer{}, noTown, "polecat-k8s", tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("set-resources = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// The command writes the town its finder returns: the profile changes in that
// town's settings file, and the output says what changed.
func TestSandboxSetResourcesCommandWritesTheTownItFinds(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxResourcesSettings, "")
	found := func() (string, error) { return root, nil }
	var out bytes.Buffer

	err := runSandboxSetResourcesCmd(&out, found, "polecat-k8s", sandboxSetResourcesFlags{requests: "cpu=500m", requestsSet: true})
	if err != nil {
		t.Fatalf("set-resources: %v", err)
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := ts.Sandboxes["polecat-k8s"].Requests; got == nil || got.CPU != "500m" {
		t.Errorf("polecat-k8s requests = %+v, want cpu 500m", got)
	}
	if !strings.Contains(out.String(), "sandboxes.polecat-k8s.requests.cpu: 1 -> 500m") {
		t.Errorf("output = %q, want the cpu change", out.String())
	}
}
