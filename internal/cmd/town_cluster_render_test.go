package cmd

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/manifestdata"
	"github.com/steveyegge/gastown/internal/templates"
)

// clusterRenderSecretsManifest is the secrets manifest the render reads from the
// repo. Its files are the config entries the live pod mounts.
const clusterRenderSecretsManifest = `files:
  - name: om-daemon-token
    consumer: "the om token the daemon passes to om"
    mode: "0600"
    required: true

  - name: forgejo-landing.env
    consumer: "the landing bot's token"
    mode: "0600"
    required: true

  - name: promote-gastown.key
    consumer: "forgejo.promote_key_file in gastown's settings"
    mode: "0600"
    required: false

  - name: forgejo-viewer.env
    consumer: "the read-only Forgejo token"
    mode: "0600"
    required: false
`

const clusterRenderDaemonDigest = "registry.example.test/gastown/daemon@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000001"

// clusterRenderFixture writes a repo directory with a values file and a secrets
// manifest, and returns the flags of a live render of it. The config directory
// is absent, so a render that opened a secret file would fail.
func clusterRenderFixture(t *testing.T) clusterRenderFlags {
	t.Helper()
	dir := t.TempDir()
	if err := imagevalues.Write(imagevalues.PathIn(dir), imagevalues.Values{
		Daemon:  clusterRenderDaemonDigest,
		Polecat: clusterRenderDaemonDigest,
		Commit:  strings.Repeat("a", 40),
	}); err != nil {
		t.Fatalf("writing the values file: %v", err)
	}
	writeClusterRenderManifest(t, dir, clusterRenderSecretsManifest)
	return clusterRenderFlags{
		dir:                dir,
		out:                filepath.Join(t.TempDir(), "out"),
		cpu:                "2",
		memory:             "4Gi",
		confirmStopped:     true,
		forgeHost:          "forgejo:3000",
		configDir:          filepath.Join(t.TempDir(), "absent-config"),
		openShellConfigDir: filepath.Join(t.TempDir(), "absent-openshell"),
	}
}

// writeClusterRenderManifest writes the secrets manifest under the repo dir.
func writeClusterRenderManifest(t *testing.T, dir, text string) {
	t.Helper()
	manifest := filepath.Join(dir, secretsManifestRelPath)
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatalf("making the manifest directory: %v", err)
	}
	if err := os.WriteFile(manifest, []byte(text), 0o600); err != nil {
		t.Fatalf("writing the secrets manifest: %v", err)
	}
}

// clusterRenderTestTown is the host's town for a render: a temporary root with
// the derived hm config the compose write leaves, which a live render requires.
func clusterRenderTestTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	derived := filepath.Join(town, filepath.FromSlash(templates.ContainerHMConfigRP))
	if err := os.MkdirAll(filepath.Dir(derived), 0o755); err != nil {
		t.Fatalf("making the derived hm config directory: %v", err)
	}
	if err := os.WriteFile(derived, []byte(`{"database_url": "postgres://host.docker.internal:5433/hm"}`), 0o600); err != nil {
		t.Fatalf("writing the derived hm config: %v", err)
	}
	return town
}

// clusterRenderTestDeps stands in the host: a town with the derived hm config, an
// empty daemon.env, a fixed home, and the real build and write steps against the
// temporary repo.
func clusterRenderTestDeps(t *testing.T, out io.Writer) clusterRenderDeps {
	t.Helper()
	return clusterRenderDepsAt(t, clusterRenderTestTown(t), out)
}

// clusterRenderDepsAt is clusterRenderTestDeps in the town it names, so a test can
// render twice in one town: the first run recording its flags, the second
// replaying them.
func clusterRenderDepsAt(t *testing.T, town string, out io.Writer) clusterRenderDeps {
	t.Helper()
	return clusterRenderDeps{
		findTown:                 func() (string, error) { return town, nil },
		containerEnv:             func(string) (map[string]string, error) { return map[string]string{}, nil },
		readFile:                 os.ReadFile,
		lstat:                    os.Lstat,
		getenv:                   func(string) string { return "" },
		userHome:                 func() (string, error) { return "/fake/home", nil },
		build:                    k8sdaemon.Build,
		render:                   k8sdaemon.Render,
		buildHelper:              k8sdaemon.BuildHelper,
		buildTrackerBackupReader: k8sdaemon.BuildTrackerBackupReader,
		encode:                   k8sdaemon.YAML,
		mkdirAll:                 func(path string) error { return os.MkdirAll(path, 0o755) },
		writeFile:                func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		saveRecord:               saveRenderFlags,
		out:                      out,
	}
}

// dirNames returns the names of the entries of dir in name order; a missing
// directory has none.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// outState describes what is at dir: absent, not a directory, or the names it
// holds. A refused render must leave it as it was.
func outState(dir string) string {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	case !info.IsDir():
		return "a file"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return "directory holding " + strings.Join(names, ",")
}

// renderedKind is the kind a rendered file declares.
func renderedKind(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		t.Fatalf("%s does not parse back: %v", path, err)
	}
	return head.Kind
}

// renderedKinds is the kind of each file in out, in file-name order.
func renderedKinds(t *testing.T, out string) []string {
	t.Helper()
	var kinds []string
	for _, name := range dirNames(t, out) {
		kinds = append(kinds, renderedKind(t, filepath.Join(out, name)))
	}
	return kinds
}

// deploymentBody is the rendered daemon Deployment's text, for a test that reads
// a mount path or a volume name the typed helpers do not expose.
func deploymentBody(t *testing.T, out string) string {
	t.Helper()
	for _, name := range dirNames(t, out) {
		if !strings.HasSuffix(name, "-deployment-daemon.yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading the Deployment: %v", err)
		}
		return string(data)
	}
	t.Fatalf("no Deployment file in %s", out)
	return ""
}

// deploymentSecretVolumes is the Secret each volume of the rendered Deployment
// mounts, in the Deployment's order.
func deploymentSecretVolumes(t *testing.T, out string) []string {
	t.Helper()
	for _, name := range dirNames(t, out) {
		if !strings.HasSuffix(name, "-deployment-daemon.yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading the Deployment: %v", err)
		}
		var dep struct {
			Spec struct {
				Template struct {
					Spec struct {
						Volumes []struct {
							Secret *struct {
								SecretName string `yaml:"secretName"`
							} `yaml:"secret"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &dep); err != nil {
			t.Fatalf("the Deployment does not parse back: %v", err)
		}
		var names []string
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Secret != nil {
				names = append(names, v.Secret.SecretName)
			}
		}
		return names
	}
	t.Fatalf("no Deployment file in %s", out)
	return nil
}

// deploymentAnnotations is the pod template's annotations in the rendered
// Deployment, or none when the Deployment carries none.
func deploymentAnnotations(t *testing.T, out string) map[string]string {
	t.Helper()
	for _, name := range dirNames(t, out) {
		if !strings.HasSuffix(name, "-deployment-daemon.yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading the Deployment: %v", err)
		}
		var dep struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Annotations map[string]string `yaml:"annotations"`
					} `yaml:"metadata"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &dep); err != nil {
			t.Fatalf("the Deployment does not parse back: %v", err)
		}
		return dep.Spec.Template.Metadata.Annotations
	}
	t.Fatalf("no Deployment file in %s", out)
	return nil
}

// createLines returns the create-secret lines the render printed. The pod's
// town-copy line is a kubectl line too, so it is not one of these.
func createLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "kubectl ") && strings.Contains(line, " create secret generic ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestTownClusterRenderWritesTheObjectsInApplyOrder(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	want := []string{
		"Namespace",
		"PersistentVolumeClaim", "PersistentVolumeClaim", "PersistentVolumeClaim",
		"ServiceAccount",
		"Service",
		"RoleBinding", "RoleBinding", "ClusterRoleBinding",
		"Deployment",
	}
	if got := renderedKinds(t, f.out); !slices.Equal(got, want) {
		t.Fatalf("apply order = %v, want %v", got, want)
	}
}

func TestTownClusterRenderPrintsOneCreateLinePerMountedSecret(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	var out bytes.Buffer
	// The config and openshell directories are absent: a render that opened a
	// secret file would fail here, so success proves none is opened.
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	mounted := deploymentSecretVolumes(t, f.out)
	lines := createLines(out.String())
	if len(lines) != len(mounted) {
		t.Fatalf("printed %d create lines for %d mounted Secrets:\n%s", len(lines), len(mounted), out.String())
	}
	for _, name := range mounted {
		var matches []string
		for _, line := range lines {
			if strings.HasPrefix(line, "kubectl -n gt create secret generic "+name+" ") {
				matches = append(matches, line)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("Secret %s has %d create lines, want exactly 1", name, len(matches))
		}
	}
	// A config entry names its source file in the config directory.
	want := "--from-file=forgejo-landing.env=" + f.configDir + "/forgejo-landing.env"
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, want) }) {
		t.Fatalf("no create line carries %q:\n%s", want, out.String())
	}
	// The openshell tree names each of its files under the openshell directory.
	want = "--from-file=gateways__k8s__mtls__tls.key=" + f.openShellConfigDir + "/gateways/k8s/mtls/tls.key"
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, want) }) {
		t.Fatalf("no create line carries %q:\n%s", want, out.String())
	}
	if !strings.Contains(out.String(), clusterRenderDaemonDigest) {
		t.Fatalf("the output does not name the daemon digest used:\n%s", out.String())
	}
}

// TestTownClusterRenderMountsTheCloudCheckKeyOnlyWhenPresent: the cloud_check
// patrol runs without its key and says so once, and a pod that mounts a Secret
// the operator never created stays in ContainerCreating, so the render mounts
// gt-cloud-check-gcp and prints its create line only when the config directory
// holds the file (gt-c57pax).
func TestTownClusterRenderMountsTheCloudCheckKeyOnlyWhenPresent(t *testing.T) {
	t.Parallel()

	render := func(t *testing.T, withKey bool) (body, configDir string, lines []string) {
		t.Helper()
		f := clusterRenderFixture(t)
		f.configDir = t.TempDir()
		if withKey {
			if err := os.WriteFile(filepath.Join(f.configDir, k8sdaemon.CloudCheckKeyFile), []byte("{}"), 0o600); err != nil {
				t.Fatalf("writing the key file: %v", err)
			}
		}
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		return deploymentBody(t, f.out), f.configDir, createLines(out.String())
	}

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		body, configDir, lines := render(t, true)
		for _, want := range []string{
			"secretName: " + k8sdaemon.CloudCheckKeySecretName,
			"mountPath: " + k8sdaemon.CloudCheckKeyPath,
			"subPath: " + k8sdaemon.CloudCheckKeyKey,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the Deployment does not carry %q:\n%s", want, body)
			}
		}
		want := "kubectl -n gt create secret generic " + k8sdaemon.CloudCheckKeySecretName +
			" --from-file=" + k8sdaemon.CloudCheckKeyKey + "=" + filepath.Join(configDir, k8sdaemon.CloudCheckKeyFile)
		if !slices.Contains(lines, want) {
			t.Errorf("create lines do not carry %q:\n%s", want, strings.Join(lines, "\n"))
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		body, _, lines := render(t, false)
		if strings.Contains(body, k8sdaemon.CloudCheckKeySecretName) {
			t.Errorf("the Deployment names %s with no key file in the config directory", k8sdaemon.CloudCheckKeySecretName)
		}
		for _, line := range lines {
			if strings.Contains(line, k8sdaemon.CloudCheckKeySecretName) {
				t.Errorf("a create line names %s with no key file in the config directory: %s", k8sdaemon.CloudCheckKeySecretName, line)
			}
		}
	})

	t.Run("an unreadable path refuses before a write", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		var out bytes.Buffer
		deps := clusterRenderTestDeps(t, &out)
		deps.lstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, k8sdaemon.CloudCheckKeyFile, "cannot be read")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})
}

