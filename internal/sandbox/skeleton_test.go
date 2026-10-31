package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
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

// writeTown writes a town under a fresh temporary root.
func writeTown(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// minimalTown holds only the files RenderSkeleton requires: rig settings and
// directives are optional.
func minimalTown() map[string]string {
	return map[string]string{
		"mayor/town.json":                        `{"type": "town"}`,
		".beads/routes.jsonl":                    `{"prefix":"gt-","path":"gastown"}` + "\n",
		".beads/metadata.json":                   `{"dolt_database": "hq", "dolt_server_host": "127.0.0.1", "dolt_server_port": 3307}`,
		"settings/config.json":                   `{"type": "town"}`,
		"gastown/metadata.json":                  `{"rig": "gastown"}`,
		"gastown/mayor/rig/.beads/config.yaml":   "issue_prefix: gt\n",
		"gastown/mayor/rig/.beads/metadata.json": `{"dolt_database": "gastown", "dolt_server_host": "127.0.0.1", "dolt_server_port": 3307}`,
	}
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

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return doc
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
		"gt/.beads/",
		"gt/.beads/metadata.json",
		"gt/.beads/routes.jsonl",
		"gt/directives/polecat.md",
		"gt/gastown/.beads/",
		"gt/gastown/.beads/redirect",
		"gt/gastown/.repo.git/HEAD",
		"gt/gastown/.repo.git/config",
		"gt/gastown/.repo.git/objects/info/",
		"gt/gastown/.repo.git/refs/heads/",
		"gt/gastown/.repo.git/refs/tags/",
		"gt/gastown/directives/_common.md",
		"gt/gastown/mayor/rig/.beads/",
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

	if redirect := string(got["gt/gastown/.beads/redirect"]); redirect != "mayor/rig/.beads\n" {
		t.Errorf("rig .beads/redirect = %q, want the pointer at the shared beads", redirect)
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
			t.Errorf("%s carries a planted secret:\n%s", name, body)
		}
	}

	town := decodeJSON(t, got["gt/settings/config.json"])
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

	rig := decodeJSON(t, got["gt/gastown/settings/config.json"])
	if v := at(t, rig, "keeper", "token"); v != "REDACTED" {
		t.Errorf("rig keeper token = %v, want REDACTED", v)
	}
}

