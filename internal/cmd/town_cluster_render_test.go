package cmd

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
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

// clusterRenderTestDeps stands in the host: a fixed town, an empty daemon.env, a
// fixed home, and the real build and write steps against the temporary repo.
func clusterRenderTestDeps(out io.Writer) clusterRenderDeps {
	return clusterRenderDeps{
		findTown:     func() (string, error) { return "/fake/town", nil },
		containerEnv: func(string) (map[string]string, error) { return map[string]string{}, nil },
		readFile:     os.ReadFile,
		lstat:        os.Lstat,
		getenv:       func(string) string { return "" },
		userHome:     func() (string, error) { return "/fake/home", nil },
		build:        k8sdaemon.Build,
		render:       k8sdaemon.Render,
		buildHelper:  k8sdaemon.BuildHelper,
		encode:       k8sdaemon.YAML,
		mkdirAll:     func(path string) error { return os.MkdirAll(path, 0o755) },
		writeFile:    func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		out:          out,
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

// createLines returns the kubectl lines the render printed.
func createLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kubectl ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

func TestTownClusterRenderWritesTheObjectsInApplyOrder(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		deps := clusterRenderTestDeps(&out)
		deps.lstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
		err := runClusterRender(deps, f)
		requireErrorNames(t, err, k8sdaemon.CloudCheckKeyFile, "cannot be read")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left --out %s, want it untouched", got)
		}
	})
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
			err := runClusterRender(clusterRenderTestDeps(&out), f)
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
			d := clusterRenderTestDeps(&out)
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		_, err := secretCreateLines(mounts("daemon-x", nil), "gt", "/cfg", "/ossl")
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
		_, err := secretCreateLines(m, "gt", "/cfg", "/ossl")
		if err == nil || !strings.Contains(err.Error(), "names no files") {
			t.Fatalf("secretCreateLines = %v, want a refusal naming the empty openshell volume", err)
		}
	})
	t.Run("the mounted Secret has no source annotation", func(t *testing.T) {
		t.Parallel()
		m := mounts("daemon-x", nil)
		m.Secrets = []k8sdaemon.Secret{{Metadata: k8sdaemon.ObjectMeta{Name: "daemon-x"}}}
		lines, err := secretCreateLines(m, "gt", "/cfg", "/ossl")
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
	lines, err := secretCreateLines(m, "gt ns", "/cfg", "/ossl")
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
	if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		{"another directory", func(f *clusterRenderFlags) { f.doltBackupHostPath = "/Users/sloan/elsewhere" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			tt.mutate(&f)
			before := outState(f.out)
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(&out), f)
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
			if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		var off bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(&off), f); err != nil {
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
		if err := runClusterRender(clusterRenderTestDeps(&with), on); err != nil {
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
		err := runClusterRender(clusterRenderTestDeps(&out), f)
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
		err := runClusterRender(clusterRenderTestDeps(&out), f)
		requireErrorNames(t, err, "--dashboard", "--rehearsal")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("a relative claude directory refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.dashboardClaudeDir = "Users/sloan/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(&out), f)
		requireErrorNames(t, err, "--dashboard-claude-dir", "absolute")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("the claude directory without the dashboard refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboardClaudeDir = "/Users/sloan/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(&out), f)
		requireErrorNames(t, err, "--dashboard-claude-dir", "--dashboard")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("the claude directory is the dashboard's only host path", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.dashboardClaudeDir = "/Users/sloan/.claude"
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if got, want := renderedHostPaths(t, f.out, "-deployment-dashboard.yaml"), []string{"/Users/sloan/.claude Directory"}; !slices.Equal(got, want) {
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
		f.daemonClaudeDir = "/Users/sloan/.claude"
		var out bytes.Buffer
		deps := clusterRenderTestDeps(&out)
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
		if got, want := renderedHostPaths(t, f.out, "-deployment-daemon.yaml"), []string{"/Users/sloan/.claude Directory"}; !slices.Equal(got, want) {
			t.Errorf("the daemon's host paths = %q, want %q", got, want)
		}
	})

	t.Run("the flag off leaves the names and the daemon's host paths as they were", func(t *testing.T) {
		f := clusterRenderFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(&out), f); err != nil {
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
		f.daemonClaudeDir = "Users/sloan/.claude"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(&out), f)
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