// TestTownClusterRenderMountsTheDeepSeekBalanceKeyOnlyWhenPresent: the
// deepseek_balance patrol reads its key from the file the gt-deepseek-balance
// Secret mounts, and a town with no key runs unconfigured (one log line, no
// fetch). The render mounts the Secret and prints its create line only when the
// operator's config directory holds the deepseek.env file, and it never opens
// that file, so the key cannot reach a manifest or the output (gt-hfcssw).
func TestTownClusterRenderMountsTheDeepSeekBalanceKeyOnlyWhenPresent(t *testing.T) {
	t.Parallel()

	render := func(t *testing.T, withKey bool) (body, configDir string, lines []string, output string) {
		t.Helper()
		f := clusterRenderFixture(t)
		f.configDir = t.TempDir()
		keyPath := filepath.Join(f.configDir, k8sdaemon.DeepSeekBalanceFile)
		if withKey {
			// A value no manifest or line may carry.
			if err := os.WriteFile(keyPath, []byte("DEEPSEEK_API_KEY=sk-fixture-1f2e\n"), 0o600); err != nil {
				t.Fatalf("writing the key file: %v", err)
			}
		}
		var out bytes.Buffer
		deps := clusterRenderTestDeps(t, &out)
		deps.readFile = func(path string) ([]byte, error) {
			if path == keyPath {
				t.Errorf("the render read the key file %s, and no agent reads a secret", path)
			}
			return os.ReadFile(path)
		}
		if err := runClusterRender(deps, f); err != nil {
			t.Fatalf("render: %v", err)
		}
		return deploymentBody(t, f.out), f.configDir, createLines(out.String()), out.String()
	}

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		body, configDir, lines, output := render(t, true)
		for _, want := range []string{
			"secretName: " + k8sdaemon.DeepSeekBalanceSecretName,
			"mountPath: " + k8sdaemon.DeepSeekBalancePath,
			"subPath: " + k8sdaemon.DeepSeekBalanceKey,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the Deployment does not carry %q:\n%s", want, body)
			}
		}
		want := "kubectl -n gt create secret generic " + k8sdaemon.DeepSeekBalanceSecretName +
			" --from-file=" + k8sdaemon.DeepSeekBalanceKey + "=" + filepath.Join(configDir, k8sdaemon.DeepSeekBalanceFile)
		if !slices.Contains(lines, want) {
			t.Errorf("create lines do not carry %q:\n%s", want, strings.Join(lines, "\n"))
		}
		deepSeek := 0
		for _, line := range lines {
			if strings.Contains(line, k8sdaemon.DeepSeekBalanceSecretName) {
				deepSeek++
			}
		}
		if deepSeek != 1 {
			t.Errorf("the render printed %d create lines for %s, want one:\n%s", deepSeek, k8sdaemon.DeepSeekBalanceSecretName, strings.Join(lines, "\n"))
		}
		if strings.Contains(body, "sk-fixture-1f2e") || strings.Contains(output, "sk-fixture-1f2e") {
			t.Error("the key file's contents reached the Deployment or the render's output")
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		body, _, lines, _ := render(t, false)
		if strings.Contains(body, k8sdaemon.DeepSeekBalanceSecretName) {
			t.Errorf("the Deployment names %s with no key file in the config directory", k8sdaemon.DeepSeekBalanceSecretName)
		}
		for _, line := range lines {
			if strings.Contains(line, k8sdaemon.DeepSeekBalanceSecretName) {
				t.Errorf("a create line names %s with no key file in the config directory: %s", k8sdaemon.DeepSeekBalanceSecretName, line)
			}
		}
	})

	t.Run("an unreadable path refuses before a write", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		keyPath := filepath.Join(f.configDir, k8sdaemon.DeepSeekBalanceFile)
		var out bytes.Buffer
		deps := clusterRenderTestDeps(t, &out)
		deps.lstat = func(path string) (fs.FileInfo, error) {
			if path == keyPath {
				return nil, fs.ErrPermission
			}
			return os.Lstat(path)
		}
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, k8sdaemon.DeepSeekBalanceFile, "cannot be inspected")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})

	t.Run("a directory is not a key file", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		keyPath := filepath.Join(f.configDir, k8sdaemon.DeepSeekBalanceFile)
		if err := os.MkdirAll(keyPath, 0o700); err != nil {
			t.Fatalf("making the key path a directory: %v", err)
		}
		var out bytes.Buffer
		deps := clusterRenderTestDeps(t, &out)
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, k8sdaemon.DeepSeekBalanceFile, "not a regular file")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})
}

// TestTownClusterRenderRehearsalIgnoresAnUnreadableDeepSeekKeyFile: a rehearsal
// never mounts the DeepSeek balance key, so an unreadable key file cannot change
// its manifest and the render must not refuse on it (gt-hfcssw).
func TestTownClusterRenderRehearsalIgnoresAnUnreadableDeepSeekKeyFile(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.rehearsal = true
	f.forgeHost = ""
	f.confirmStopped = false
	f.namespace = "gt-rehearsal"
	keyPath := filepath.Join(f.configDir, k8sdaemon.DeepSeekBalanceFile)
	var out bytes.Buffer
	deps := clusterRenderTestDeps(t, &out)
	deps.lstat = func(path string) (fs.FileInfo, error) {
		if path == keyPath {
			return nil, fs.ErrPermission
		}
		return os.Lstat(path)
	}
	if err := runClusterRender(deps, f); err != nil {
		t.Fatalf("a rehearsal refused on the deepseek balance key file it never mounts: %v", err)
	}
	if body := deploymentBody(t, f.out); strings.Contains(body, k8sdaemon.DeepSeekBalanceSecretName) {
		t.Errorf("the rehearsal names %s:\n%s", k8sdaemon.DeepSeekBalanceSecretName, body)
	}
	for _, line := range createLines(out.String()) {
		if strings.Contains(line, k8sdaemon.DeepSeekBalanceSecretName) {
			t.Errorf("a rehearsal create line names %s: %s", k8sdaemon.DeepSeekBalanceSecretName, line)
		}
	}
}

// TestTownClusterRenderPinsTheDeepSeekKeyPath: the mount path the render writes
// is the path the patrol reads when the town names no key_file, so the mount
// alone configures a pod (gt-hfcssw).
func TestTownClusterRenderPinsTheDeepSeekKeyPath(t *testing.T) {
	t.Parallel()
	if k8sdaemon.DeepSeekBalancePath != config.DeepSeekBalanceDefaultKeyFile {
		t.Errorf("the pod mounts the key at %s, but the patrol reads %s by default", k8sdaemon.DeepSeekBalancePath, config.DeepSeekBalanceDefaultKeyFile)
	}
}

// TestTownClusterRenderMountsTheHMConfigOnlyWhenDerived: hm reads its database
// and its embedder and reranker from the derived hm config, so a live render
// mounts the gt-hm-config Secret, sets HM_CONFIG to it, and prints the create
// line that fills the Secret from the derived file under the town root. A live
// render with no derived file refuses before a write, since the pod's hm would
// read its defaults, which name the pod's own loopback; a rehearsal mounts none.
// The manifest never carries the content (gt-7vrjuo, gt-fg6ami).
func TestTownClusterRenderMountsTheHMConfigOnlyWhenDerived(t *testing.T) {
	t.Parallel()
	derivedPath := func(town string) string {
		return filepath.Join(town, filepath.FromSlash(templates.ContainerHMConfigRP))
	}
	// writeDerived leaves the config `gt daemon compose write` derives under town.
	writeDerived := func(t *testing.T, town string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(derivedPath(town)), 0o755); err != nil {
			t.Fatalf("making the derived config directory: %v", err)
		}
		if err := os.WriteFile(derivedPath(town), []byte(`{"database_url": "postgres://host.docker.internal:5433/hm"}`), 0o600); err != nil {
			t.Fatalf("writing the derived config: %v", err)
		}
	}
	render := func(t *testing.T, rehearsal bool) (body, town, out string, lines []string) {
		t.Helper()
		f := clusterRenderFixture(t)
		town = t.TempDir()
		writeDerived(t, town)
		if rehearsal {
			f.rehearsal = true
			f.forgeHost = ""
			f.confirmStopped = false
			f.namespace = "gt-rehearsal"
		}
		var buf bytes.Buffer
		deps := clusterRenderTestDeps(t, &buf)
		deps.findTown = func() (string, error) { return town, nil }
		if err := runClusterRender(deps, f); err != nil {
			t.Fatalf("render: %v", err)
		}
		return deploymentBody(t, f.out), town, buf.String(), createLines(buf.String())
	}

	t.Run("derived", func(t *testing.T) {
		t.Parallel()
		body, town, out, lines := render(t, false)
		for _, want := range []string{
			"secretName: " + k8sdaemon.HMConfigSecretName,
			"mountPath: " + k8sdaemon.HMConfigPath,
			"subPath: " + k8sdaemon.HMConfigKey,
			"value: " + k8sdaemon.HMConfigPath,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the Deployment does not carry %q:\n%s", want, body)
			}
		}
		want := "kubectl -n gt create secret generic " + k8sdaemon.HMConfigSecretName +
			" --from-file=" + k8sdaemon.HMConfigKey + "=" + shellQuote(derivedPath(town))
		if !slices.Contains(lines, want) {
			t.Errorf("create lines do not carry %q:\n%s", want, strings.Join(lines, "\n"))
		}
		if strings.Contains(out, "derived hm config") {
			t.Errorf("the render reports a derived hm config problem though one exists:\n%s", out)
		}
	})

	t.Run("absent refuses before a write", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		town := t.TempDir()
		var buf bytes.Buffer
		deps := clusterRenderTestDeps(t, &buf)
		deps.findTown = func() (string, error) { return town, nil }
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, "derived hm config", derivedPath(town), "gt daemon compose write", "no file was written")
		if buf.Len() != 0 {
			t.Errorf("the refused render printed %q", buf.String())
		}
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})

	t.Run("a rehearsal mounts none", func(t *testing.T) {
		t.Parallel()
		body, _, _, lines := render(t, true)
		if strings.Contains(body, k8sdaemon.HMConfigSecretName) || strings.Contains(body, "HM_CONFIG") {
			t.Errorf("the rehearsal names the hm config:\n%s", body)
		}
		for _, line := range lines {
			if strings.Contains(line, k8sdaemon.HMConfigSecretName) {
				t.Errorf("the rehearsal prints a create line for %s: %s", k8sdaemon.HMConfigSecretName, line)
			}
		}
	})

	t.Run("an unreadable path refuses before a write", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		town := t.TempDir()
		var buf bytes.Buffer
		deps := clusterRenderTestDeps(t, &buf)
		deps.findTown = func() (string, error) { return town, nil }
		deps.lstat = func(path string) (fs.FileInfo, error) {
			if strings.HasSuffix(path, "hm-config.json") {
				return nil, fs.ErrPermission
			}
			return os.Lstat(path)
		}
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, "derived hm config", "cannot be read")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})

	t.Run("the source path is the one compose write derives", func(t *testing.T) {
		t.Parallel()
		if k8sdaemon.HMConfigSource != templates.ContainerHMConfigRP {
			t.Errorf("HMConfigSource = %q, want the compose write's %q", k8sdaemon.HMConfigSource, templates.ContainerHMConfigRP)
		}
	})
}

// errClusterRenderBuildStopped is what the recording build step of
// TestTownClusterRenderValuesMatchWhatTheRenderBuilds returns, so the render stops
// after the Values it hands over and before any write.
var errClusterRenderBuildStopped = errors.New("the test stopped the render at its build step")

// clusterRenderBaseValues is the Values a live render of the fixture builds
// before any flag this table sets: the fixture's share and forge host, the
// parsed secrets manifest, the town's empty environment and the derived hm
// config the test town holds.
func clusterRenderBaseValues(t *testing.T) k8sdaemon.Values {
	t.Helper()
	manifest, err := manifestdata.ParseSecretsManifest([]byte(clusterRenderSecretsManifest))
	if err != nil {
		t.Fatalf("parsing the fixture secrets manifest: %v", err)
	}
	return k8sdaemon.Values{
		Secrets:                 manifest.Files,
		Image:                   clusterRenderDaemonDigest,
		Env:                     map[string]string{},
		CPU:                     "2",
		Memory:                  "4Gi",
		ConfirmContainerStopped: true,
		OMToken:                 true,
		HMConfig:                true,
		ForgeHost:               "forgejo:3000",
	}
}

