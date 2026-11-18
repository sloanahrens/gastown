package data

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const composeFile = "docker-compose.yml"

// expectedUnit is the whole compose file as the contract reads it. The file must
// decode to exactly this document, so a key it does not name fails by
// construction: privileged, network_mode, a literal in command or environment,
// a service added, a bind in a top-level volume. Image values are compared as
// their variable text and never resolved.
func expectedUnit() map[string]any {
	return map[string]any{
		"name": "gt-data",
		"services": map[string]any{
			"dolt": map[string]any{
				"image":          "${GT_DOLT_IMAGE:?set GT_DOLT_IMAGE to the dolt-sql-server image pinned by digest}",
				"container_name": "gt-dolt",
				"restart":        "unless-stopped",
				"command": []any{
					"--config",
					"${GT_DOLT_CONFIG:?set GT_DOLT_CONFIG to the Dolt config file}",
				},
				"labels": map[string]any{"org.gastown.keep": "true"},
				"ports":  []any{"127.0.0.1:3307:3306"},
				"volumes": []any{
					"dolt-var:/var/lib/dolt",
					"${GT_DOLT_DATA:?set GT_DOLT_DATA to the .dolt-data directory}:${GT_DOLT_DATA}",
					"${GT_DOLT_BACKUPS:?set GT_DOLT_BACKUPS to the Dolt backups directory}:${GT_DOLT_BACKUPS}",
					"${GT_DOLT_CONFIG}:${GT_DOLT_CONFIG}:ro",
				},
				"mem_limit":     "8g",
				"memswap_limit": "8g",
				"healthcheck": map[string]any{
					"test":     []any{"CMD", "dolt", "--host", "127.0.0.1", "--port", "3306", "-u", "root", "sql", "-q", "select 1"},
					"interval": "10s",
					"timeout":  "5s",
					"retries":  5,
				},
			},
			"bd": map[string]any{
				"image":          "${BD_IMAGE:?set BD_IMAGE to a digest reference name@sha256:<digest>}",
				"container_name": "gt-bd",
				"restart":        "unless-stopped",
				"depends_on": map[string]any{
					"dolt": map[string]any{"condition": "service_healthy"},
				},
				"command": []any{
					"--tokens-dir", "/run/bdgw/tokens",
					"--verbs", "/etc/bdgw/verbs.json",
					"--log-dir", "/var/log/bdgw",
					"--workspace", "/var/lib/bdgw/ws",
				},
				"environment": map[string]any{
					"HOME":               "/home/bdgw",
					"TRACKER_URL":        "${GT_TRACKER_URL:-}",
					"TRACKER_TOKEN_FILE": "/run/bdgw/tokens/tracker",
				},
				"labels": map[string]any{"org.gastown.keep": "true"},
				"ports":  []any{"127.0.0.1:7440:7440"},
				"volumes": []any{
					"${GT_BD_TOKEN_DIR:?set GT_BD_TOKEN_DIR to the gateway token directory}:/run/bdgw/tokens:ro",
					"bd-logs:/var/log/bdgw",
				},
				"mem_limit": "1g",
				"cpus":      1,
				"healthcheck": map[string]any{
					"test": []any{"CMD", "/usr/local/bin/bdgw", "ready", "--tokens-dir", "/run/bdgw/tokens",
						"--verbs", "/etc/bdgw/verbs.json", "--workspace", "/var/lib/bdgw/ws"},
					"interval": "10s",
					"timeout":  "5s",
					"retries":  5,
				},
			},
		},
		"volumes": map[string]any{
			"dolt-var": nil,
			"bd-logs":  nil,
		},
	}
}

// decodeUnit parses the compose file into a generic document. A file that does
// not parse is refused before any comparison runs.
func decodeUnit(raw []byte) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", composeFile, err)
	}
	// The exact-document check compares one document; a second one in the file
	// would be read by nothing here and by compose as the file's own content, so
	// any further document, empty or not, is refused.
	var extra any
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, fmt.Errorf("%s holds more than one YAML document", composeFile)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%s is not valid YAML after its first document: %w", composeFile, err)
	}
	return doc, nil
}

// checkUnit returns every difference between the file and the expected document,
// or nil when the file matches it exactly.
func checkUnit(raw []byte) error {
	doc, err := decodeUnit(raw)
	if err != nil {
		return err
	}
	return checkDoc(doc)
}

