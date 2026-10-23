package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func fixtureSpec(t *testing.T) SkeletonSpec {
	t.Helper()
	return SkeletonSpec{
		TownRoot:         filepath.Join("testdata", "town"),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
}

func renderFixture(t *testing.T) []byte {
	t.Helper()
	data, err := RenderSkeleton(fixtureSpec(t))
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	return data
}

// untar decodes the archive into entry name -> body, a directory mapping to nil.
func untar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	got := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			got[hdr.Name] = nil
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("%s: %v", hdr.Name, err)
		}
		got[hdr.Name] = body
	}
	return got
}

// at walks decoded JSON objects by key.
func at(t *testing.T, doc map[string]any, keys ...string) any {
	t.Helper()
	var cur any = doc
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%q: parent is %T, not an object", strings.Join(keys, "."), cur)
		}
		cur = obj[k]
	}
	return cur
}

// TestSkeletonFileSet: the archive holds exactly the files gt and bd read, a
// bare repo a host can push into, and nothing else.
func TestSkeletonFileSet(t *testing.T) {
	t.Parallel()

	spec := fixtureSpec(t)
	data := renderFixture(t)
	got := untar(t, data)

	want := []string{
		"gt/",
		"gt/.beads/metadata.json",
		"gt/.beads/routes.jsonl",
		"gt/directives/polecat.md",
		"gt/gastown/.repo.git/HEAD",
		"gt/gastown/.repo.git/config",
		"gt/gastown/.repo.git/objects/info/",
		"gt/gastown/.repo.git/refs/heads/",
		"gt/gastown/.repo.git/refs/tags/",
		"gt/gastown/directives/_common.md",
		"gt/gastown/mayor/rig/.beads/config.yaml",
		"gt/gastown/mayor/rig/.beads/metadata.json",
		"gt/gastown/metadata.json",
		"gt/gastown/settings/config.json",
		"gt/mayor/town.json",
		"gt/settings/config.json",
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Errorf("archive entries:\n got %q\nwant %q", names, want)
	}

	if head := string(got["gt/gastown/.repo.git/HEAD"]); head != "ref: refs/heads/main\n" {
		t.Errorf("HEAD = %q, want the main branch ref", head)
	}
	config := string(got["gt/gastown/.repo.git/config"])
	if !strings.Contains(config, "url = "+spec.SandboxRepoURL+"\n") {
		t.Errorf("bare repo config does not carry the sandbox-side origin:\n%s", config)
	}
	if !strings.Contains(config, "bare = true") {
		t.Errorf("bare repo config is not bare:\n%s", config)
	}
	if len(data) >= 64<<10 {
		t.Errorf("archive is %d bytes, want under 64 KiB", len(data))
	}
}

// TestSkeletonCarriesNoSecrets: no entry repeats a planted credential, the
// top-level secrets block is gone, and values that are not credentials survive.
func TestSkeletonCarriesNoSecrets(t *testing.T) {
	t.Parallel()

	got := untar(t, renderFixture(t))
	for name, body := range got {
		if bytes.Contains(body, []byte("PLANTED")) {
			t.Errorf("%s carries a planted secret", name)
		}
	}

	var town map[string]any
	if err := json.Unmarshal(got["gt/settings/config.json"], &town); err != nil {
		t.Fatalf("town settings: %v", err)
	}
	if _, ok := town["secrets"]; ok {
		t.Error("town settings kept the top-level secrets block")
	}
	if v := at(t, town, "token"); v != "REDACTED" {
		t.Errorf("token = %v, want REDACTED", v)
	}
	if v := at(t, town, "agents", "deepseek-flash", "env", "ANTHROPIC_API_KEY"); v != "REDACTED" {
		t.Errorf("agent env key = %v, want REDACTED", v)
	}
	if v := at(t, town, "agents", "deepseek-flash", "env", "ANTHROPIC_BASE_URL"); v != "http://host.docker.internal:4000" {
		t.Errorf("agent env URL = %v, want it kept", v)
	}
	if v := at(t, town, "keep", "note"); v != "kept" {
		t.Errorf("keep.note = %v, want it kept", v)
	}

	var rig map[string]any
	if err := json.Unmarshal(got["gt/gastown/settings/config.json"], &rig); err != nil {
		t.Fatalf("rig settings: %v", err)
	}
	if v := at(t, rig, "keeper", "token"); v != "REDACTED" {
		t.Errorf("rig keeper token = %v, want REDACTED", v)
	}
}