// TestTownClusterRenderValuesMatchWhatTheRenderBuilds holds the Values the render
// hands its build step to the flag sets that change them: the dashboard, the
// tracker, the daemon's bd gateway and a rehearsal for the live-only fields.
// clusterRenderValues returns the same Values and image pair the render passes
// on, and the town root the render's Secret lines and values file are built
// against, so the two cannot drift while both are built.
func TestTownClusterRenderValuesMatchWhatTheRenderBuilds(t *testing.T) {
	t.Parallel()
	trackerImage := "registry.example.test/gastown/tracker-backup@sha256:" + strings.Repeat("2", 64)
	tests := []struct {
		name   string
		mutate func(*clusterRenderFlags)
		want   func(base k8sdaemon.Values) k8sdaemon.Values
	}{
		{
			name:   "a live render",
			mutate: func(*clusterRenderFlags) {},
			want:   func(base k8sdaemon.Values) k8sdaemon.Values { return base },
		},
		{
			name: "the dashboard and its bd gateway",
			mutate: func(f *clusterRenderFlags) {
				f.dashboard = true
				f.dashboardClaudeDir = "/Users/user/.claude"
				f.bdGatewayURL = " https://bd.example.test:8443 "
			},
			want: func(base k8sdaemon.Values) k8sdaemon.Values {
				base.Dashboard = true
				base.DashboardClaudeDir = "/Users/user/.claude"
				base.BdGatewayURL = "https://bd.example.test:8443"
				return base
			},
		},
		{
			name: "the tracker",
			mutate: func(f *clusterRenderFlags) {
				f.tracker = true
				f.trackerDSNSecret = " tracker-dsn-secret "
				f.proxyImage = clusterRenderDaemonDigest
			},
			want: func(base k8sdaemon.Values) k8sdaemon.Values {
				base.Tracker = true
				base.TrackerDSNSecret = "tracker-dsn-secret"
				base.ProxyImage = clusterRenderDaemonDigest
				return base
			},
		},
		{
			name: "the tracker's nightly dump",
			mutate: func(f *clusterRenderFlags) {
				f.tracker = true
				f.trackerDSNSecret = "tracker-dsn-secret"
				f.proxyImage = clusterRenderDaemonDigest
				f.trackerBackup = true
				f.trackerBackupImage = " " + trackerImage + " "
			},
			want: func(base k8sdaemon.Values) k8sdaemon.Values {
				base.Tracker = true
				base.TrackerDSNSecret = "tracker-dsn-secret"
				base.ProxyImage = clusterRenderDaemonDigest
				base.TrackerBackup = true
				base.TrackerBackupImage = trackerImage
				return base
			},
		},
		{
			name: "the daemon's bd gateway",
			mutate: func(f *clusterRenderFlags) {
				f.daemonBdGatewayURL = " https://bd.example.test:8443 "
				f.daemonBdGatewayURLSet = true
			},
			want: func(base k8sdaemon.Values) k8sdaemon.Values {
				base.DaemonBdGatewayURL = "https://bd.example.test:8443"
				return base
			},
		},
		{
			name: "a rehearsal carries no live-only field",
			mutate: func(f *clusterRenderFlags) {
				f.rehearsal = true
				f.forgeHost = ""
				f.confirmStopped = false
				f.namespace = "gt-rehearsal"
			},
			want: func(base k8sdaemon.Values) k8sdaemon.Values {
				base.Rehearsal = true
				base.Namespace = "gt-rehearsal"
				base.ConfirmContainerStopped = false
				base.OMToken = false
				base.HMConfig = false
				base.ForgeHost = ""
				return base
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			tt.mutate(&f)
			town := clusterRenderTestTown(t)
			d := clusterRenderDepsAt(t, town, io.Discard)
			var built k8sdaemon.Values
			d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				built = v
				return k8sdaemon.Manifests{}, errClusterRenderBuildStopped
			}
			if err := runClusterRender(d, f); !errors.Is(err, errClusterRenderBuildStopped) {
				t.Fatalf("render = %v, want it to reach the build step and stop there", err)
			}
			want := tt.want(clusterRenderBaseValues(t))
			if !reflect.DeepEqual(built, want) {
				t.Fatalf("the render built %+v, want %+v", built, want)
			}
			got, pair, gotTown, err := clusterRenderValues(d, f, func() (string, error) { return town, nil })
			if err != nil {
				t.Fatalf("clusterRenderValues: %v", err)
			}
			if !reflect.DeepEqual(got, built) {
				t.Errorf("clusterRenderValues returned %+v, but the render built %+v for the same flags", got, built)
			}
			if gotTown != town {
				t.Errorf("clusterRenderValues returned the town root %q, want the %q it read", gotTown, town)
			}
			wantPair := imagevalues.Values{
				Daemon:  clusterRenderDaemonDigest,
				Polecat: clusterRenderDaemonDigest,
				Commit:  strings.Repeat("a", 40),
			}
			if !reflect.DeepEqual(pair, wantPair) {
				t.Errorf("clusterRenderValues returned the pair %+v, want %+v", pair, wantPair)
			}
		})
	}
}

// TestTownClusterRenderValuesRefuseAnUnreadableInput drives each refusal
// clusterRenderValues makes on its own: an image pair, town, environment or
// manifest it cannot read, a manifest it refuses, a dashboard without its
// viewer entry, and a cloud-check key or hm config whose presence it cannot
// answer. Each returns the error and no Values, no pair and no town root.
func TestTownClusterRenderValuesRefuseAnUnreadableInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		dashboard bool
		mutate    func(t *testing.T, f *clusterRenderFlags, d *clusterRenderDeps, town string)
		cause     string
	}{
		{
			name: "the image values do not parse",
			mutate: func(t *testing.T, f *clusterRenderFlags, _ *clusterRenderDeps, _ string) {
				if err := os.WriteFile(imagevalues.PathIn(f.dir), []byte("daemon: [\n"), 0o600); err != nil {
					t.Fatalf("writing a corrupt values file: %v", err)
				}
			},
			cause: "reading the image values",
		},
		{
			name: "the repo holds no image pair",
			mutate: func(t *testing.T, f *clusterRenderFlags, _ *clusterRenderDeps, _ string) {
				if err := os.Remove(imagevalues.PathIn(f.dir)); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
			},
			cause: "holds no image pair",
		},
		{
			name: "the town cannot be found",
			mutate: func(_ *testing.T, _ *clusterRenderFlags, d *clusterRenderDeps, _ string) {
				d.findTown = failing("no town")
			},
			cause: "no town",
		},
		{
			name: "the town's daemon.env cannot be read",
			mutate: func(_ *testing.T, _ *clusterRenderFlags, d *clusterRenderDeps, _ string) {
				d.containerEnv = func(string) (map[string]string, error) { return nil, errors.New("unreadable") }
			},
			cause: "reading the town's daemon.env",
		},
		{
			name: "the secrets manifest cannot be read",
			mutate: func(_ *testing.T, f *clusterRenderFlags, d *clusterRenderDeps, _ string) {
				manifest := filepath.Join(f.dir, secretsManifestRelPath)
				d.readFile = func(path string) ([]byte, error) {
					if path == manifest {
						return nil, errors.New("unreadable")
					}
					return os.ReadFile(path)
				}
			},
			cause: "reading the secrets manifest",
		},
		{
			name: "the secrets manifest is refused",
			mutate: func(t *testing.T, f *clusterRenderFlags, _ *clusterRenderDeps, _ string) {
				writeClusterRenderManifest(t, f.dir, "files:\n  - name: forgejo-landing.env\n    consumer: \"x\"\n    mode: \"0644\"\n    required: true\n")
			},
			cause: "is refused",
		},
		{
			name:      "the dashboard has no viewer token entry",
			dashboard: true,
			mutate: func(t *testing.T, f *clusterRenderFlags, _ *clusterRenderDeps, _ string) {
				writeClusterRenderManifest(t, f.dir, "files:\n  - name: om-daemon-token\n    consumer: \"x\"\n    mode: \"0600\"\n    required: true\n")
			},
			cause: "needs the forgejo-viewer.env entry",
		},
		{
			name: "the cloud check's key file cannot be read",
			mutate: func(_ *testing.T, f *clusterRenderFlags, d *clusterRenderDeps, _ string) {
				key := filepath.Join(f.configDir, k8sdaemon.CloudCheckKeyFile)
				d.lstat = func(path string) (fs.FileInfo, error) {
					if path == key {
						return nil, errors.New("permission denied")
					}
					return os.Lstat(path)
				}
			},
			cause: "the cloud check's key file",
		},
		{
			name: "the deepseek balance key file cannot be read",
			mutate: func(_ *testing.T, f *clusterRenderFlags, d *clusterRenderDeps, _ string) {
				key := filepath.Join(f.configDir, k8sdaemon.DeepSeekBalanceFile)
				d.lstat = func(path string) (fs.FileInfo, error) {
					if path == key {
						return nil, errors.New("permission denied")
					}
					return os.Lstat(path)
				}
			},
			cause: "the deepseek balance key file",
		},
		{
			name: "the derived hm config cannot be read",
			mutate: func(_ *testing.T, _ *clusterRenderFlags, d *clusterRenderDeps, town string) {
				derived := filepath.Join(town, filepath.FromSlash(k8sdaemon.HMConfigSource))
				d.lstat = func(path string) (fs.FileInfo, error) {
					if path == derived {
						return nil, errors.New("permission denied")
					}
					return os.Lstat(path)
				}
			},
			cause: "the derived hm config",
		},
		{
			name: "the derived hm config is absent",
			mutate: func(t *testing.T, _ *clusterRenderFlags, _ *clusterRenderDeps, town string) {
				derived := filepath.Join(town, filepath.FromSlash(k8sdaemon.HMConfigSource))
				if err := os.Remove(derived); err != nil {
					t.Fatalf("removing the derived hm config: %v", err)
				}
			},
			cause: "is absent at",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.dashboard = tt.dashboard
			town := clusterRenderTestTown(t)
			d := clusterRenderDepsAt(t, town, io.Discard)
			tt.mutate(t, &f, &d, town)
			v, pair, root, err := clusterRenderValues(d, f, d.findTown)
			if err == nil {
				t.Fatalf("clusterRenderValues succeeded, want a refusal naming %q", tt.cause)
			}
			if !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("refusal = %q, want it to name %q", err, tt.cause)
			}
			if !reflect.DeepEqual(v, k8sdaemon.Values{}) || !reflect.DeepEqual(pair, imagevalues.Values{}) || root != "" {
				t.Errorf("a refused read returned Values %+v, pair %+v and root %q, want none", v, pair, root)
			}
		})
	}
}