func checkDoc(doc map[string]any) error {
	return diffError(diffDoc("", expectedUnit(), doc))
}

// checkSections compares each dotted path of the file with the same path of the
// expected document. A path absent from both is equal.
func checkSections(raw []byte, paths ...string) error {
	doc, err := decodeUnit(raw)
	if err != nil {
		return err
	}
	want := expectedUnit()
	var diffs []string
	for _, p := range paths {
		diffs = append(diffs, diffDoc(p, at(want, p), at(doc, p))...)
	}
	return diffError(diffs)
}

// at returns the value at a dotted path, or nil when the path is absent.
func at(doc any, path string) any {
	for _, k := range strings.Split(path, ".") {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		doc = m[k]
	}
	return doc
}

// diffDoc lists each place got differs from want, by dotted path. A key present
// in only one of the two is a difference, so an extra key fails by construction.
func diffDoc(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a mapping, got %v", path, got)}
		}
		keys := make([]string, 0, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
		}
		for k := range g {
			if _, dup := w[k]; !dup {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var diffs []string
		for _, k := range keys {
			wv, inWant := w[k]
			gv, inGot := g[k]
			p := joinPath(path, k)
			switch {
			case !inWant:
				diffs = append(diffs, fmt.Sprintf("%s: not in the expected document (got %v)", p, gv))
			case !inGot:
				diffs = append(diffs, fmt.Sprintf("%s: missing (want %v)", p, wv))
			default:
				diffs = append(diffs, diffDoc(p, wv, gv)...)
			}
		}
		return diffs
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %v, got %v", path, w, got)}
		}
		var diffs []string
		for i := range w {
			diffs = append(diffs, diffDoc(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return diffs
	default:
		if reflect.DeepEqual(want, got) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %v, got %v", path, want, got)}
	}
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func diffError(diffs []string) error {
	if len(diffs) == 0 {
		return nil
	}
	return errors.New(strings.Join(diffs, "\n"))
}

// readCompose returns the real compose file.
func readCompose(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	return raw
}

func TestComposeLimits(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t),
		"services.dolt.mem_limit", "services.dolt.memswap_limit",
		"services.bd.mem_limit", "services.bd.cpus")
	if err != nil {
		t.Errorf("%s limits:\n%v", composeFile, err)
	}
}

func TestComposeLoopbackPorts(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.dolt.ports", "services.bd.ports")
	if err != nil {
		t.Errorf("%s ports:\n%v", composeFile, err)
	}
}

func TestComposeHealthChecks(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.dolt.healthcheck", "services.bd.healthcheck")
	if err != nil {
		t.Errorf("%s health checks:\n%v", composeFile, err)
	}
}

func TestComposeKeepLabel(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.dolt.labels", "services.bd.labels")
	if err != nil {
		t.Errorf("%s keep label:\n%v", composeFile, err)
	}
}

// TestComposeNoSecrets compares the two places a literal secret can sit: the
// command arrays and the environment maps of both services.
func TestComposeNoSecrets(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t),
		"services.dolt.command", "services.dolt.environment",
		"services.bd.command", "services.bd.environment")
	if err != nil {
		t.Errorf("%s carries a secret or an unexpected argument:\n%v", composeFile, err)
	}
}

func TestComposeVolumes(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.dolt.volumes", "services.bd.volumes", "volumes")
	if err != nil {
		t.Errorf("%s volumes:\n%v", composeFile, err)
	}
}

func TestComposeDependsOn(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.bd.depends_on")
	if err != nil {
		t.Errorf("%s depends_on:\n%v", composeFile, err)
	}
}

// bdWorkspaceRoot is the store root the bd service starts the gateway on. The
// workspace store swaps the root directory by renaming a tree into place, so
// the root must sit under a parent the gateway user owns, not at a mount point.
const bdWorkspaceRoot = "/var/lib/bdgw/ws"

// stringList returns a decoded YAML string sequence as strings, refusing a
// value of another shape.
func stringList(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want a sequence of strings, got %T", v)
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("element %d is %T, want a string: %v", i, item, item)
		}
		out[i] = s
	}
	return out, nil
}