// TestSkeletonScrubsCopiedFiles: every copied file is scrubbed, not just the
// settings documents (gt-2929c).
func TestSkeletonScrubsCopiedFiles(t *testing.T) {
	t.Parallel()

	got := untar(t, renderFixture(t))

	town := decodeJSON(t, got["gt/mayor/town.json"])
	if v := at(t, town, "registry", "rigs", "gastown", "git_url"); v != "http://REDACTED@forgejo:3000/sloan/gastown.git" {
		t.Errorf("town.json git_url = %v, want the userinfo redacted and the host kept", v)
	}
	if v := at(t, town, "name"); v != "testtown" {
		t.Errorf("town.json name = %v, want it kept", v)
	}

	meta := decodeJSON(t, got["gt/gastown/metadata.json"])
	if v := at(t, meta, "repo"); v != "https://REDACTED@forgejo:3000/sloan/gastown.git" {
		t.Errorf("rig metadata repo = %v, want the userinfo redacted", v)
	}

	routes := strings.Split(strings.TrimSpace(string(got["gt/.beads/routes.jsonl"])), "\n")
	if len(routes) != 1 {
		t.Fatalf("routes lines = %q, want one", routes)
	}
	route := decodeJSON(t, []byte(routes[0]))
	if v := at(t, route, "auth"); v != "REDACTED" {
		t.Errorf("route auth = %v, want REDACTED", v)
	}
	if v := at(t, route, "path"); v != "gastown" {
		t.Errorf("route path = %v, want it kept", v)
	}

	config := string(got["gt/gastown/mayor/rig/.beads/config.yaml"])
	if !strings.Contains(config, "dolt_server_token: REDACTED\n") {
		t.Errorf("beads config kept a token:\n%s", config)
	}
	if !strings.Contains(config, "issue_prefix: gt\n") {
		t.Errorf("beads config lost its non-secret line:\n%s", config)
	}

	directive := string(got["gt/directives/polecat.md"])
	if strings.Contains(directive, "PLANTED") || !strings.Contains(directive, "Bearer REDACTED") {
		t.Errorf("directive kept a bearer token:\n%s", directive)
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

// TestSkeletonWithoutOptionalFiles: a rig with no settings and a town with no
// directives still renders, and the rig still gets the sandbox-side remote.
func TestSkeletonWithoutOptionalFiles(t *testing.T) {
	t.Parallel()

	spec := SkeletonSpec{
		TownRoot:         writeTown(t, minimalTown()),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	data, err := RenderSkeleton(spec)
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	got := untar(t, data)

	for name := range got {
		if strings.Contains(name, "directives") {
			t.Errorf("archive holds %s, want no directive entries", name)
		}
	}
	rig := decodeJSON(t, got["gt/gastown/settings/config.json"])
	if v := at(t, rig, "merge_queue", "forgejo", "remote_url"); v != spec.SandboxRepoURL {
		t.Errorf("remote_url = %v, want %q", v, spec.SandboxRepoURL)
	}
}

// TestSkeletonWithoutRigMetadata: a rig whose metadata lives under .beads/ has
// no <rig>/metadata.json. The skeleton still renders, without that entry, and
// keeps the rig's other files.
func TestSkeletonWithoutRigMetadata(t *testing.T) {
	t.Parallel()

	files := minimalTown()
	delete(files, "gastown/metadata.json")
	spec := SkeletonSpec{
		TownRoot:         writeTown(t, files),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	data, err := RenderSkeleton(spec)
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	got := untar(t, data)

	if _, ok := got["gt/gastown/metadata.json"]; ok {
		t.Errorf("archive holds gt/gastown/metadata.json, want no entry for a rig without it")
	}
	for _, name := range []string{"gt/mayor/town.json", "gt/.beads/routes.jsonl", "gt/gastown/mayor/rig/.beads/config.yaml"} {
		if _, ok := got[name]; !ok {
			t.Errorf("archive lacks %s, want it kept", name)
		}
	}
}

// TestSkeletonRejectsADirectivesFile: a directives path that is not a directory
// is an error, not a silent omission.
func TestSkeletonRejectsADirectivesFile(t *testing.T) {
	t.Parallel()

	town := minimalTown()
	town["directives"] = "not a directory\n"
	spec := SkeletonSpec{
		TownRoot:         writeTown(t, town),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	if _, err := RenderSkeleton(spec); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("RenderSkeleton error = %v, want a not-a-directory error", err)
	}
}

// TestSkeletonRefusesAnEscapingSymlink: a required file swapped for a symlink
// pointing outside the town fails the read instead of shipping the host file.
func TestSkeletonRefusesAnEscapingSymlink(t *testing.T) {
	t.Parallel()

	town := minimalTown()
	townRoot := writeTown(t, town)
	if err := os.Remove(filepath.Join(townRoot, ".beads", "metadata.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"dolt_server_host": "PLANTED-HOST"}`), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(townRoot, ".beads", "metadata.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	spec := SkeletonSpec{
		TownRoot:         townRoot,
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	data, err := RenderSkeleton(spec)
	if err == nil {
		t.Fatalf("RenderSkeleton = %d bytes, want an error for the escaping symlink", len(data))
	}
	if !strings.Contains(err.Error(), ".beads/metadata.json") {
		t.Errorf("error = %v, want it to name the refused path", err)
	}
}

// TestSkeletonRejectsMalformedJSON: a copied JSON file that is not an object is
// an error; encoding a nil skeleton would drop the town silently.
func TestSkeletonRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"not JSON":      "not json\n",
		"an array":      `[1, 2]`,
		"a JSON scalar": `"town"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town := minimalTown()
			town["mayor/town.json"] = body
			spec := SkeletonSpec{
				TownRoot:         writeTown(t, town),
				Rig:              "gastown",
				ControlPlaneHost: "host.docker.internal",
				SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
			}
			if _, err := RenderSkeleton(spec); err == nil {
				t.Fatalf("RenderSkeleton(%s) = nil error, want one", name)
			}
		})
	}
}

// TestSkeletonRejectsAMissingTownRoot: the error names the root instead of
// rendering an empty town.
func TestSkeletonRejectsAMissingTownRoot(t *testing.T) {
	t.Parallel()

	spec := SkeletonSpec{
		TownRoot:         filepath.Join(t.TempDir(), "absent"),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	if _, err := RenderSkeleton(spec); err == nil {
		t.Fatal("RenderSkeleton = nil error, want one")
	}
}

// TestSkeletonValidation: an unusable spec is refused before any read, so a
// path-shaped rig or a blank host never becomes an archive entry.
func TestSkeletonValidation(t *testing.T) {
	t.Parallel()

	valid := SkeletonSpec{
		TownRoot:         t.TempDir(),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	}
	tests := []struct {
		name string
		mut  func(*SkeletonSpec)
		want string
	}{
		{"no town root", func(s *SkeletonSpec) { s.TownRoot = "" }, "TownRoot"},
		{"empty rig", func(s *SkeletonSpec) { s.Rig = "" }, "rig"},
		{"dot rig", func(s *SkeletonSpec) { s.Rig = "." }, "rig"},
		{"parent rig", func(s *SkeletonSpec) { s.Rig = ".." }, "rig"},
		{"nested rig", func(s *SkeletonSpec) { s.Rig = "a/b" }, "rig"},
		{"backslash rig", func(s *SkeletonSpec) { s.Rig = `a\b` }, "rig"},
		{"empty host", func(s *SkeletonSpec) { s.ControlPlaneHost = "" }, "host"},
		{"host with a space", func(s *SkeletonSpec) { s.ControlPlaneHost = "a b" }, "host"},
		{"host with a slash", func(s *SkeletonSpec) { s.ControlPlaneHost = "a/b" }, "host"},
		{"empty URL", func(s *SkeletonSpec) { s.SandboxRepoURL = "" }, "URL"},
		{"URL with a newline", func(s *SkeletonSpec) { s.SandboxRepoURL = "http://a\nb" }, "URL"},
		{"URL with a NUL", func(s *SkeletonSpec) { s.SandboxRepoURL = "http://a\x00b" }, "URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := valid
			tt.mut(&spec)
			err := spec.validate()
			if err == nil {
				t.Fatalf("validate(%+v) = nil error, want one", spec)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to name %q", err, tt.want)
			}
		})
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

	doc := decodeJSON(t, out)
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

// TestScrubSettingsRedactsNonStringValues: a number or bool under a credential
// key is redacted too; a scalar under an innocent key is kept.
func TestScrubSettingsRedactsNonStringValues(t *testing.T) {
	t.Parallel()

	out, err := ScrubSettings([]byte(`{
		"api_key": 12345,
		"token_enabled": false,
		"retries": 3,
		"enabled": true,
		"nested": {"password": null}
	}`))
	if err != nil {
		t.Fatalf("ScrubSettings: %v", err)
	}
	doc := decodeJSON(t, out)
	for _, keys := range [][]string{
		{"api_key"},
		{"token_enabled"},
		{"nested", "password"},
	} {
		if v := at(t, doc, keys...); v != "REDACTED" {
			t.Errorf("%s = %v, want REDACTED", strings.Join(keys, "."), v)
		}
	}
	if v := at(t, doc, "retries"); v != float64(3) {
		t.Errorf("retries = %v, want 3", v)
	}
	if v := at(t, doc, "enabled"); v != true {
		t.Errorf("enabled = %v, want true", v)
	}
}

// TestScrubSettingsRejectsANonObject: settings are a JSON object; anything else
// is an error, not a silently empty skeleton.
func TestScrubSettingsRejectsANonObject(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"not json", "[1, 2]", "null", `"settings"`, `{"a": 1} {"b": 2}`} {
		if _, err := ScrubSettings([]byte(raw)); err == nil {
			t.Errorf("ScrubSettings(%q) = nil error, want one", raw)
		}
	}
}

// TestCredentialName: credential keys are recognised however they spell their
// words, and ordinary settings keys are not.
func TestCredentialName(t *testing.T) {
	t.Parallel()

	secret := []string{
		"token", "tokens", "api_token", "APIKey", "apikey", "accesskey",
		"ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "db.password", "passwd",
		"clientSecret", "secret", "secrets", "private-key", "privatekey",
		"signing_key", "promote_key_file", "credentials", "authorization",
		"auth_token", "bearer", "oauth", "jwt", "dsn", "cookie", "DB_PASSWD",
	}
	for _, key := range secret {
		if !credentialName(key) {
			t.Errorf("credentialName(%q) = false, want true", key)
		}
	}
	plain := []string{
		"type", "version", "name", "keep", "note", "max_parallel", "retries",
		"merge_queue", "remote_url", "landing_branch", "agent", "command",
		"forgejo", "bots", "registry", "backend", "session_backend", "host",
		"dolt_server_host", "dolt_database", "dolt_server_port", "issue_prefix",
		"SECRETARY_NAME", "TOKENIZER_MODE",
	}
	for _, key := range plain {
		if credentialName(key) {
			t.Errorf("credentialName(%q) = true, want false", key)
		}
	}
}

// TestCredentialNameAgreesWithConfigMarkers: every key config redacts by marker is
// a credential name here too, so the skeleton never copies a value config refuses.
func TestCredentialNameAgreesWithConfigMarkers(t *testing.T) {
	t.Parallel()
	for _, marker := range config.SecretKeyMarkers() {
		key := "DB_" + marker
		if !credentialName(key) {
			t.Errorf("credentialName(%q) = false, but config.IsSecretKey(%q) = true", key, key)
		}
	}
}

// TestLooksLikeCredential: a credential-shaped value is recognised whatever key
// carries it, and prose, paths, hosts and URLs without userinfo are not.
func TestLooksLikeCredential(t *testing.T) {
	t.Parallel()

	credential := []string{
		"http://bot:s3cret@forgejo:3000/sloan/gastown.git",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.fixture-signature-not-a-real-token",
		"sk-abcdefghijklmnopqrstuvwx",
		"ghp_abcdefghijklmnopqrstuvwxyz",
		"github_pat_11ABCDEFGHIJKLMNOPQRSTUV",
		"glpat-abcdefghijklmnopqrst",
		"xoxb-fixture-not-a-real-token",
		"AKIAIOSFODNN7EXAMPLE",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"dckr_pat_abcdefghijklmnopqrstuvwx",
		"hvs.abcdefghijklmnopqrstuvwx",
		"-----BEGIN RSA PRIVATE KEY-----",
		"0123456789abcdef0123456789abcdef01234567",
		"AbC3dEf6GhI9jKl2MnO5pQr8StU1vWx4YzA7",
		"abcdefghijklmnop12345678qrstuvwx",
		"ASIA0123456789ABCDEF",
	}
	for _, value := range credential {
		if !looksLikeCredential(value) {
			t.Errorf("looksLikeCredential(%q) = false, want true", value)
		}
	}
	plain := []string{
		"http://host.docker.internal:4000",
		"http://forgejo:3000/sloan/gastown.git",
		"host.docker.internal",
		"make gate",
		"main",
		"cleared",
		"~/gt/gastown/settings/config.json",
		"2026-10-22-openshell-sandbox-backend-plan.md",
		"openshell-sandbox-backend-plan-for-polecats",
		"ASIA-PACIFIC-EAST-1",
	}
	for _, value := range plain {
		if looksLikeCredential(value) {
			t.Errorf("looksLikeCredential(%q) = true, want false", value)
		}
	}
}

// TestStripUserInfo: only the credentials leave the URL.
func TestStripUserInfo(t *testing.T) {
	t.Parallel()

	if got, want := stripUserInfo("http://bot:PLANTED-TOKEN-1@forgejo:3000/sloan/gastown.git"), "http://REDACTED@forgejo:3000/sloan/gastown.git"; got != want {
		t.Errorf("stripUserInfo = %q, want %q", got, want)
	}
	if got := stripUserInfo("https://forgejo:3000/sloan/gastown.git"); got != "https://forgejo:3000/sloan/gastown.git" {
		t.Errorf("stripUserInfo changed a URL without userinfo: %q", got)
	}
}

// TestScrubText: text files keep their lines and keys, and lose the credentials
// a URL, a bearer header or a credential assignment carried.
func TestScrubText(t *testing.T) {
	t.Parallel()

	in := strings.Join([]string{
		"# Directive",
		"issue_prefix: gt",
		"password: PLANTED-TOKEN-1",
		"Push: git push http://bot:PLANTED-TOKEN-1@forgejo:3000/x.git",
		`curl -H "Authorization: Bearer PLANTED-KEY-2" http://forgejo:3000/api`,
		"- token: PLANTED-TOKEN-1",
		"",
	}, "\n")
	got := string(scrubText([]byte(in)))
	if strings.Contains(got, "PLANTED") {
		t.Fatalf("text scrub left a planted secret:\n%s", got)
	}
	for _, want := range []string{
		"issue_prefix: gt",
		"password: REDACTED",
		"http://REDACTED@forgejo:3000/x.git",
		"Bearer REDACTED",
		"- token: REDACTED",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubbed text lost %q:\n%s", want, got)
		}
	}
}

// TestSanitizeFileJSONL: every line is scrubbed and stays one line per object.
func TestSanitizeFileJSONL(t *testing.T) {
	t.Parallel()

	in := []byte(`{"prefix":"gt-","path":"gastown","token":"PLANTED-TOKEN-1"}
{"prefix":"om-","path":"om"}
`)
	out, err := sanitizeFile("routes.jsonl", in)
	if err != nil {
		t.Fatalf("sanitizeFile: %v", err)
	}
	if bytes.Contains(out, []byte("PLANTED")) {
		t.Fatalf("JSONL scrub left a planted secret:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL lines = %d, want 2:\n%s", len(lines), out)
	}
	if v := at(t, decodeJSON(t, []byte(lines[0])), "token"); v != "REDACTED" {
		t.Errorf("token = %v, want REDACTED", v)
	}
	if v := at(t, decodeJSON(t, []byte(lines[1])), "path"); v != "om" {
		t.Errorf("path = %v, want it kept", v)
	}
}

// TestSanitizeFileJSONLRejectsANonObjectLine: a line that is not one JSON object
// is an error rather than a line that ships unscrubbed.
func TestSanitizeFileJSONLRejectsANonObjectLine(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"not JSON": "nope\n",
		"an array": "[1]\n",
		"a null":   "null\n",
		"a scalar": `"x"` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := sanitizeFile("routes.jsonl", []byte(in)); err == nil {
				t.Fatalf("sanitizeFile(%q) = nil error, want one", in)
			}
		})
	}
}