func TestTownClusterRenderRefusesWithoutADigestOrAShare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, f *clusterRenderFlags)
		cause  string
	}{
		{
			name: "missing values file",
			mutate: func(t *testing.T, f *clusterRenderFlags) {
				if err := os.Remove(imagevalues.PathIn(f.dir)); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
			},
			cause: "holds no image pair",
		},
		{
			name:   "missing cpu",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.cpu = "" },
			cause:  "--cpu is required",
		},
		{
			name:   "live render without a forge host",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.forgeHost = "  " },
			cause:  "--forge-host is required for a live render",
		},
		{
			name: "rehearsal with a forge host",
			mutate: func(_ *testing.T, f *clusterRenderFlags) {
				f.rehearsal = true
				f.namespace = "gt-rehearsal"
			},
			cause: "--forge-host is for a live render",
		},
		{
			name:   "missing memory",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.memory = "" },
			cause:  "--memory is required",
		},
		{
			name: "non-empty out",
			mutate: func(t *testing.T, f *clusterRenderFlags) {
				if err := os.MkdirAll(f.out, 0o755); err != nil {
					t.Fatalf("making --out: %v", err)
				}
				if err := os.WriteFile(filepath.Join(f.out, "keep.txt"), []byte("x"), 0o600); err != nil {
					t.Fatalf("writing the existing file: %v", err)
				}
			},
			cause: "is not empty",
		},
		{
			name:   "live render without confirmation",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.confirmStopped = false },
			cause:  "--confirm-container-stopped",
		},
		{
			name:   "missing dir",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.dir = "" },
			cause:  "--dir is required",
		},
		{
			name:   "missing out",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.out = "" },
			cause:  "--out is required",
		},
		{
			name:   "relative config dir",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.configDir = "relative/gt" },
			cause:  `--config-dir "relative/gt" must be an absolute path`,
		},
		{
			name:   "relative openshell config dir",
			mutate: func(_ *testing.T, f *clusterRenderFlags) { f.openShellConfigDir = "relative/openshell" },
			cause:  `--openshell-config-dir "relative/openshell" must be an absolute path`,
		},
		{
			name: "out is a file",
			mutate: func(t *testing.T, f *clusterRenderFlags) {
				if err := os.WriteFile(f.out, []byte("x"), 0o600); err != nil {
					t.Fatalf("writing --out as a file: %v", err)
				}
			},
			cause: "cannot be read as a directory",
		},
		{
			name: "values file does not parse",
			mutate: func(t *testing.T, f *clusterRenderFlags) {
				if err := os.WriteFile(imagevalues.PathIn(f.dir), []byte("daemon: [\n"), 0o600); err != nil {
					t.Fatalf("writing a corrupt values file: %v", err)
				}
			},
			cause: "reading the image values",
		},
		{
			name: "rehearsal in the live namespace",
			mutate: func(_ *testing.T, f *clusterRenderFlags) {
				f.rehearsal = true
				f.forgeHost = ""
				f.confirmStopped = false
				f.namespace = k8sdaemon.DefaultNamespace
			},
			cause: "may not render into the live namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			tt.mutate(t, &f)
			before := outState(f.out)
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			if err == nil {
				t.Fatalf("render succeeded, want a refusal naming %q", tt.cause)
			}
			if !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("refusal = %q, want it to name %q", err, tt.cause)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused render printed %q", out.String())
			}
			if after := outState(f.out); after != before {
				t.Fatalf("a refused render changed --out: before %q, after %q", before, after)
			}
		})
	}
}

// TestTownClusterRenderRefusesWhenAnInputOrStepFails drives each failure the
// render reads from the host or writes through: a town, environment or manifest
// it cannot read, and a build, encode or write step that fails. Each refusal
// names the step and writes nothing.
func TestTownClusterRenderRefusesWhenAnInputOrStepFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		with           bool // --with-helper
		noConfigDirs   bool // leave --config-dir and --openshell-config-dir to the defaults
		noOpenShellDir bool // leave --openshell-config-dir to its default
		mutate         func(t *testing.T, dir string, d *clusterRenderDeps)
		cause          string
		writeRefused   bool // the refused write may leave --out made, but never with a file in it
	}{
		{
			name:   "no town",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) { d.findTown = failing("no town") },
			cause:  "not in a Gas Town workspace",
		},
		{
			name: "no daemon env",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.containerEnv = func(string) (map[string]string, error) { return nil, errors.New("unreadable") }
			},
			cause: "reading the town's daemon.env",
		},
		{
			name: "no secrets manifest",
			mutate: func(_ *testing.T, dir string, d *clusterRenderDeps) {
				d.readFile = func(path string) ([]byte, error) {
					if path == filepath.Join(dir, secretsManifestRelPath) {
						return nil, errors.New("unreadable")
					}
					return os.ReadFile(path)
				}
			},
			cause: "reading the secrets manifest",
		},
		{
			name: "secrets manifest refused",
			mutate: func(t *testing.T, dir string, _ *clusterRenderDeps) {
				writeClusterRenderManifest(t, dir, "files:\n  - name: forgejo-landing.env\n    consumer: \"x\"\n    mode: \"0644\"\n    required: true\n")
			},
			cause: "is refused",
		},
		{
			name:         "no HOME for the default config directory",
			noConfigDirs: true,
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.userHome = func() (string, error) { return "", errors.New("no HOME") }
			},
			cause: "HOME cannot be found, so the default ~/.config/gt",
		},
		{
			name:           "no HOME for the openshell default",
			noOpenShellDir: true,
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.userHome = func() (string, error) { return "", errors.New("no HOME") }
			},
			cause: "HOME cannot be found, so the default ~/.config/openshell",
		},
		{
			name: "build refuses",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.build = func(k8sdaemon.Values) (k8sdaemon.Manifests, error) {
					return k8sdaemon.Manifests{}, errors.New("refused")
				}
			},
			cause: "cannot render the manifests",
		},
		{
			name: "render fails",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.render = func(io.Writer, k8sdaemon.Manifests) error { return errors.New("encode") }
			},
			cause: "cannot encode the manifests",
		},
		{
			name: "render emits a kind with no place in the apply order",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.render = func(w io.Writer, _ k8sdaemon.Manifests) error {
					_, err := io.WriteString(w, "kind: Bogus\nmetadata:\n  name: x\n")
					return err
				}
			},
			cause: "no place in the apply order",
		},
		{
			name: "the Deployment mounts a Secret the render did not write",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
					m, err := k8sdaemon.Build(v)
					m.Secrets = nil
					return m, err
				}
			},
			cause: "did not write",
		},
		{
			name: "seed helper refuses",
			with: true,
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.buildHelper = func(k8sdaemon.Values) (k8sdaemon.Helper, error) { return k8sdaemon.Helper{}, errors.New("refused") }
			},
			cause: "cannot render the seed helper",
		},
		{
			name: "seed helper fails to encode",
			with: true,
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.encode = func(io.Writer, any) error { return errors.New("encode") }
			},
			cause: "cannot encode the seed helper",
		},
		{
			name: "out cannot be made",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.mkdirAll = func(string) error { return errors.New("read-only") }
			},
			cause: "cannot create --out",
		},
		{
			name: "a file cannot be written",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps) {
				d.writeFile = func(string, []byte) error { return errors.New("disk full") }
			},
			cause:        "cannot write",
			writeRefused: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.withHelper = tt.with
			if tt.noConfigDirs {
				f.configDir, f.openShellConfigDir = "", ""
			}
			if tt.noOpenShellDir {
				f.openShellConfigDir = ""
			}
			var out bytes.Buffer
			d := clusterRenderTestDeps(t, &out)
			tt.mutate(t, f.dir, &d)
			err := runClusterRender(d, f)
			if err == nil {
				t.Fatalf("render succeeded, want a refusal naming %q", tt.cause)
			}
			if !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("refusal = %q, want it to name %q", err, tt.cause)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused render printed %q", out.String())
			}
			// Every refusal before the write step leaves --out absent. A refused write
			// may leave --out made, but never with a file in it.
			if tt.writeRefused {
				if names := dirNames(t, f.out); len(names) != 0 {
					t.Fatalf("a refused write left %v in --out", names)
				}
				return
			}
			if _, statErr := os.Stat(f.out); statErr == nil {
				t.Fatalf("a refused render created --out %s", f.out)
			}
		})
	}
}

// failing is a step that always fails with the given message.
func failing(msg string) func() (string, error) {
	return func() (string, error) { return "", errors.New(msg) }
}

func TestTownClusterRenderRehearsalOmitsPromoteAndSweep(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.rehearsal = true
	f.forgeHost = ""
	f.confirmStopped = false
	f.namespace = "gt-rehearsal"
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, kind := range renderedKinds(t, f.out) {
		if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
			t.Fatalf("a rehearsal wrote a sweep binding of kind %s", kind)
		}
	}
	mounted := deploymentSecretVolumes(t, f.out)
	if !slices.Equal(mounted, []string{"daemon-forgejo-viewer.env"}) {
		t.Fatalf("rehearsal mounts %v, want only the read-only viewer token", mounted)
	}
	for _, line := range createLines(out.String()) {
		if strings.Contains(line, "promote") {
			t.Fatalf("a rehearsal create line names the promote key: %s", line)
		}
	}
}

// clusterRenderPolecatDigest is a polecat image digest distinct from the daemon's,
// so the forwarding test cannot pass by reading the wrong field.
const clusterRenderPolecatDigest = "registry.example.test/gastown/polecat@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000002"

// TestTownClusterRenderForwardsThePolecatImage holds --polecat-image to the
// rendered pod template's annotation, the value the Deployment carries.
func TestTownClusterRenderForwardsThePolecatImage(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.polecatImage = clusterRenderPolecatDigest
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := deploymentAnnotations(t, f.out)["gastown.dev/polecat-image"]; got != clusterRenderPolecatDigest {
		t.Fatalf("the pod template's gastown.dev/polecat-image annotation = %q, want the flag's digest %q", got, clusterRenderPolecatDigest)
	}
}

func TestTownClusterRenderWithHelperAppliesTheSeedBeforeTheDeployment(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.withHelper = true
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	kinds := renderedKinds(t, f.out)
	n := len(kinds)
	if n < 2 || kinds[n-2] != "Pod" || kinds[n-1] != "Deployment" {
		t.Fatalf("with --with-helper the last two objects are %v, want the seed Pod then the Deployment", kinds[max(0, n-2):])
	}
}

// TestApplyObjectsRefusesWhatItCannotOrder holds the stream reader to a refusal
// for each document it cannot read, name, rank or re-encode.
func TestApplyObjectsRefusesWhatItCannotOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stream  string
		marshal func(any) ([]byte, error)
		cause   string
	}{
		{name: "stream is not YAML", stream: "kind: [\n", marshal: yaml.Marshal, cause: "reading the rendered manifests back"},
		{name: "kind is not a string", stream: "kind: [a]\n", marshal: yaml.Marshal, cause: "reading a rendered document's kind"},
		{name: "unknown kind", stream: "kind: Bogus\nmetadata:\n  name: x\n", marshal: yaml.Marshal, cause: "no place in the apply order"},
		{
			name:    "document does not re-encode",
			stream:  "kind: Namespace\nmetadata:\n  name: gt\n",
			marshal: func(any) ([]byte, error) { return nil, errors.New("encode") },
			cause:   "encoding a rendered Namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := applyObjects([]byte(tt.stream), tt.marshal)
			if err == nil || !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("applyObjects = %v, want a refusal naming %q", err, tt.cause)
			}
		})
	}
}

// TestApplyObjectsRanksTheTrackerBackupClaimWithTheCronJob holds the one claim
// applyObjects lifts out of the claims' place: the tracker-backup claim ranks
// with the CronJob, so it applies after trackerd's Deployment, and every other
// claim keeps the claims' rank.
func TestApplyObjectsRanksTheTrackerBackupClaimWithTheCronJob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		claim string
		rank  string
	}{
		{name: "the tracker-backup claim ranks with the CronJob", claim: k8sdaemon.TrackerBackupClaim, rank: "CronJob"},
		{name: "another claim keeps the claims' rank", claim: k8sdaemon.TownStateClaim, rank: "PersistentVolumeClaim"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := "kind: PersistentVolumeClaim\nmetadata:\n  name: " + tt.claim + "\n"
			objects, err := applyObjects([]byte(stream), yaml.Marshal)
			if err != nil {
				t.Fatalf("applyObjects: %v", err)
			}
			if len(objects) != 1 {
				t.Fatalf("applyObjects returned %d objects, want the one claim", len(objects))
			}
			if got, want := objects[0].rank, applyRank[tt.rank]; got != want {
				t.Errorf("the %s claim's rank = %d, want the %s rank %d", tt.claim, got, tt.rank, want)
			}
		})
	}
}