// workspaceFlag returns the value of argv's single --workspace flag.
func workspaceFlag(argv []string) (string, error) {
	var values []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--workspace" {
			values = append(values, argv[i+1])
		}
	}
	switch len(values) {
	case 0:
		return "", errors.New("no --workspace flag")
	case 1:
		return values[0], nil
	default:
		return "", fmt.Errorf("%d --workspace flags: %v", len(values), values)
	}
}

// bdWorkspacePaths returns the workspace path the bd service's command and its
// health check name.
func bdWorkspacePaths(doc map[string]any) (command, health string, err error) {
	bd := service(doc, "bd")
	cmd, err := stringList(bd["command"])
	if err != nil {
		return "", "", fmt.Errorf("the bd command: %w", err)
	}
	hc, ok := bd["healthcheck"].(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("the bd health check is not a mapping: %v", bd["healthcheck"])
	}
	test, err := stringList(hc["test"])
	if err != nil {
		return "", "", fmt.Errorf("the bd health check command: %w", err)
	}
	if command, err = workspaceFlag(cmd); err != nil {
		return "", "", fmt.Errorf("the bd command: %w", err)
	}
	if health, err = workspaceFlag(test); err != nil {
		return "", "", fmt.Errorf("the bd health check: %w", err)
	}
	return command, health, nil
}

// workspaceAgreement returns an error unless the bd service's command and its
// health check name the same workspace path, so readiness is judged against the
// tree the running gateway serves.
func workspaceAgreement(doc map[string]any) error {
	command, health, err := bdWorkspacePaths(doc)
	if err != nil {
		return err
	}
	if command != health {
		return fmt.Errorf("the bd command names --workspace %q, the health check names %q", command, health)
	}
	return nil
}

// volumeTarget returns the container path a compose volume entry mounts at. A
// ${...} source may hold colons in its default text, so the source ends at the
// first closing brace; a container path holds no colon, so any colon left on
// the target ends it and begins a mount mode.
func volumeTarget(spec string) string {
	rest := spec
	switch {
	case strings.HasPrefix(rest, "${"):
		if end := strings.Index(rest, "}"); end >= 0 {
			rest = strings.TrimPrefix(rest[end+1:], ":")
		}
	case strings.Contains(rest, ":"):
		rest = rest[strings.Index(rest, ":")+1:]
	}
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// bdMountTargets returns the container paths the bd service mounts volumes at.
func bdMountTargets(doc map[string]any) ([]string, error) {
	specs, err := stringList(service(doc, "bd")["volumes"])
	if err != nil {
		return nil, fmt.Errorf("the bd volumes: %w", err)
	}
	targets := make([]string, len(specs))
	for i, spec := range specs {
		targets[i] = volumeTarget(spec)
	}
	return targets, nil
}

// atOrUnder reports whether target is root itself or a path beneath it.
func atOrUnder(target, root string) bool {
	return target == root || strings.HasPrefix(target, root+"/")
}

// TestComposeWorkspaceLayout: the gateway runs on a store root its user owns and
// that no volume covers, so its swap can rename a tree into place.
func TestComposeWorkspaceLayout(t *testing.T) {
	t.Parallel()
	doc, err := decodeUnit(readCompose(t))
	if err != nil {
		t.Fatalf("%s: %v", composeFile, err)
	}
	command, health, err := bdWorkspacePaths(doc)
	if err != nil {
		t.Fatalf("%s: %v", composeFile, err)
	}
	if command != bdWorkspaceRoot {
		t.Errorf("the bd command's --workspace = %q, want %q", command, bdWorkspaceRoot)
	}
	if health != bdWorkspaceRoot {
		t.Errorf("the bd health check's --workspace = %q, want %q", health, bdWorkspaceRoot)
	}
	targets, err := bdMountTargets(doc)
	if err != nil {
		t.Fatalf("%s: %v", composeFile, err)
	}
	for _, target := range targets {
		if atOrUnder(target, "/ws") || atOrUnder(target, "/var/lib/bdgw") {
			t.Errorf("the bd service mounts a volume at %q; the store renames its root, so no volume may sit at or above it", target)
		}
	}
}

// TestComposeWorkspaceLayoutFlagsAgree: the command and the health check start
// the gateway on the same store root, and the comparison reports two paths that
// differ.
func TestComposeWorkspaceLayoutFlagsAgree(t *testing.T) {
	t.Parallel()
	raw := readCompose(t)
	doc, err := decodeUnit(raw)
	if err != nil {
		t.Fatalf("%s: %v", composeFile, err)
	}
	if err := workspaceAgreement(doc); err != nil {
		t.Errorf("%s: %v", composeFile, err)
	}
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, bd map[string]any)
	}{
		{
			name: "the health check names another path",
			set: func(t *testing.T, bd map[string]any) {
				hc, ok := bd["healthcheck"].(map[string]any)
				if !ok {
					t.Fatalf("the bd health check is not a mapping: %v", bd["healthcheck"])
				}
				swapWorkspaceFlag(t, hc["test"], "/var/lib/bdgw/other")
			},
		},
		{
			name: "the command names another path",
			set: func(t *testing.T, bd map[string]any) {
				swapWorkspaceFlag(t, bd["command"], "/ws")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := decodeUnit(raw)
			if err != nil {
				t.Fatalf("%s: %v", composeFile, err)
			}
			tc.set(t, service(doc, "bd"))
			if err := workspaceAgreement(doc); err == nil {
				t.Error("agreement accepted a command and a health check naming different workspace paths")
			}
		})
	}
}