// TestSanitizeFileRejectsANonObjectJSON: a JSON file that is not an object is
// an error, so nothing is emitted without passing the scrubber.
func TestSanitizeFileRejectsANonObjectJSON(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"not JSON":      "nope",
		"an array":      "[1]",
		"trailing data": `{"a": 1} {"b": 2}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := sanitizeFile("town.json", []byte(in)); err == nil {
				t.Fatalf("sanitizeFile(%q) = nil error, want one", in)
			}
		})
	}
}

// bd refuses to connect when metadata.json's project_id differs from the
// database's, and the UUID reads as a token to the value-shape rules, so the
// two beads metadata files keep it. Credential-shaped values beside it stay
// redacted. Directories named .beads are 0700 (bd warns otherwise); the rest
// stay 0755 (gt-zkkdo).
func TestSkeletonKeepsProjectIDAndTightensBeadsDirs(t *testing.T) {
	t.Parallel()

	const id = "8f550f3e-d9e1-452f-bb70-6a991fdb8fdb"
	files := minimalTown()
	files[".beads/metadata.json"] = `{"project_id": "` + id + `", "dolt_database": "hq", "dolt_server_port": 3307, "dolt_password": "hunter2"}`
	files["gastown/mayor/rig/.beads/metadata.json"] = `{"project_id": "` + id + `", "dolt_database": "gastown", "dolt_server_port": 3307}`
	data, err := RenderSkeleton(SkeletonSpec{
		TownRoot:         writeTown(t, files),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	})
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	got := untar(t, data)

	for _, name := range []string{"gt/.beads/metadata.json", "gt/gastown/mayor/rig/.beads/metadata.json"} {
		if v := at(t, decodeJSON(t, got[name]), "project_id"); v != id {
			t.Errorf("%s project_id = %v, want %q", name, v, id)
		}
	}
	if v := at(t, decodeJSON(t, got["gt/.beads/metadata.json"]), "dolt_password"); v != "REDACTED" {
		t.Errorf("dolt_password = %v, want REDACTED", v)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	beads := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeDir {
			continue
		}
		want := int64(0o755)
		if strings.HasSuffix(strings.TrimSuffix(hdr.Name, "/"), "/.beads") {
			want = 0o700
			beads++
		}
		if hdr.Mode != want {
			t.Errorf("dir %s mode = %o, want %o", hdr.Name, hdr.Mode, want)
		}
	}
	if beads == 0 {
		t.Error("archive holds no .beads directory entry")
	}
}

// bd's project_id is kept only when it is a UUID. Any other string under that
// key could be a credential, so the render fails instead of passing it through.
func TestSkeletonRejectsANonUUIDProjectID(t *testing.T) {
	t.Parallel()

	files := minimalTown()
	files[".beads/metadata.json"] = `{"project_id": "hunter2-not-a-uuid", "dolt_database": "hq", "dolt_server_port": 3307}`
	_, err := RenderSkeleton(SkeletonSpec{
		TownRoot:         writeTown(t, files),
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	})
	if err == nil {
		t.Fatal("RenderSkeleton kept a project_id that is not a UUID")
	}
}

// gt must read the scrubbed town settings back: a price per billion tokens
// stays a number, while a string under that key and other numbers under
// credential-named keys are still redacted (gt-zkkdo).
func TestScrubSettingsKeepsTheTokenPriceNumber(t *testing.T) {
	t.Parallel()

	out, err := ScrubSettings([]byte(`{
		"patrols": {"jev": {"usd_per_billion_tokens": 42, "max_usd_per_day": 1}},
		"odd": {"usd_per_billion_tokens": "sk-should-not-survive-this-string-0123456789"},
		"pin": {"api_token": 123456},
		"under": {"credentials_x": {"usd_per_billion_tokens": 7}}
	}`))
	if err != nil {
		t.Fatalf("ScrubSettings: %v", err)
	}
	var settings config.TownSettings
	if err := json.Unmarshal(out, &settings); err != nil {
		t.Fatalf("scrubbed settings do not parse as TownSettings: %v", err)
	}
	doc := decodeJSON(t, out)
	if v := at(t, doc, "patrols", "jev", "usd_per_billion_tokens"); v != float64(42) {
		t.Errorf("usd_per_billion_tokens = %#v, want the number 42", v)
	}
	if v := at(t, doc, "patrols", "jev", "max_usd_per_day"); v != float64(1) {
		t.Errorf("max_usd_per_day = %#v, want 1", v)
	}
	if v := at(t, doc, "odd", "usd_per_billion_tokens"); v != "REDACTED" {
		t.Errorf("string under the rate key = %#v, want REDACTED", v)
	}
	if v := at(t, doc, "pin", "api_token"); v != "REDACTED" {
		t.Errorf("number under api_token = %#v, want REDACTED", v)
	}
}

// renderRigWithout renders the minimal town after dropping and adding files.
func renderRigWithout(t *testing.T, drop []string, add map[string]string) (map[string][]byte, error) {
	t.Helper()
	files := minimalTown()
	for _, f := range drop {
		delete(files, f)
	}
	for k, v := range add {
		files[k] = v
	}
	return renderTown(t, writeTown(t, files))
}

// renderTown renders the skeleton for rig gastown from townRoot and returns the
// archive's entries.
func renderTown(t *testing.T, townRoot string) (map[string][]byte, error) {
	t.Helper()
	data, err := RenderSkeleton(SkeletonSpec{
		TownRoot:         townRoot,
		Rig:              "gastown",
		ControlPlaneHost: "host.docker.internal",
		SandboxRepoURL:   "http://host.docker.internal:3000/sloan/gastown.git",
	})
	if err != nil {
		return nil, err
	}
	return untar(t, data), nil
}

// TestSkeletonRigConfigFromTheRigBeadsDir: a rig whose mayor/rig/.beads holds
// only metadata.json and a redirect (devops, fractals, beaver, agentlab) keeps
// its config in <rig>/.beads; the skeleton carries it to the tracked workspace
// (gt-z7ut1).
func TestSkeletonRigConfigFromTheRigBeadsDir(t *testing.T) {
	t.Parallel()

	got, err := renderRigWithout(t, []string{"gastown/mayor/rig/.beads/config.yaml"},
		map[string]string{
			"gastown/mayor/rig/.beads/redirect": "../../.beads\n",
			"gastown/.beads/config.yaml":        "issue_prefix: gt\n",
		})
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	if c := string(got["gt/gastown/mayor/rig/.beads/config.yaml"]); !strings.Contains(c, "issue_prefix: gt") {
		t.Errorf("tracked workspace config = %q, want the rig .beads config", c)
	}
	if _, ok := got["gt/gastown/.beads/config.yaml"]; ok {
		t.Error("the config was also written under the rig .beads, where the redirect already points away")
	}
}

// TestSkeletonRigConfigPrefersTheTrackedWorkspace: when both exist, the tracked
// workspace's config is the one carried.
func TestSkeletonRigConfigPrefersTheTrackedWorkspace(t *testing.T) {
	t.Parallel()

	got, err := renderRigWithout(t, nil, map[string]string{"gastown/.beads/config.yaml": "issue-prefix: other\n"})
	if err != nil {
		t.Fatalf("RenderSkeleton: %v", err)
	}
	if c := string(got["gt/gastown/mayor/rig/.beads/config.yaml"]); !strings.Contains(c, "issue_prefix: gt") {
		t.Errorf("config = %q, want the tracked workspace's", c)
	}
}

// TestSkeletonRigWithoutAnyBeadsConfig: a rig with no config.yaml in either
// beads directory stops the render rather than ship a workspace with no prefix.
func TestSkeletonRigWithoutAnyBeadsConfig(t *testing.T) {
	t.Parallel()

	_, err := renderRigWithout(t, []string{"gastown/mayor/rig/.beads/config.yaml"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no beads config.yaml") {
		t.Fatalf("RenderSkeleton error = %v, want it to name the missing config.yaml", err)
	}
}

// TestSkeletonRigConfigEscapingSymlinkFails: a rig config.yaml that is a symlink
// out of the town fails the render. It does not fall through to the valid copy
// in <rig>/.beads, and the outside file is not shipped.
func TestSkeletonRigConfigEscapingSymlinkFails(t *testing.T) {
	t.Parallel()

	files := minimalTown()
	delete(files, "gastown/mayor/rig/.beads/config.yaml")
	files["gastown/.beads/config.yaml"] = "issue_prefix: gt\n"
	townRoot := writeTown(t, files)
	outside := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outside, []byte("issue_prefix: PLANTED\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads", "config.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := renderTown(t, townRoot); err == nil {
		t.Fatal("RenderSkeleton succeeded, want an error for the escaping config symlink")
	} else if !strings.Contains(err.Error(), "gastown/mayor/rig/.beads/config.yaml") {
		t.Errorf("error = %v, want it to name the refused config", err)
	}
}