// TestSecretCreateLinesRefusesAVolumeItCannotName holds the create-line printer
// to a refusal for each mounted Secret it cannot name a source for.
func TestSecretCreateLinesRefusesAVolumeItCannotName(t *testing.T) {
	t.Parallel()
	mounts := func(name string, items []k8sdaemon.KeyToPath) k8sdaemon.Manifests {
		var m k8sdaemon.Manifests
		m.Deployment.Spec.Template.Spec.Volumes = []k8sdaemon.Volume{{
			Name:   "v",
			Secret: &k8sdaemon.SecretVolumeSource{SecretName: name, Items: items},
		}}
		return m
	}
	t.Run("no Secret was written for the volume", func(t *testing.T) {
		t.Parallel()
		_, err := secretCreateLines(mounts("daemon-x", nil), "gt", "/cfg", "/ossl", "/town")
		if err == nil || !strings.Contains(err.Error(), "did not write") {
			t.Fatalf("secretCreateLines = %v, want a refusal naming the unwritten Secret", err)
		}
	})
	t.Run("the openshell volume names no files", func(t *testing.T) {
		t.Parallel()
		m := mounts("daemon-openshell", nil)
		m.Secrets = []k8sdaemon.Secret{{Metadata: k8sdaemon.ObjectMeta{
			Name:        "daemon-openshell",
			Annotations: map[string]string{k8sdaemon.SourceFileAnnotation: "openshell"},
		}}}
		_, err := secretCreateLines(m, "gt", "/cfg", "/ossl", "/town")
		if err == nil || !strings.Contains(err.Error(), "names no files") {
			t.Fatalf("secretCreateLines = %v, want a refusal naming the empty openshell volume", err)
		}
	})
	t.Run("the mounted Secret has no source annotation", func(t *testing.T) {
		t.Parallel()
		m := mounts("daemon-x", nil)
		m.Secrets = []k8sdaemon.Secret{{Metadata: k8sdaemon.ObjectMeta{Name: "daemon-x"}}}
		lines, err := secretCreateLines(m, "gt", "/cfg", "/ossl", "/town")
		if err == nil || !strings.Contains(err.Error(), "no source file annotation") {
			t.Fatalf("secretCreateLines = %q, %v; want a refusal naming the missing source annotation", lines, err)
		}
		if lines != nil {
			t.Fatalf("a refused Secret still printed %q", lines)
		}
	})
}

// TestSecretCreateLinesQuotesEachWord holds the create line to one shell word
// per value: a namespace, Secret name or key with a space stays one argument.
func TestSecretCreateLinesQuotesEachWord(t *testing.T) {
	t.Parallel()
	var m k8sdaemon.Manifests
	m.Deployment.Spec.Template.Spec.Volumes = []k8sdaemon.Volume{{
		Name:   "v",
		Secret: &k8sdaemon.SecretVolumeSource{SecretName: "daemon x"},
	}}
	m.Secrets = []k8sdaemon.Secret{{Metadata: k8sdaemon.ObjectMeta{
		Name:        "daemon x",
		Annotations: map[string]string{k8sdaemon.SourceFileAnnotation: "my config.toml"},
	}}}
	lines, err := secretCreateLines(m, "gt ns", "/cfg", "/ossl", "/town")
	if err != nil {
		t.Fatalf("secretCreateLines: %v", err)
	}
	want := []string{`kubectl -n 'gt ns' create secret generic 'daemon x' --from-file='my config.toml'='/cfg/my config.toml'`}
	if !slices.Equal(lines, want) {
		t.Fatalf("secretCreateLines = %q, want %q", lines, want)
	}
}

// TestTownClusterRenderDefaultsTheConfigDirectories holds the defaults a render
// takes when --config-dir and --openshell-config-dir are left out: the XDG config
// home when it is set, else the home directory's .config, and no default at all
// when there is no home to read.
func TestTownClusterRenderDefaultsTheConfigDirectories(t *testing.T) {
	t.Parallel()
	noHome := func() (string, error) { return "", errors.New("no HOME") }
	home := func() (string, error) { return "/home/op", nil }
	tests := []struct {
		name    string
		env     map[string]string
		home    func() (string, error)
		want    string
		wantErr bool
	}{
		{name: "XDG config home set", env: map[string]string{"XDG_CONFIG_HOME": "/xdg"}, home: home, want: "/xdg/gt"},
		{name: "home directory's .config", env: map[string]string{}, home: home, want: "/home/op/.config/gt"},
		{name: "no home to read", env: map[string]string{}, home: noHome, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := xdgConfigSubdir("gt", func(k string) string { return tt.env[k] }, tt.home)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("xdgConfigSubdir = %q, want a refusal naming the missing home", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("xdgConfigSubdir = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// daemonContainer returns the daemon container's arguments, its mounts by path,
// and its environment by name, from the rendered Deployment file.
func daemonContainer(t *testing.T, out string) (args []string, mounts map[string]string, env map[string]string) {
	t.Helper()
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `yaml:"name"`
						Args []string `yaml:"args"`
						Env  []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
						VolumeMounts []struct {
							MountPath string `yaml:"mountPath"`
							SubPath   string `yaml:"subPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	for _, name := range dirNames(t, out) {
		if !strings.Contains(name, "-deployment-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if err := yaml.Unmarshal(data, &dep); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
	}
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatalf("no Deployment container was rendered into %s: %v", out, dirNames(t, out))
	}
	c := containers[0]
	mounts = map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m.SubPath
	}
	env = map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return c.Args, mounts, env
}

// TestTownClusterRenderLiveCarriesTheEntrypointArguments holds the live
// Deployment to the arguments and mounts the live compose command carries, so
// the pod's git has the forge credential helper (gt-22wubb). The forge-reading
// children read the same Forgejo, so the pod's environment names its API root
// (gt-s0homy).
func TestTownClusterRenderLiveCarriesTheEntrypointArguments(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	args, mounts, env := daemonContainer(t, f.out)
	wantArgs := []string{"--om-token-file", "/run/secrets/om-token", "--forge-host", "forgejo:3000", "--forge-token-file", "/run/secrets/forge-token"}
	if !slices.Equal(args, wantArgs) {
		t.Fatalf("daemon args = %v, want %v", args, wantArgs)
	}
	for path, sub := range map[string]string{
		"/run/secrets/om-token":    "om-daemon-token",
		"/run/secrets/forge-token": "forgejo-landing.env",
	} {
		if mounts[path] != sub {
			t.Fatalf("mount %s has subPath %q, want %q (mounts %v)", path, mounts[path], sub, mounts)
		}
	}
	if got, want := env["FORGEJO_API_URL"], "http://forgejo:3000/api/v1"; got != want {
		t.Fatalf("daemon env FORGEJO_API_URL = %q, want %q: the forge-reading children default to the pod's loopback otherwise", got, want)
	}
}

// TestTownClusterRenderRehearsalCarriesNeitherToken keeps the rehearsal pod off
// the om token and the forge host: it mounts only the viewer token.
func TestTownClusterRenderRehearsalCarriesNeitherToken(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.rehearsal = true
	f.forgeHost = ""
	f.confirmStopped = false
	f.namespace = "gt-rehearsal"
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	args, mounts, env := daemonContainer(t, f.out)
	if len(args) != 0 {
		t.Fatalf("a rehearsal daemon carries args %v, want none", args)
	}
	for _, path := range []string{"/run/secrets/om-token", "/run/secrets/forge-token"} {
		if sub, ok := mounts[path]; ok {
			t.Fatalf("a rehearsal mounts %s from %q", path, sub)
		}
	}
	if got, ok := env["FORGEJO_API_URL"]; ok {
		t.Fatalf("a rehearsal daemon carries FORGEJO_API_URL = %q, want none: it has no forge host", got)
	}
}

// TestTownClusterRenderRefusesTheDoltBackupHostPath drives each refusal of
// --dolt-backup-host-path: a rehearsal, a relative path, and an absolute path
// that is not the Dolt backup directory. Each names the flag and writes nothing.
func TestTownClusterRenderRefusesTheDoltBackupHostPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(f *clusterRenderFlags)
	}{
		{"a rehearsal", func(f *clusterRenderFlags) {
			f.rehearsal = true
			f.forgeHost = ""
			f.confirmStopped = false
			f.namespace = "gt-rehearsal"
			f.doltBackupHostPath = k8sdaemon.DoltBackupPath
		}},
		{"a relative path", func(f *clusterRenderFlags) { f.doltBackupHostPath = "gt-backups/dolt" }},
		{"another directory", func(f *clusterRenderFlags) { f.doltBackupHostPath = "/Users/user/elsewhere" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			tt.mutate(&f)
			before := outState(f.out)
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			if err == nil {
				t.Fatal("render succeeded, want a refusal naming --dolt-backup-host-path")
			}
			if !strings.Contains(err.Error(), "--dolt-backup-host-path") {
				t.Fatalf("refusal = %q, want it to name --dolt-backup-host-path", err)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused render printed %q", out.String())
			}
			if after := outState(f.out); after != before {
				t.Fatalf("a refused render changed --out: before %q, after %q", before, after)
			}
		})
	}
}

// TestTownClusterRenderLiveMountsTheDoltBackupHostPath holds a live render given
// the host path to the host's backup directory: no dolt-backup claim is written.
// Without the flag the claim is written. Either way the daemon names the backup
// root the pod mounts the backups at (gt-cbsx3k).
func TestTownClusterRenderLiveMountsTheDoltBackupHostPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		hostPath  string
		wantClaim bool
	}{
		{"without the flag", "", true},
		{"with the flag", k8sdaemon.DoltBackupPath, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.doltBackupHostPath = tc.hostPath
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			_, _, env := daemonContainer(t, f.out)
			if got := env["GT_DOLT_BACKUP_ROOT"]; got != k8sdaemon.DoltBackupPath {
				t.Errorf("daemon env GT_DOLT_BACKUP_ROOT = %q, want %q", got, k8sdaemon.DoltBackupPath)
			}
			wantFile := "persistentvolumeclaim-" + k8sdaemon.DoltBackupClaim
			hasClaim := false
			for _, name := range dirNames(t, f.out) {
				if strings.Contains(name, wantFile) {
					hasClaim = true
				}
			}
			if hasClaim != tc.wantClaim {
				t.Errorf("a %s claim written = %t, want %t: files %v", k8sdaemon.DoltBackupClaim, hasClaim, tc.wantClaim, dirNames(t, f.out))
			}
		})
	}
}

// renderedHostPaths is the path and type of each hostPath volume in the rendered
// Deployment whose file name ends in suffix, as "path type" strings.
func renderedHostPaths(t *testing.T, out, suffix string) []string {
	t.Helper()
	for _, name := range dirNames(t, out) {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		var dep struct {
			Spec struct {
				Template struct {
					Spec struct {
						Volumes []struct {
							HostPath *struct {
								Path string `yaml:"path"`
								Type string `yaml:"type"`
							} `yaml:"hostPath"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &dep); err != nil {
			t.Fatalf("%s does not parse back: %v", name, err)
		}
		var got []string
		for _, vol := range dep.Spec.Template.Spec.Volumes {
			if vol.HostPath != nil {
				got = append(got, vol.HostPath.Path+" "+vol.HostPath.Type)
			}
		}
		return got
	}
	t.Fatalf("no file ending %s in %s", suffix, out)
	return nil
}

// TestTownClusterRenderDashboardFlag holds --dashboard and --dashboard-claude-dir:
// the pair is written in apply order, the flag off leaves the file names and the
// Secret lines as they were, each refusal writes nothing, and the claude directory
// is the dashboard's only host path.
func TestTownClusterRenderDashboardFlag(t *testing.T) {
	t.Parallel()

	t.Run("the pair is written in apply order", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		want := []string{
			"01-namespace-gt.yaml",
			"02-persistentvolumeclaim-town-state.yaml",
			"03-persistentvolumeclaim-landing-work.yaml",
			"04-persistentvolumeclaim-dolt-backup.yaml",
			"05-serviceaccount-daemon.yaml",
			"06-service-forgejo.yaml",
			"07-service-dashboard.yaml",
			"08-rolebinding-daemon-sweep-runner.yaml",
			"09-rolebinding-daemon-gt-ci-cache-trim.yaml",
			"10-clusterrolebinding-daemon-gt-sweep-reader.yaml",
			"11-deployment-daemon.yaml",
			"12-deployment-dashboard.yaml",
		}
		if got := dirNames(t, f.out); !slices.Equal(got, want) {
			t.Fatalf("file names = %v, want %v", got, want)
		}
	})

	t.Run("the flag off leaves the names and the Secret lines as they were", func(t *testing.T) {
		f := clusterRenderFixture(t)
		// Both renders read one town, so the derived hm config's create line
		// carries the same path in each.
		town := clusterRenderTestTown(t)
		deps := func(out io.Writer) clusterRenderDeps {
			d := clusterRenderTestDeps(t, out)
			d.findTown = func() (string, error) { return town, nil }
			return d
		}
		var off bytes.Buffer
		if err := runClusterRender(deps(&off), f); err != nil {
			t.Fatalf("render without the flag: %v", err)
		}
		want := []string{
			"01-namespace-gt.yaml",
			"02-persistentvolumeclaim-town-state.yaml",
			"03-persistentvolumeclaim-landing-work.yaml",
			"04-persistentvolumeclaim-dolt-backup.yaml",
			"05-serviceaccount-daemon.yaml",
			"06-service-forgejo.yaml",
			"07-rolebinding-daemon-sweep-runner.yaml",
			"08-rolebinding-daemon-gt-ci-cache-trim.yaml",
			"09-clusterrolebinding-daemon-gt-sweep-reader.yaml",
			"10-deployment-daemon.yaml",
		}
		if got := dirNames(t, f.out); !slices.Equal(got, want) {
			t.Fatalf("file names without the flag = %v, want %v", got, want)
		}
		// The same fixture with the flag on writes to a second directory, so the
		// Secret lines compare with the same config paths.
		on := f
		on.dashboard = true
		on.out = filepath.Join(t.TempDir(), "out-with-dashboard")
		var with bytes.Buffer
		if err := runClusterRender(deps(&with), on); err != nil {
			t.Fatalf("render with the flag: %v", err)
		}
		if got, want := createLines(with.String()), createLines(off.String()); !slices.Equal(got, want) {
			t.Errorf("the Secret lines with the flag = %q, want %q", got, want)
		}
	})

	t.Run("a missing viewer entry refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		// Renaming the entry leaves the manifest with no forgejo-viewer.env.
		writeClusterRenderManifest(t, f.dir, strings.Replace(clusterRenderSecretsManifest, "name: forgejo-viewer.env", "name: forgejo-viewer-retired.env", 1))
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, k8sdaemon.ViewerTokenFile, "--dashboard")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("the dashboard with a rehearsal refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.rehearsal = true
		f.forgeHost = ""
		f.namespace = "gt-rehearsal"
		f.dashboard = true
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--dashboard", "--rehearsal")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("a relative claude directory refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.dashboardClaudeDir = "Users/user/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--dashboard-claude-dir", "absolute")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("the claude directory without the dashboard refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboardClaudeDir = "/Users/user/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--dashboard-claude-dir", "--dashboard")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("the claude directory is the dashboard's only host path", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.dashboardClaudeDir = "/Users/user/.claude"
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if got, want := renderedHostPaths(t, f.out, "-deployment-dashboard.yaml"), []string{"/Users/user/.claude Directory"}; !slices.Equal(got, want) {
			t.Errorf("the dashboard's host paths = %q, want %q", got, want)
		}
		if got := renderedHostPaths(t, f.out, "-deployment-daemon.yaml"); len(got) != 0 {
			t.Errorf("the daemon's host paths = %q, want none", got)
		}
	})
}

// TestTownClusterRenderDaemonClaudeDirFlag holds --daemon-claude-dir: the flag sets
// Values.DaemonClaudeDir, its absence leaves the file names and the daemon's host
// paths as they were, and a relative path is refused naming the flag before a write.
func TestTownClusterRenderDaemonClaudeDirFlag(t *testing.T) {
	t.Parallel()

	t.Run("the flag sets Values.DaemonClaudeDir", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.daemonClaudeDir = "/Users/user/.claude"
		var out bytes.Buffer
		deps := clusterRenderTestDeps(t, &out)
		var seen k8sdaemon.Values
		deps.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
			seen = v
			return k8sdaemon.Build(v)
		}
		if err := runClusterRender(deps, f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if seen.DaemonClaudeDir != f.daemonClaudeDir {
			t.Errorf("Values.DaemonClaudeDir = %q, want %q", seen.DaemonClaudeDir, f.daemonClaudeDir)
		}
		if got, want := renderedHostPaths(t, f.out, "-deployment-daemon.yaml"), []string{"/Users/user/.claude Directory"}; !slices.Equal(got, want) {
			t.Errorf("the daemon's host paths = %q, want %q", got, want)
		}
	})

	t.Run("the flag off leaves the names and the daemon's host paths as they were", func(t *testing.T) {
		f := clusterRenderFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render without the flag: %v", err)
		}
		want := []string{
			"01-namespace-gt.yaml",
			"02-persistentvolumeclaim-town-state.yaml",
			"03-persistentvolumeclaim-landing-work.yaml",
			"04-persistentvolumeclaim-dolt-backup.yaml",
			"05-serviceaccount-daemon.yaml",
			"06-service-forgejo.yaml",
			"07-rolebinding-daemon-sweep-runner.yaml",
			"08-rolebinding-daemon-gt-ci-cache-trim.yaml",
			"09-clusterrolebinding-daemon-gt-sweep-reader.yaml",
			"10-deployment-daemon.yaml",
		}
		if got := dirNames(t, f.out); !slices.Equal(got, want) {
			t.Fatalf("file names without the flag = %v, want %v", got, want)
		}
		if got := renderedHostPaths(t, f.out, "-deployment-daemon.yaml"); len(got) != 0 {
			t.Errorf("the daemon's host paths without the flag = %q, want none", got)
		}
	})

	t.Run("a relative claude directory refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.daemonClaudeDir = "Users/user/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--daemon-claude-dir", "absolute")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})
}

// requireErrorNames fails the test unless err is non-nil and its text names each
// of want.
func requireErrorNames(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the render succeeded, want a refusal naming %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal %q does not name %q", err, w)
		}
	}
}