// swapWorkspaceFlag replaces the value after each --workspace flag in a decoded
// YAML sequence.
func swapWorkspaceFlag(t *testing.T, seq any, value string) {
	t.Helper()
	list, ok := seq.([]any)
	if !ok {
		t.Fatalf("want a command sequence, got %T", seq)
	}
	swapped := false
	for i := 0; i+1 < len(list); i++ {
		if list[i] == "--workspace" {
			list[i+1] = value
			swapped = true
		}
	}
	if !swapped {
		t.Fatal("the sequence has no --workspace flag")
	}
}

// TestWorkspaceFlag: one --workspace flag supplies the path; no flag, a flag
// with no value after it, or two flags are refused.
func TestWorkspaceFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		argv    []string
		want    string
		wantErr bool
	}{
		{"one flag", []string{"--workspace", "/ws"}, "/ws", false},
		{"no flag", []string{"--tokens-dir", "/t"}, "", true},
		{"two flags", []string{"--workspace", "/a", "--workspace", "/b"}, "", true},
		{"a trailing flag with no value", []string{"--workspace"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := workspaceFlag(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("workspaceFlag(%q) error = %v, wantErr %v", tc.argv, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("workspaceFlag(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestStringList: a sequence of strings is returned; a value of another shape,
// or an element that is not a string, is refused.
func TestStringList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      any
		want    []string
		wantErr bool
	}{
		{"strings", []any{"a", "b"}, []string{"a", "b"}, false},
		{"not a sequence", "a", nil, true},
		{"a non-string element", []any{"a", 2}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := stringList(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("stringList(%v) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stringList(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVolumeTarget: the target is the container path, after a named or variable
// source and before a mount mode.
func TestVolumeTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, spec, want string }{
		{"a named volume", "bd-logs:/var/log/bdgw", "/var/log/bdgw"},
		{"a read-only variable source", "${GT_BD_TOKEN_DIR:?set GT_BD_TOKEN_DIR to the gateway token directory}:/run/bdgw/tokens:ro", "/run/bdgw/tokens"},
		{"a variable target", "${GT_DOLT_DATA:?set GT_DOLT_DATA to the .dolt-data directory}:${GT_DOLT_DATA}", "${GT_DOLT_DATA}"},
		{"an anonymous volume", "/data", "/data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := volumeTarget(tc.spec); got != tc.want {
				t.Errorf("volumeTarget(%q) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

// TestComposeBdImageVariable holds the bd image to the one documented variable,
// BD_IMAGE, required: never a literal, a tag, or a default that renders without it.
func TestComposeBdImageVariable(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.bd.image")
	if err != nil {
		t.Errorf("%s bd image:\n%v", composeFile, err)
	}
}

func TestComposeDoltImage(t *testing.T) {
	t.Parallel()
	err := checkSections(readCompose(t), "services.dolt.image")
	if err != nil {
		t.Errorf("%s dolt image:\n%v", composeFile, err)
	}
}

// TestComposeContract compares the whole file with the expected document.
func TestComposeContract(t *testing.T) {
	t.Parallel()
	if err := checkUnit(readCompose(t)); err != nil {
		t.Errorf("%s does not match the expected document:\n%v", composeFile, err)
	}
}

// service returns a named service of a decoded document. The caller has matched
// the document against the expected one first, so the shape holds.
func service(doc map[string]any, name string) map[string]any {
	return doc["services"].(map[string]any)[name].(map[string]any)
}

// holes are the ways a file could slip past a looser check. Each mutates a fresh
// decode of the real file; the comparison must reject it and name the path.
var holes = []struct {
	name   string
	path   string
	mutate func(doc map[string]any)
}{
	{
		name: "secret passed as a command flag",
		path: "services.dolt.command",
		mutate: func(d map[string]any) {
			cmd := service(d, "dolt")["command"].([]any)
			service(d, "dolt")["command"] = append(cmd, "--password=hunter2")
		},
	},
	{
		name: "bind-style top-level volume",
		path: "volumes.dolt-var",
		mutate: func(d map[string]any) {
			d["volumes"].(map[string]any)["dolt-var"] = map[string]any{
				"driver_opts": map[string]any{"type": "none", "o": "bind", "device": "/home/user/gt"},
			}
		},
	},
	{
		name: "network_mode host",
		path: "services.bd.network_mode",
		mutate: func(d map[string]any) {
			service(d, "bd")["network_mode"] = "host"
		},
	},
	{
		name: "privileged",
		path: "services.bd.privileged",
		mutate: func(d map[string]any) {
			service(d, "bd")["privileged"] = true
		},
	},
	{
		name: "escaped dollar in an environment value",
		path: "services.bd.environment.HOME",
		mutate: func(d map[string]any) {
			service(d, "bd")["environment"].(map[string]any)["HOME"] = "$$hunter2"
		},
	},
	{
		name: "service added",
		path: "services.extra",
		mutate: func(d map[string]any) {
			d["services"].(map[string]any)["extra"] = map[string]any{"image": "alpine"}
		},
	},
	{
		name: "bd image a literal tag",
		path: "services.bd.image",
		mutate: func(d map[string]any) {
			service(d, "bd")["image"] = "forgejo:3000/bot-registry/gt-bd:latest"
		},
	},
	{
		name: "bd image default renders unset",
		path: "services.bd.image",
		mutate: func(d map[string]any) {
			service(d, "bd")["image"] = "${BD_IMAGE:-forgejo:3000/bot-registry/gt-bd@sha256:0000}"
		},
	},
}

// TestComposeRefusals drives each hole through the comparison on a fresh decode
// of the real file. The unmutated file must match first.
func TestComposeRefusals(t *testing.T) {
	t.Parallel()
	raw := readCompose(t)
	if err := checkUnit(raw); err != nil {
		t.Fatalf("the file must match the expected document before any mutation: %v", err)
	}
	for _, tt := range holes {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc, err := decodeUnit(raw)
			if err != nil {
				t.Fatalf("decoding the real file: %v", err)
			}
			tt.mutate(doc)
			err = checkDoc(doc)
			if err == nil {
				t.Fatalf("mutated document matched the expected document; want a difference at %s", tt.path)
			}
			if !strings.Contains(err.Error(), tt.path+":") {
				t.Errorf("difference = %v; want it at %s", err, tt.path)
			}
		})
	}
}

// TestDecodeUnitRefusesASecondDocument: the whole file is the document, so a
// second YAML document after a separator, however small, is refused rather than
// left unread.
func TestDecodeUnitRefusesASecondDocument(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	if _, err := decodeUnit(raw); err != nil {
		t.Fatalf("the committed file is refused: %v", err)
	}
	for _, tc := range []struct{ name, tail string }{
		{"a service document", "\n---\nservices:\n  evil:\n    privileged: true\n"},
		{"an empty trailing document", "\n---\n"},
	} {
		_, err := decodeUnit(append(append([]byte{}, raw...), tc.tail...))
		if err == nil {
			t.Errorf("%s after the first document was accepted", tc.name)
		} else if !strings.Contains(err.Error(), "more than one YAML document") {
			t.Errorf("%s was refused for the wrong reason: %v", tc.name, err)
		}
	}
	if err := checkUnit(append(append([]byte{}, raw...), "\n---\nservices: {}\n"...)); err == nil {
		t.Error("checkUnit accepted a file with a second document")
	}
}