// TestSkeletonPointsAtTheSandboxSide: both beads metadata files reach Dolt at
// the control plane, and the rig settings carry the sandbox-side Forgejo URL
// beside the rest of the rig's merge-queue settings.
func TestSkeletonPointsAtTheSandboxSide(t *testing.T) {
	t.Parallel()

	spec := fixtureSpec(t)
	got := untar(t, renderFixture(t))

	for _, name := range []string{
		"gt/.beads/metadata.json",
		"gt/gastown/mayor/rig/.beads/metadata.json",
	} {
		var meta struct {
			DoltDatabase   string `json:"dolt_database"`
			DoltServerHost string `json:"dolt_server_host"`
			DoltServerPort int    `json:"dolt_server_port"`
		}
		if err := json.Unmarshal(got[name], &meta); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if meta.DoltServerHost != spec.ControlPlaneHost {
			t.Errorf("%s: dolt_server_host = %q, want %q", name, meta.DoltServerHost, spec.ControlPlaneHost)
		}
		if meta.DoltServerPort != 3307 {
			t.Errorf("%s: dolt_server_port = %d, want it left at 3307", name, meta.DoltServerPort)
		}
		if meta.DoltDatabase == "" {
			t.Errorf("%s: lost dolt_database", name)
		}
	}

	var rig struct {
		MergeQueue struct {
			TestCommand string `json:"test_command"`
			Forgejo     struct {
				RemoteURL     string `json:"remote_url"`
				LandingBranch string `json:"landing_branch"`
			} `json:"forgejo"`
		} `json:"merge_queue"`
	}
	if err := json.Unmarshal(got["gt/gastown/settings/config.json"], &rig); err != nil {
		t.Fatalf("rig settings: %v", err)
	}
	if rig.MergeQueue.Forgejo.RemoteURL != spec.SandboxRepoURL {
		t.Errorf("remote_url = %q, want %q", rig.MergeQueue.Forgejo.RemoteURL, spec.SandboxRepoURL)
	}
	if rig.MergeQueue.TestCommand != "make gate" || rig.MergeQueue.Forgejo.LandingBranch != "main" {
		t.Errorf("rewrite dropped the rest of the rig settings: %+v", rig.MergeQueue)
	}
}

// TestScrubSettingsNested: redaction follows a credential-named key into
// nested objects and arrays, and leaves everything else alone.
func TestScrubSettingsNested(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"api_token": "PLANTED-TOKEN-1",
		"keep": "visible",
		"max_parallel": 3,
		"agents": {"flash": {"env": {"ANTHROPIC_API_KEY": "PLANTED-KEY-2"}}},
		"hooks": [{"name": "land", "password": "PLANTED-TOKEN-1"}],
		"secrets": {"forgejo_bot": "PLANTED-TOKEN-1"}
	}`)
	out, err := ScrubSettings(raw)
	if err != nil {
		t.Fatalf("ScrubSettings: %v", err)
	}
	if bytes.Contains(out, []byte("PLANTED")) {
		t.Fatalf("scrubbed settings still carry a planted secret:\n%s", out)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("scrubbed settings are not JSON: %v", err)
	}
	if _, ok := doc["secrets"]; ok {
		t.Error("top-level secrets block kept")
	}
	if v := at(t, doc, "api_token"); v != "REDACTED" {
		t.Errorf("api_token = %v, want REDACTED", v)
	}
	if v := at(t, doc, "keep"); v != "visible" {
		t.Errorf("keep = %v, want it kept", v)
	}
	if v := at(t, doc, "max_parallel"); v != float64(3) {
		t.Errorf("max_parallel = %v, want 3", v)
	}
	if v := at(t, doc, "agents", "flash", "env", "ANTHROPIC_API_KEY"); v != "REDACTED" {
		t.Errorf("nested key = %v, want REDACTED", v)
	}
	hooks, ok := doc["hooks"].([]any)
	if !ok || len(hooks) != 1 {
		t.Fatalf("hooks = %v, want one element", doc["hooks"])
	}
	hook, ok := hooks[0].(map[string]any)
	if !ok {
		t.Fatalf("hook = %T, want an object", hooks[0])
	}
	if v := at(t, hook, "password"); v != "REDACTED" {
		t.Errorf("hook password = %v, want REDACTED", v)
	}
	if v := at(t, hook, "name"); v != "land" {
		t.Errorf("hook name = %v, want it kept", v)
	}
}

// TestScrubSettingsRejectsANonObject: settings are a JSON object; anything else
// is an error, not a silently empty skeleton.
func TestScrubSettingsRejectsANonObject(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"not json", "[1, 2]", "null"} {
		if _, err := ScrubSettings([]byte(raw)); err == nil {
			t.Errorf("ScrubSettings(%q) = nil error, want one", raw)
		}
	}
}