// TestTownClusterRenderTrackerFlag holds --tracker: trackerd's Service sorts
// after the daemon's Service and its Deployment after the daemon's Deployment in
// apply order, and each refusal names the flag it is about before anything is
// written.
func TestTownClusterRenderTrackerFlag(t *testing.T) {
	t.Parallel()

	trackerFixture := func(t *testing.T) clusterRenderFlags {
		t.Helper()
		f := clusterRenderFixture(t)
		f.tracker = true
		f.trackerDSNSecret = "gt-trackerd-dsn"
		f.proxyImage = clusterRenderDaemonDigest
		f.hostGateway = "192.168.64.1"
		return f
	}

	t.Run("the pair follows the daemon's Service and Deployment in apply order", func(t *testing.T) {
		f := trackerFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		want := []string{
			"01-namespace-gt.yaml",
			"02-persistentvolumeclaim-town-state.yaml",
			"03-persistentvolumeclaim-landing-work.yaml",
			"04-persistentvolumeclaim-dolt-backup.yaml",
			"05-serviceaccount-daemon.yaml",
			"06-service-forgejo.yaml",
			"07-service-trackerd.yaml",
			"08-rolebinding-daemon-sweep-runner.yaml",
			"09-rolebinding-daemon-gt-ci-cache-trim.yaml",
			"10-clusterrolebinding-daemon-gt-sweep-reader.yaml",
			"11-deployment-daemon.yaml",
			"12-deployment-trackerd.yaml",
		}
		if got := dirNames(t, f.out); !slices.Equal(got, want) {
			t.Fatalf("file names = %v, want %v", got, want)
		}
	})

	t.Run("each refusal names its flag and writes nothing", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(f *clusterRenderFlags)
			names  []string
		}{
			{
				name: "with a rehearsal",
				mutate: func(f *clusterRenderFlags) {
					f.rehearsal = true
					f.namespace = "gt-rehearsal"
					f.forgeHost = ""
				},
				names: []string{"--tracker", "--rehearsal"},
			},
			{
				name:   "without the DSN Secret name",
				mutate: func(f *clusterRenderFlags) { f.trackerDSNSecret = "  " },
				names:  []string{"--tracker-dsn-secret"},
			},
			{
				name:   "without a proxy image",
				mutate: func(f *clusterRenderFlags) { f.proxyImage = "" },
				names:  []string{"--proxy-image"},
			},
			{
				name:   "the DSN Secret without the tracker",
				mutate: func(f *clusterRenderFlags) { f.tracker = false },
				names:  []string{"--tracker-dsn-secret", "--tracker"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := trackerFixture(t)
				tt.mutate(&f)
				var out bytes.Buffer
				requireErrorNames(t, runClusterRender(clusterRenderTestDeps(t, &out), f), tt.names...)
				if _, err := os.Stat(f.out); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("--out was created by a refused render: stat = %v", err)
				}
			})
		}
	})

}

// TestTownClusterRenderTrackerDoesNotReadTheDSN: the render names the Secret
// trackerd reads its DSN from and never opens the DSN. The render's secret-file
// reads go through readFile, so the fake below records every path the render
// opens and fails when one is not the secrets manifest; grepping the output for
// a DSN-shaped string cannot see a read that is not printed.
func TestTownClusterRenderTrackerDoesNotReadTheDSN(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.tracker = true
	f.trackerDSNSecret = "gt-trackerd-dsn"
	f.proxyImage = clusterRenderDaemonDigest
	f.hostGateway = "192.168.64.1"
	// A DSN file named for the Secret sits in the config directory, so a render
	// that opens one reads a real value and the fake records the read.
	f.configDir = t.TempDir()
	dsnPath := filepath.Join(f.configDir, f.trackerDSNSecret)
	if err := os.WriteFile(dsnPath, []byte("postgres://trackerd:sentinel@example.test:5432/tracker"), 0o600); err != nil {
		t.Fatalf("writing the DSN file: %v", err)
	}
	var out bytes.Buffer
	deps := clusterRenderTestDeps(t, &out)
	var reads []string
	deps.readFile = func(path string) ([]byte, error) {
		reads = append(reads, path)
		return os.ReadFile(path)
	}
	if err := runClusterRender(deps, f); err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := []string{filepath.Join(f.dir, secretsManifestRelPath)}; !slices.Equal(reads, want) {
		t.Fatalf("the render read %v, want only the secrets manifest %v", reads, want)
	}
	if !strings.Contains(out.String(), "reads its DSN from Secret gt-trackerd-dsn under key dsn") {
		t.Fatalf("the output does not name the DSN Secret and its key:\n%s", out.String())
	}
	// The DSN Secret is read as an environment variable, so no create line
	// names it: a create line would need a value the render does not have.
	for _, l := range createLines(out.String()) {
		if strings.Contains(l, "gt-trackerd-dsn") {
			t.Errorf("a create line names the DSN Secret: %s", l)
		}
	}
	if strings.Contains(out.String(), "postgres://") {
		t.Errorf("the output holds a DSN-shaped string:\n%s", out.String())
	}
	for _, name := range dirNames(t, f.out) {
		data, err := os.ReadFile(filepath.Join(f.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if strings.Contains(string(data), "postgres://") {
			t.Errorf("%s holds a DSN-shaped string", name)
		}
	}
}

// TestTownClusterRenderTrackerBackupFlag holds --tracker-backup: the dump's claim
// and CronJob follow trackerd's Deployment in apply order, each refusal names the
// flag it is about before anything is written, and the render names the image and
// the DSN Secret the dump runs with, never the DSN.
func TestTownClusterRenderTrackerBackupFlag(t *testing.T) {
	t.Parallel()

	const backupImage = "registry.example.test/library/postgres@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000002"

	backupFixture := func(t *testing.T) clusterRenderFlags {
		t.Helper()
		f := clusterRenderFixture(t)
		f.tracker = true
		f.trackerDSNSecret = "gt-trackerd-dsn"
		f.proxyImage = clusterRenderDaemonDigest
		f.hostGateway = "192.168.64.1"
		f.trackerBackup = true
		f.trackerBackupImage = backupImage
		return f
	}

	t.Run("the claim and the CronJob follow trackerd's Deployment", func(t *testing.T) {
		f := backupFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		want := []string{
			"01-namespace-gt.yaml",
			"02-persistentvolumeclaim-town-state.yaml",
			"03-persistentvolumeclaim-landing-work.yaml",
			"04-persistentvolumeclaim-dolt-backup.yaml",
			"05-serviceaccount-daemon.yaml",
			"06-service-forgejo.yaml",
			"07-service-trackerd.yaml",
			"08-rolebinding-daemon-sweep-runner.yaml",
			"09-rolebinding-daemon-gt-ci-cache-trim.yaml",
			"10-clusterrolebinding-daemon-gt-sweep-reader.yaml",
			"11-deployment-daemon.yaml",
			"12-deployment-trackerd.yaml",
			"13-persistentvolumeclaim-tracker-backup.yaml",
			"14-cronjob-tracker-backup.yaml",
			"15-pod-tracker-backup-reader.yaml",
		}
		if got := dirNames(t, f.out); !slices.Equal(got, want) {
			t.Fatalf("file names = %v, want %v", got, want)
		}
		cron, err := os.ReadFile(filepath.Join(f.out, "14-cronjob-tracker-backup.yaml"))
		if err != nil {
			t.Fatalf("reading the CronJob: %v", err)
		}
		if !strings.Contains(string(cron), backupImage) {
			t.Errorf("the CronJob does not run the named image %s:\n%s", backupImage, cron)
		}
	})

	t.Run("each refusal names its flag and writes nothing", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(f *clusterRenderFlags)
			names  []string
		}{
			{
				name: "without the tracker",
				mutate: func(f *clusterRenderFlags) {
					f.tracker = false
					f.trackerDSNSecret = ""
					f.proxyImage = ""
				},
				names: []string{"--tracker-backup needs --tracker:"},
			},
			{
				name:   "without the image",
				mutate: func(f *clusterRenderFlags) { f.trackerBackupImage = "  " },
				names:  []string{"--tracker-backup needs --tracker-backup-image"},
			},
			{
				name: "with a rehearsal",
				mutate: func(f *clusterRenderFlags) {
					f.rehearsal = true
					f.namespace = "gt-rehearsal"
					f.forgeHost = ""
				},
				names: []string{"--tracker is for a live render"},
			},
			{
				name: "the image without the backup",
				mutate: func(f *clusterRenderFlags) {
					f.trackerBackup = false
				},
				names: []string{"--tracker-backup-image is for the nightly dump", "name --tracker-backup with it"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := backupFixture(t)
				tt.mutate(&f)
				var out bytes.Buffer
				requireErrorNames(t, runClusterRender(clusterRenderTestDeps(t, &out), f), tt.names...)
				if _, err := os.Stat(f.out); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("--out was created by a refused render: stat = %v", err)
				}
			})
		}
	})

	t.Run("the image and the DSN Secret are named, and the DSN is never printed or written", func(t *testing.T) {
		f := backupFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out.String(), "runs in image "+backupImage+" and reads its DSN from Secret gt-trackerd-dsn under key dsn") {
			t.Fatalf("the output does not name the dump's image and DSN Secret:\n%s", out.String())
		}
		for _, l := range createLines(out.String()) {
			if strings.Contains(l, "gt-trackerd-dsn") {
				t.Errorf("a create line names the DSN Secret: %s", l)
			}
		}
		for _, name := range dirNames(t, f.out) {
			data, err := os.ReadFile(filepath.Join(f.out, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			if strings.Contains(string(data), "postgres://") {
				t.Errorf("%s holds a DSN-shaped string", name)
			}
		}
	})

	t.Run("the reader mounts the claim read-only, never restarts, and is not applied with the rest", func(t *testing.T) {
		f := backupFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(f.out, "15-pod-tracker-backup-reader.yaml"))
		if err != nil {
			t.Fatalf("reading the reader: %v", err)
		}
		var pod struct {
			Spec struct {
				RestartPolicy string `yaml:"restartPolicy"`
				Containers    []struct {
					Image        string `yaml:"image"`
					VolumeMounts []struct {
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &pod); err != nil {
			t.Fatalf("the reader does not parse back: %v", err)
		}
		if kind := renderedKind(t, filepath.Join(f.out, "15-pod-tracker-backup-reader.yaml")); kind != "Pod" {
			t.Errorf("the reader's kind = %q, want Pod", kind)
		}
		if pod.Spec.RestartPolicy != "Never" {
			t.Errorf("the reader's restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
		}
		if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != backupImage {
			t.Fatalf("the reader's containers = %+v, want one on the dump image %s", pod.Spec.Containers, backupImage)
		}
		mounts := pod.Spec.Containers[0].VolumeMounts
		if len(mounts) != 1 || mounts[0].MountPath != "/backups" || !mounts[0].ReadOnly {
			t.Errorf("the reader's mounts = %+v, want the claim at /backups read-only", mounts)
		}
		if !strings.Contains(out.String(), "tracker-backup-reader Pod") || !strings.Contains(out.String(), "is not applied with the rest") {
			t.Errorf("the output does not say the reader is not applied with the rest:\n%s", out.String())
		}
	})

	t.Run("a reader that cannot render or encode refuses and writes nothing", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(d *clusterRenderDeps)
			cause  string
		}{
			{
				name: "the builder refuses",
				mutate: func(d *clusterRenderDeps) {
					d.buildTrackerBackupReader = func(k8sdaemon.Values) (k8sdaemon.TrackerBackupReader, error) {
						return k8sdaemon.TrackerBackupReader{}, errors.New("refused")
					}
				},
				cause: "cannot render the tracker dump's reader",
			},
			{
				name: "the encoder fails",
				mutate: func(d *clusterRenderDeps) {
					d.encode = func(io.Writer, any) error { return errors.New("encode") }
				},
				cause: "cannot encode the tracker dump's reader",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := backupFixture(t)
				var out bytes.Buffer
				d := clusterRenderTestDeps(t, &out)
				tt.mutate(&d)
				requireErrorNames(t, runClusterRender(d, f), tt.cause, "no file was written")
				if got := outState(f.out); got != "absent" {
					t.Errorf("the refused render left --out %s, want it untouched", got)
				}
			})
		}
	})
}

// TestTownClusterRenderWritesTheRunningValuesFile holds a live render to the
// town's own values file, the one the pair-ready check reads the running commit
// from (gt-tju7oo).
func TestTownClusterRenderWritesTheRunningValuesFile(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	d := clusterRenderTestDeps(t, io.Discard)
	town, err := d.findTown()
	if err != nil {
		t.Fatalf("finding the town: %v", err)
	}
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("render: %v", err)
	}
	want, err := os.ReadFile(imagevalues.PathIn(f.dir))
	if err != nil {
		t.Fatalf("reading the render's values file: %v", err)
	}
	got, err := os.ReadFile(imagevalues.PathIn(filepath.Join(town, "gastown")))
	if err != nil {
		t.Fatalf("the render wrote no running values file under the town: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the town's values file = %q, want the render's %q", got, want)
	}
	if !strings.Contains(string(got), "commit: "+strings.Repeat("a", 40)+"\n") {
		t.Fatalf("the town's values file does not hold the commit line:\n%s", got)
	}
}

// TestTownClusterRenderRehearsalWritesNoRunningValuesFile holds a rehearsal to
// writing nothing under the town, and to printing no copy step for the live pod.
func TestTownClusterRenderRehearsalWritesNoRunningValuesFile(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.rehearsal = true
	f.forgeHost = ""
	f.confirmStopped = false
	f.namespace = "gt-rehearsal"
	d := clusterRenderTestDeps(t, io.Discard)
	town, err := d.findTown()
	if err != nil {
		t.Fatalf("finding the town: %v", err)
	}
	var out bytes.Buffer
	d.out = &out
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := os.Stat(imagevalues.PathIn(filepath.Join(town, "gastown"))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a rehearsal wrote the running values file under the town (stat: %v)", err)
	}
	if strings.Contains(out.String(), "kubectl -n gt-rehearsal exec") {
		t.Fatalf("a rehearsal prints a copy step for the live pod:\n%s", out.String())
	}
}

// TestTownClusterRenderPrintsThePodsTownCopyStep names the command that copies
// the values file into the daemon pod's town copy: the pod's town root is
// TownRootPath, so the copy lands at TownRootPath/gastown, and the line is
// printed, never run.
func TestTownClusterRenderPrintsThePodsTownCopyStep(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	d := clusterRenderTestDeps(t, io.Discard)
	town, err := d.findTown()
	if err != nil {
		t.Fatalf("finding the town: %v", err)
	}
	var out bytes.Buffer
	d.out = &out
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("render: %v", err)
	}
	townValues := imagevalues.PathIn(filepath.Join(town, "gastown"))
	podPath := k8sdaemon.TownRootPath + "/gastown/deploy/values/images.yaml"
	var copyLines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, " exec ") {
			copyLines = append(copyLines, strings.TrimSpace(line))
		}
	}
	if len(copyLines) != 1 {
		t.Fatalf("printed %d copy lines, want exactly 1:\n%s", len(copyLines), out.String())
	}
	line := copyLines[0]
	if !strings.HasPrefix(line, "kubectl -n gt exec -i deploy/daemon -c daemon -- sh -c ") {
		t.Errorf("the copy line does not exec into the daemon container: %s", line)
	}
	if !strings.Contains(line, podPath) {
		t.Errorf("the copy line does not name the pod's town copy %s: %s", podPath, line)
	}
	if !strings.HasSuffix(line, "< "+townValues) {
		t.Errorf("the copy line does not copy from the town's values file %s: %s", townValues, line)
	}
	// The pod's file is replaced only by a move from a non-empty .tmp copy, so
	// a copy that loses its stdin (no -i) leaves the pod's file as it was.
	for _, step := range []string{" -i ", podPath + ".tmp", "test -s " + podPath + ".tmp && mv " + podPath + ".tmp " + podPath} {
		if !strings.Contains(line, step) {
			t.Errorf("the copy line lacks %q, so a failed copy could empty the pod's file: %s", step, line)
		}
	}
}

// TestTownClusterRenderRefusesAnUnwritableRunningValuesFile holds a failed write
// of the town's values file to a named refusal, not a silent success.
func TestTownClusterRenderRefusesAnUnwritableRunningValuesFile(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	d := clusterRenderTestDeps(t, io.Discard)
	town, err := d.findTown()
	if err != nil {
		t.Fatalf("finding the town: %v", err)
	}
	// A regular file where the town's gastown directory belongs makes the write fail.
	if err := os.WriteFile(filepath.Join(town, "gastown"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}
	err = runClusterRender(d, f)
	requireErrorNames(t, err, "running values file", imagevalues.PathIn(filepath.Join(town, "gastown")))
}

// renderedText is the whole text of one rendered file.
func renderedText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestTownClusterRenderRecordsItsFlagsAndReplaysThem: a live render with
// --record-flags writes the flags it used, defaults and all, and a later
// --use-recorded render of that town writes the same manifests.
func TestTownClusterRenderRecordsItsFlagsAndReplaysThem(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	// The config directories are left empty, so the record must hold the ones the
	// run resolved rather than the empty flags it was given.
	f.configDir, f.openShellConfigDir = "", ""
	town := clusterRenderTestTown(t)
	var first bytes.Buffer
	d := clusterRenderDepsAt(t, town, &first)
	d.record = clusterRenderRecordOptions{record: true}
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("the recording render: %v", err)
	}
	recordPath := renderFlagsPathIn(town)
	rec, ok, err := loadRenderFlags(recordPath)
	if err != nil {
		t.Fatalf("reading the recorded flags: %v", err)
	}
	if !ok {
		t.Fatalf("the render recorded no flags at %s", recordPath)
	}
	if want := filepath.Join("/fake/home", ".config", "gt"); rec.configDir != want {
		t.Errorf("recorded --config-dir = %q, want the resolved %q: the record holds the flags the run used", rec.configDir, want)
	}
	for _, diff := range []struct {
		flag string
		got  any
		want any
	}{
		{"--dir", rec.dir, f.dir},
		{"--out", rec.out, f.out},
		{"--cpu", rec.cpu, f.cpu},
		{"--memory", rec.memory, f.memory},
		{"--forge-host", rec.forgeHost, f.forgeHost},
		{"--confirm-container-stopped", rec.confirmStopped, true},
	} {
		if diff.got != diff.want {
			t.Errorf("recorded %s = %v, want the value the run used, %v", diff.flag, diff.got, diff.want)
		}
	}
	if !strings.Contains(first.String(), recordPath) {
		t.Errorf("the render did not say where it recorded the flags:\n%s", first.String())
	}

	// The replay names only a fresh --out; every other flag is the record's.
	replay := clusterRenderFlags{out: filepath.Join(t.TempDir(), "replay")}
	d = clusterRenderDepsAt(t, town, io.Discard)
	d.record = clusterRenderRecordOptions{
		use:   true,
		named: func(flag string) bool { return flag == "out" },
	}
	if err := runClusterRender(d, replay); err != nil {
		t.Fatalf("the replay: %v", err)
	}
	wantNames := dirNames(t, f.out)
	gotNames := dirNames(t, replay.out)
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("the replay wrote %v, want the recorded render's %v", gotNames, wantNames)
	}
	for _, name := range wantNames {
		if got, want := renderedText(t, filepath.Join(replay.out, name)), renderedText(t, filepath.Join(f.out, name)); got != want {
			t.Errorf("the replay's %s differs from the recorded render's:\n%s\nwant:\n%s", name, got, want)
		}
	}
}

// TestTownClusterRenderCommandLineFlagWinsOverTheRecordedOne: a flag named on the
// replay's command line wins over its recorded value.
func TestTownClusterRenderCommandLineFlagWinsOverTheRecordedOne(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	f.namespace = "gt-recorded"
	town := clusterRenderTestTown(t)
	d := clusterRenderDepsAt(t, town, io.Discard)
	d.record = clusterRenderRecordOptions{record: true}
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("the recording render: %v", err)
	}
	replay := clusterRenderFlags{out: filepath.Join(t.TempDir(), "replay"), namespace: "gt-replay"}
	d = clusterRenderDepsAt(t, town, io.Discard)
	d.record = clusterRenderRecordOptions{
		use: true,
		named: func(flag string) bool {
			return flag == "namespace" || flag == "out"
		},
	}
	if err := runClusterRender(d, replay); err != nil {
		t.Fatalf("the replay: %v", err)
	}
	names := dirNames(t, replay.out)
	if !slices.Contains(names, "01-namespace-gt-replay.yaml") {
		t.Fatalf("the replay wrote %v, want the namespace file of the one named on the command line", names)
	}
	if slices.Contains(names, "01-namespace-gt-recorded.yaml") {
		t.Fatalf("the replay rendered the recorded namespace over the one named on the command line: %v", names)
	}
}

// TestTownClusterRenderRefusesARecordedReplayWithNothingRecorded: a replay with no
// record takes no defaults from nowhere, and writes no manifest.
func TestTownClusterRenderRefusesARecordedReplayWithNothingRecorded(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	var out bytes.Buffer
	d := clusterRenderTestDeps(t, &out)
	town, err := d.findTown()
	if err != nil {
		t.Fatalf("finding the town: %v", err)
	}
	d.record = clusterRenderRecordOptions{use: true}
	err = runClusterRender(d, f)
	requireErrorNames(t, err, "--use-recorded", "no render has recorded its flags", renderFlagsPathIn(town))
	if out.Len() != 0 {
		t.Fatalf("a refused replay printed %q", out.String())
	}
	if names := dirNames(t, f.out); len(names) != 0 {
		t.Fatalf("a refused replay wrote %v", names)
	}
}

// TestTownClusterRenderRefusesTheRecordedFlags drives the remaining refusals the
// two flags add: a rehearsal's record, a recorded file the loader refuses, and a
// record that cannot be written. Each names its cause; none leaves a claimed
// success.
func TestTownClusterRenderRefusesTheRecordedFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		mutate         func(t *testing.T, town string, d *clusterRenderDeps, f *clusterRenderFlags)
		cause          string
		wroteManifests bool // the record is written after the manifests, so they exist
	}{
		{
			name: "recording a rehearsal",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps, f *clusterRenderFlags) {
				f.rehearsal = true
				f.forgeHost = ""
				f.confirmStopped = false
				f.namespace = "gt-rehearsal"
				d.record = clusterRenderRecordOptions{record: true}
			},
			cause: "--record-flags is for a live render",
		},
		{
			name: "a recorded file that the loader refuses",
			mutate: func(t *testing.T, town string, d *clusterRenderDeps, _ *clusterRenderFlags) {
				path := renderFlagsPathIn(town)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("making the record's directory: %v", err)
				}
				if err := os.WriteFile(path, []byte(`{"cpu":`), 0o600); err != nil {
					t.Fatalf("writing the refused record: %v", err)
				}
				d.record = clusterRenderRecordOptions{use: true}
			},
			cause: "are refused",
		},
		{
			name: "a replay that cannot tell which flags were named",
			mutate: func(t *testing.T, town string, d *clusterRenderDeps, _ *clusterRenderFlags) {
				// A record is there, so the refusal is the replay's missing answer to
				// which flags the command line named, not an absent record.
				if err := saveRenderFlags(renderFlagsPathIn(town), clusterRenderFixture(t)); err != nil {
					t.Fatalf("writing the record: %v", err)
				}
				d.record = clusterRenderRecordOptions{use: true}
			},
			cause: "cannot tell which flags the command line named",
		},
		{
			name: "a record that cannot be written",
			mutate: func(_ *testing.T, _ string, d *clusterRenderDeps, _ *clusterRenderFlags) {
				d.record = clusterRenderRecordOptions{record: true}
				d.saveRecord = func(string, clusterRenderFlags) error { return errors.New("read-only") }
			},
			cause:          "cannot write the recorded render flags, and the manifests are written",
			wroteManifests: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			town := clusterRenderTestTown(t)
			var out bytes.Buffer
			d := clusterRenderDepsAt(t, town, &out)
			tt.mutate(t, town, &d, &f)
			// A run that refuses records nothing, so whatever the file held when it
			// started is still there.
			recordPath := renderFlagsPathIn(town)
			before, beforeErr := os.ReadFile(recordPath)
			err := runClusterRender(d, f)
			requireErrorNames(t, err, tt.cause)
			if out.Len() != 0 {
				t.Fatalf("a refused run printed %q", out.String())
			}
			names := dirNames(t, f.out)
			if !tt.wroteManifests && len(names) != 0 {
				t.Fatalf("a refused run wrote %v", names)
			}
			if tt.wroteManifests && len(names) == 0 {
				t.Fatal("the refusal says the manifests are written, and the run wrote none")
			}
			after, afterErr := os.ReadFile(recordPath)
			if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
				t.Fatalf("a refused run changed the recorded flags at %s", recordPath)
			}
		})
	}
}

// TestTownClusterRenderFindsTheTownWhereTheRunNeedsIt: a run with neither record
// flag checks its flags and the image pair before the town, so a broken pair is
// what it refuses on; a run with either record flag reads the record under the
// town root, so the town is looked up first.
func TestTownClusterRenderFindsTheTownWhereTheRunNeedsIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		record clusterRenderRecordOptions
		cause  string
	}{
		{
			name:  "no record flag reports the image pair before the town",
			cause: "holds no image pair",
		},
		{
			name:   "--record-flags needs the town first",
			record: clusterRenderRecordOptions{record: true},
			cause:  "not in a Gas Town workspace",
		},
		{
			name:   "--use-recorded needs the town first",
			record: clusterRenderRecordOptions{use: true},
			cause:  "not in a Gas Town workspace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			// A directory with no image pair in it.
			f.dir = filepath.Join(t.TempDir(), "no-pair")
			d := clusterRenderTestDeps(t, io.Discard)
			d.findTown = failing("no town")
			d.record = tt.record
			err := runClusterRender(d, f)
			requireErrorNames(t, err, tt.cause)
			other := "not in a Gas Town workspace"
			if tt.cause == other {
				other = "holds no image pair"
			}
			if strings.Contains(err.Error(), other) {
				t.Fatalf("the refusal %q also names %q: the run checked them in the other order", err, other)
			}
		})
	}
}

// filledRenderFlagsRecord is a recorded flag set with a value in every field,
// built from the record's own fields so a field added to it is filled here too.
func filledRenderFlagsRecord(t *testing.T) renderFlagsRecord {
	t.Helper()
	var r renderFlagsRecord
	value := reflect.ValueOf(&r).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(strings.ToLower(value.Type().Field(i).Name))
		case reflect.Bool:
			field.SetBool(true)
		default:
			t.Fatalf("renderFlagsRecord.%s is a %s, which this test cannot fill", value.Type().Field(i).Name, field.Kind())
		}
	}
	return r
}

// TestRenderFlagsWithRecordedCoversTheFlagStruct: with nothing named on the
// command line, every flag of the recorded set is the replay's, including a flag
// the record file has no field for. A flag added to the struct and not to
// withRecorded fails here, since a replay would render it empty.
func TestRenderFlagsWithRecordedCoversTheFlagStruct(t *testing.T) {
	t.Parallel()
	full := filledRenderFlagsRecord(t).flags()
	// The record fill leaves a field zero when the record has no field for it, and
	// a zero expectation cannot see withRecorded drop it. Each such field is set
	// here by name, so a field added to the struct and left out of the record fails
	// here until it has its case.
	filledValue := reflect.ValueOf(full)
	for i := 0; i < filledValue.NumField(); i++ {
		if !filledValue.Field(i).IsZero() {
			continue
		}
		switch name := filledValue.Type().Field(i).Name; name {
		case "daemonBdGatewayURLSet":
			// The loader answers it true for a recorded URL (renderFlagsRecord.flags).
			full.daemonBdGatewayURLSet = true
		default:
			t.Fatalf("clusterRenderFlags.%s is zero after the record fill, so the record has no field for it and nothing here would see withRecorded drop it", name)
		}
	}
	got, err := (clusterRenderFlags{}).withRecorded(full, func(string) bool { return false })
	if err != nil {
		t.Fatalf("withRecorded with nothing named: %v", err)
	}
	if got == full {
		return
	}
	fullValue, gotValue := reflect.ValueOf(full), reflect.ValueOf(got)
	flagType := fullValue.Type()
	for i := 0; i < flagType.NumField(); i++ {
		name := flagType.Field(i).Name
		switch want, g := fullValue.Field(i), gotValue.Field(i); want.Kind() {
		case reflect.String:
			if want.String() != g.String() {
				t.Errorf("withRecorded drops %s: nothing was named, so the record's %q should be the replay's, got %q", name, want.String(), g.String())
			}
		case reflect.Bool:
			if want.Bool() != g.Bool() {
				t.Errorf("withRecorded drops %s: nothing was named, so the record's %v should be the replay's, got %v", name, want.Bool(), g.Bool())
			}
		}
	}
}

// TestRenderFlagsWithRecordedKeepsWhatWasNamed: a flag named on the command line
// keeps the value it was given, even an empty one, which is then refused as empty
// rather than filled in from the file.
func TestRenderFlagsWithRecordedKeepsWhatWasNamed(t *testing.T) {
	t.Parallel()
	recorded := clusterRenderFlags{dir: "/recorded", cpu: "2", memory: "4Gi", rehearsal: true}
	named := clusterRenderFlags{cpu: "", memory: "8Gi", rehearsal: false}
	got, err := named.withRecorded(recorded, func(flag string) bool {
		return flag == "cpu" || flag == "memory" || flag == "rehearsal"
	})
	if err != nil {
		t.Fatalf("withRecorded with the named flags answered: %v", err)
	}
	if got.cpu != "" {
		t.Errorf("--cpu named empty = %q, want the empty value kept, not the recorded %q", got.cpu, recorded.cpu)
	}
	if got.rehearsal {
		t.Error("--rehearsal named false is true again, so the recorded value won over the command line")
	}
	if got.memory != "8Gi" {
		t.Errorf("--memory = %q, want the named 8Gi", got.memory)
	}
	if got.dir != "/recorded" {
		t.Errorf("--dir = %q, want the unnamed flag's recorded /recorded", got.dir)
	}
}
