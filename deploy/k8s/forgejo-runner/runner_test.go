package forgejorunner

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// updatingGoldens reports whether -update was passed. The flag is registered in
// TestMain, so no test can change it while another reads it: a run without it
// compares the goldens, so a change to the rendered output fails loudly (gt-67qco).
func updatingGoldens() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// goldenValues is the configuration the goldens record.
func goldenValues() Values {
	return Values{
		Labels: []string{
			"docker-cli:docker://data.forgejo.org/oci/docker:28.5-cli",
			"node:docker://data.forgejo.org/oci/node:20.19",
			"ci-base:docker://data.forgejo.org/oci/gastown-runner:go1.27-node20",
		},
		ForgejoHostIP: "192.168.5.2",
	}
}

func mustBuild(t *testing.T, v Values) Manifests {
	t.Helper()
	m, err := Build(v)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}

// compareGolden compares got with testdata's file, or rewrites the file when
// -update is set.
func compareGolden(t *testing.T, file string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", file)
	if updatingGoldens() {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if msg := goldenMismatch(path, got, want); msg != "" {
		t.Error(msg)
	}
}

// goldenMismatch returns the message a golden comparison reports, or "" when got
// is exactly want.
func goldenMismatch(path string, got, want []byte) string {
	if bytes.Equal(got, want) {
		return ""
	}
	return fmt.Sprintf("%s does not match the renderer; re-run with -update if the change is intended\n--- %s\n%s--- renderer\n%s", path, path, want, got)
}

func containerNamed(t *testing.T, pod PodSpec, name string) Container {
	t.Helper()
	for _, c := range pod.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod has no container %q", name)
	return Container{}
}

func volumeNamed(t *testing.T, volumes []Volume, name string) Volume {
	t.Helper()
	for _, v := range volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("the pod has no volume %q", name)
	return Volume{}
}

// optionMounts returns the bind mounts a container.options string carries, keyed
// by source. It reads the string the runner hands to the engine, so a mount
// built with the wrong target is reported rather than matched as a prefix.
func optionMounts(t *testing.T, options string) map[string]string {
	t.Helper()
	fields := strings.Fields(options)
	mounts := make(map[string]string)
	for i, field := range fields {
		if field != "-v" {
			continue
		}
		if i+1 >= len(fields) {
			t.Fatalf("container.options %q ends with a -v and no mount", options)
		}
		src, dst, ok := strings.Cut(fields[i+1], ":")
		if !ok {
			t.Fatalf("container.options %q carries %q, which has no destination", options, fields[i+1])
		}
		mounts[src] = dst
	}
	return mounts
}

// configDoc is config.yml as the test reads it back: a shape separate from the
// renderer's, so a renamed or dropped setting fails the read.
type configDoc struct {
	Runner struct {
		File            string            `yaml:"file"`
		Capacity        int               `yaml:"capacity"`
		Envs            map[string]string `yaml:"envs"`
		Timeout         string            `yaml:"timeout"`
		ShutdownTimeout string            `yaml:"shutdown_timeout"`
		Labels          []string          `yaml:"labels"`
	} `yaml:"runner"`
	Container struct {
		Network      string   `yaml:"network"`
		Options      string   `yaml:"options"`
		ValidVolumes []string `yaml:"valid_volumes"`
	} `yaml:"container"`
}

func decodeConfig(t *testing.T, m Manifests) configDoc {
	t.Helper()
	raw := m.ConfigMap.Data[configKey]
	var doc configDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("config.yml is not valid YAML: %v\n%s", err, raw)
	}
	return doc
}

func TestRenderDeploymentGolden(t *testing.T) {
	t.Parallel()
	v := goldenValues()
	m := mustBuild(t, v)
	var buf bytes.Buffer
	if err := YAML(&buf, m.Deployment); err != nil {
		t.Fatalf("YAML: %v", err)
	}
	compareGolden(t, "deployment.yaml", buf.Bytes())

	pod := m.Deployment.Spec.Template.Spec
	if pod.PriorityClassName != PriorityClassName {
		t.Errorf("priorityClassName = %q, want %q", pod.PriorityClassName, PriorityClassName)
	}
	if len(pod.HostAliases) != 1 {
		t.Fatalf("the pod has %d host aliases, want one", len(pod.HostAliases))
	}
	alias := pod.HostAliases[0]
	if alias.IP != v.ForgejoHostIP || !slices.Equal(alias.Hostnames, []string{ForgejoHostname}) {
		t.Errorf("hostAlias = %+v, want %s -> %v", alias, v.ForgejoHostIP, []string{ForgejoHostname})
	}
	if len(pod.Containers) != 2 {
		t.Fatalf("the pod has %d containers, want the runner and the engine", len(pod.Containers))
	}
	runner := containerNamed(t, pod, runnerContainerName)
	dind := containerNamed(t, pod, dindContainerName)
	if want := "data.forgejo.org/forgejo/runner:13.2.0"; runner.Image != want {
		t.Errorf("runner image = %q, want %q", runner.Image, want)
	}
	if !strings.HasSuffix(runner.Image, ":"+Version) {
		t.Errorf("runner image = %q, want it tagged with Version %q", runner.Image, Version)
	}
	if want := "data.forgejo.org/oci/docker:dind"; dind.Image != want {
		t.Errorf("engine image = %q, want %q", dind.Image, want)
	}
	if dind.SecurityContext == nil || !dind.SecurityContext.Privileged {
		t.Errorf("engine securityContext = %+v, want it privileged", dind.SecurityContext)
	}
	if runner.SecurityContext != nil && runner.SecurityContext.Privileged {
		t.Error("the runner container is privileged; the engine is the pod's only privileged container")
	}
}

// TestRenderDeploymentRecreates drives the update strategy: Recreate stops the
// old pod before it starts the new one, so a rollout never runs two engines on
// one dind data claim or two runners on one registration file.
func TestRenderDeploymentRecreates(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	if got := m.Deployment.Spec.Strategy.Type; got != Recreate {
		t.Errorf("strategy type = %q, want %q", got, Recreate)
	}

	// The golden is what an operator applies, so it must carry the strategy too.
	path := filepath.Join("testdata", "deployment.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var golden Deployment
	if err := yaml.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	if got := golden.Spec.Strategy.Type; got != Recreate {
		t.Errorf("%s sets strategy type %q, want %q", path, got, Recreate)
	}
}

func TestRenderConfigMapGolden(t *testing.T) {
	t.Parallel()
	v := goldenValues()
	m := mustBuild(t, v)
	compareGolden(t, "config.yaml", []byte(m.ConfigMap.Data[configKey]))

	if want := Name + "-config"; m.ConfigMap.Metadata.Name != want {
		t.Errorf("ConfigMap name = %q, want %q", m.ConfigMap.Metadata.Name, want)
	}
	doc := decodeConfig(t, m)
	if doc.Runner.Capacity != DefaultCapacity {
		t.Errorf("capacity = %d, want the default %d", doc.Runner.Capacity, DefaultCapacity)
	}
	if !slices.Equal(doc.Runner.Labels, v.Labels) {
		t.Errorf("labels = %q, want the Values' %q", doc.Runner.Labels, v.Labels)
	}
	if doc.Runner.Timeout != RunnerTimeout {
		t.Errorf("timeout = %q, want %q", doc.Runner.Timeout, RunnerTimeout)
	}
	if doc.Runner.ShutdownTimeout != ShutdownTimeout {
		t.Errorf("shutdown_timeout = %q, want %q", doc.Runner.ShutdownTimeout, ShutdownTimeout)
	}
	if doc.Container.Network != containerNetwork {
		t.Errorf("container.network = %q, want %q", doc.Container.Network, containerNetwork)
	}

	t.Run("capacity and labels come from Values", func(t *testing.T) {
		t.Parallel()
		values := goldenValues()
		values.Capacity = 3
		values.Labels = []string{"gate:docker://example:1"}
		doc := decodeConfig(t, mustBuild(t, values))
		if doc.Runner.Capacity != 3 {
			t.Errorf("capacity = %d, want the Values' 3", doc.Runner.Capacity)
		}
		if !slices.Equal(doc.Runner.Labels, values.Labels) {
			t.Errorf("labels = %q, want %q", doc.Runner.Labels, values.Labels)
		}
	})

	t.Run("options mount the three cache volumes", func(t *testing.T) {
		t.Parallel()
		doc := decodeConfig(t, m)
		for _, mount := range cacheMounts {
			if want := "-v " + mount.volume + ":" + mount.path; !strings.Contains(doc.Container.Options, want) {
				t.Errorf("container.options %q does not mount %q", doc.Container.Options, want)
			}
		}
		if want := "--add-host=" + ForgejoHostname + ":" + v.ForgejoHostIP; !strings.Contains(doc.Container.Options, want) {
			t.Errorf("container.options %q does not carry %q", doc.Container.Options, want)
		}
		if want := []string{gomodCacheVolume, gocacheVolume, npmCacheVolume, certsClientDir}; !slices.Equal(doc.Container.ValidVolumes, want) {
			t.Errorf("valid_volumes = %q, want %q", doc.Container.ValidVolumes, want)
		}
	})
}

// TestDindArgsInsecureRegistry drives the engine argument: a job's image comes
// from Forgejo's package registry, which speaks plain HTTP, so dockerd must be
// told that registry is insecure.
func TestDindArgsInsecureRegistry(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, goldenValues()).Deployment.Spec.Template.Spec
	dind := containerNamed(t, pod, dindContainerName)
	if want := "--insecure-registry=forgejo:3000"; !slices.Contains(dind.Args, want) {
		t.Errorf("the engine args = %q, want them to carry %q", dind.Args, want)
	}
	// The runner's entrypoint is forgejo-runner, which has no such flag: the
	// argument belongs to the engine alone.
	if args := containerNamed(t, pod, runnerContainerName).Args; len(args) != 0 {
		t.Errorf("the runner args = %q, want none", args)
	}
}

// TestGomodMountsAtGoPath drives the module cache mount: the volume mounts the
// Go path the image owns, not a directory inside it, so the job user can write
// its module cache.
func TestGomodMountsAtGoPath(t *testing.T) {
	t.Parallel()
	doc := decodeConfig(t, mustBuild(t, goldenValues()))
	mounts := optionMounts(t, doc.Container.Options)
	if got, want := mounts[gomodCacheVolume], "/home/ci/go"; got != want {
		t.Errorf("container.options mounts %s at %q, want %q", gomodCacheVolume, got, want)
	}
	for src, dst := range mounts {
		if dst == "/home/ci/go/pkg/mod" || strings.HasPrefix(dst, "/home/ci/go/") {
			t.Errorf("container.options mounts %s at %q, inside the Go path", src, dst)
		}
	}
}

// TestRunnerEnvsReachEngine drives runner.envs: they are how a job step learns
// where the engine listens and which client certificate to present.
func TestRunnerEnvsReachEngine(t *testing.T) {
	t.Parallel()
	doc := decodeConfig(t, mustBuild(t, goldenValues()))
	want := map[string]string{
		"DOCKER_HOST":       "tcp://localhost:2376",
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_CERT_PATH":  "/certs/client",
	}
	for name, value := range want {
		if got := doc.Runner.Envs[name]; got != value {
			t.Errorf("runner.envs %s = %q, want %q", name, got, value)
		}
	}
	if len(doc.Runner.Envs) != len(want) {
		t.Errorf("runner.envs = %q, want exactly %d entries", doc.Runner.Envs, len(want))
	}
}

// TestJobOptionsInitAndCerts drives the job container's own settings: an init
// process, the engine's client certificates mounted where DOCKER_CERT_PATH
// points, and that path allowed as a volume.
func TestJobOptionsInitAndCerts(t *testing.T) {
	t.Parallel()
	doc := decodeConfig(t, mustBuild(t, goldenValues()))
	if !slices.Contains(strings.Fields(doc.Container.Options), "--init") {
		t.Errorf("container.options %q does not carry --init", doc.Container.Options)
	}
	if got := optionMounts(t, doc.Container.Options)["/certs/client"]; got != "/certs/client" {
		t.Errorf("container.options mounts /certs/client at %q, want %q", got, "/certs/client")
	}
	if !slices.Contains(doc.Container.ValidVolumes, "/certs/client") {
		t.Errorf("valid_volumes = %q, want it to allow %q", doc.Container.ValidVolumes, "/certs/client")
	}
}

// TestGoldenMismatchIsReported drives the exact comparison the golden tests use:
// a document that differs from the golden by one line must be reported, so a
// hand-edited golden cannot pass.
func TestGoldenMismatchIsReported(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := YAML(&buf, mustBuild(t, goldenValues()).Deployment); err != nil {
		t.Fatalf("YAML: %v", err)
	}
	want := buf.Bytes()
	path := filepath.Join("testdata", "deployment.yaml")
	if msg := goldenMismatch(path, want, want); msg != "" {
		t.Fatalf("the comparison reported %q for identical documents", msg)
	}
	changed := bytes.Replace(want, []byte("replicas: 1"), []byte("replicas: 2"), 1)
	if bytes.Equal(changed, want) {
		t.Fatalf("the deployment carries none of the line this test changes")
	}
	if msg := goldenMismatch(path, changed, want); !strings.Contains(msg, path) {
		t.Errorf("a changed golden produced %q, want a mismatch naming %s", msg, path)
	}
}

func TestRenderClaimsAndLimits(t *testing.T) {
	t.Parallel()
	v := goldenValues()
	m := mustBuild(t, v)

	checkClaimGolden(t, m.DindClaim, "dind-claim.yaml")
	checkClaimGolden(t, m.RunnerClaim, "runner-claim.yaml")

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			claim PersistentVolumeClaim
			size  string
		}{
			{"engine", m.DindClaim, DefaultDindStorage},
			{"runner data", m.RunnerClaim, DefaultRunnerStorage},
		} {
			if got := tc.claim.Spec.Resources.Requests["storage"]; got != tc.size {
				t.Errorf("the %s claim requests %q, want the default %q", tc.name, got, tc.size)
			}
		}
		pod := m.Deployment.Spec.Template.Spec
		for _, tc := range []struct {
			name string
			got  ResourceList
			want ResourceList
		}{
			{"runner", containerNamed(t, pod, runnerContainerName).Resources.Requests, ResourceList{CPU: DefaultRunnerCPU, Memory: DefaultRunnerMemory}},
			{"engine", containerNamed(t, pod, dindContainerName).Resources.Requests, ResourceList{CPU: DefaultDindCPU, Memory: DefaultDindMemory}},
		} {
			if tc.got != tc.want {
				t.Errorf("the %s container requests %+v, want %+v", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("requests equal limits", func(t *testing.T) {
		t.Parallel()
		pod := m.Deployment.Spec.Template.Spec
		for _, c := range pod.Containers {
			if c.Resources.Requests != c.Resources.Limits {
				t.Errorf("container %q requests %+v but limits %+v", c.Name, c.Resources.Requests, c.Resources.Limits)
			}
		}
	})

	t.Run("shares and claim sizes come from Values", func(t *testing.T) {
		t.Parallel()
		values := goldenValues()
		values.DindStorage = "80Gi"
		values.RunnerStorage = "2Gi"
		values.RunnerResources = Share{CPU: "2", Memory: "3Gi"}
		values.DindResources = Share{CPU: "8", Memory: "16Gi"}
		m := mustBuild(t, values)
		if got := m.DindClaim.Spec.Resources.Requests["storage"]; got != "80Gi" {
			t.Errorf("the engine claim requests %q, want 80Gi", got)
		}
		if got := m.RunnerClaim.Spec.Resources.Requests["storage"]; got != "2Gi" {
			t.Errorf("the runner data claim requests %q, want 2Gi", got)
		}
		pod := m.Deployment.Spec.Template.Spec
		if got, want := containerNamed(t, pod, runnerContainerName).Resources.Requests, (ResourceList{CPU: "2", Memory: "3Gi"}); got != want {
			t.Errorf("the runner container requests %+v, want %+v", got, want)
		}
		if got, want := containerNamed(t, pod, dindContainerName).Resources.Requests, (ResourceList{CPU: "8", Memory: "16Gi"}); got != want {
			t.Errorf("the engine container requests %+v, want %+v", got, want)
		}
	})

	t.Run("values the deployment cannot start with are refused", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			mutate func(*Values)
			field  string
		}{
			{"no labels", func(v *Values) { v.Labels = nil }, "Values.Labels"},
			{"no Forgejo host IP", func(v *Values) { v.ForgejoHostIP = "" }, "Values.ForgejoHostIP"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				values := goldenValues()
				tc.mutate(&values)
				_, err := Build(values)
				if err == nil {
					t.Fatalf("Build accepted %+v", values)
				}
				if !strings.Contains(err.Error(), tc.field) {
					t.Errorf("error = %q, want it to name %q", err, tc.field)
				}
			})
		}
	})
}

// checkClaimGolden writes claim as YAML and compares it with testdata's file.
func checkClaimGolden(t *testing.T, claim PersistentVolumeClaim, file string) {
	t.Helper()
	var buf bytes.Buffer
	if err := YAML(&buf, claim); err != nil {
		t.Fatalf("YAML(%s): %v", file, err)
	}
	compareGolden(t, file, buf.Bytes())
}

// secretWords are names a credential-bearing field is given in a manifest.
var secretWords = []string{"token", "password", "secret", "registration"}

// secretShapes are the forms a credential's value takes in rendered text. A
// value is what leaks; a word only says a manifest talks about credentials.
var secretShapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a 40-character hex token", regexp.MustCompile(`\b[0-9a-fA-F]{40}\b`)},
	{"a JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.`)},
	{"a PEM private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a base64 run of 32 characters or more", regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}`)},
}

// scanSecretText reports every credential word and every credential shape in
// text, naming the rule rather than echoing the value: a failure message must
// not print the credential it found.
func scanSecretText(text string) []string {
	var findings []string
	lower := strings.ToLower(text)
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			findings = append(findings, fmt.Sprintf("the word %q", word))
		}
	}
	for _, shape := range secretShapes {
		if shape.re.MatchString(text) {
			findings = append(findings, shape.name)
		}
	}
	return findings
}

// TestSecretScanFindsCredentials drives the scan: a text holding a registration
// token, a JWT and a private key must be reported, and the text a manifest
// legitimately carries must not.
func TestSecretScanFindsCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		text     string
		findings []string
	}{
		{
			name: "credentials",
			text: "registration_token: 0123456789abcdef0123456789abcdef01234567\nclient: eyJhbGciOiJIUzI1NiJ9.payload\n" +
				"key: -----BEGIN RSA PRIVATE KEY-----\n",
			findings: []string{
				`the word "registration"`,
				`the word "token"`,
				"a 40-character hex token",
				"a JWT",
				"a PEM private key",
				"a base64 run of 32 characters or more",
			},
		},
		{name: "a manifest's own text", text: "name: forgejo-runner\nimage: data.forgejo.org/forgejo/runner:13.2.0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanSecretText(tc.text)
			for _, want := range tc.findings {
				if !slices.Contains(got, want) {
					t.Errorf("the scan reported %q, want it to report %q", got, want)
				}
			}
			if len(tc.findings) == 0 && len(got) != 0 {
				t.Errorf("the scan reported %q for text that carries no credential", got)
			}
		})
	}
}

func TestNoSecretInRender(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := buf.String()
	if findings := scanSecretText(text); len(findings) != 0 {
		t.Errorf("the rendered manifests carry %s; the registration token never reaches a manifest", strings.Join(findings, ", "))
	}
	if strings.Contains(text, "kind: Secret") {
		t.Error("the stream holds a Secret; the renderer has no secret value to put in one")
	}

	// Every environment variable is one of the four below, so a credential
	// cannot arrive through an env var by default: a new one is a deliberate
	// edit here as well as in the renderer.
	allowedEnv := []string{"DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS_CERTDIR"}
	for _, c := range m.Deployment.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if !slices.Contains(allowedEnv, env.Name) {
				t.Errorf("container %q sets the unvetted environment variable %q", c.Name, env.Name)
			}
		}
	}

	// The registration result is written under the data mount: the only mount
	// the runner can write to, and a claim, so it survives a pod restart and no
	// rendered object carries it.
	pod := m.Deployment.Spec.Template.Spec
	runner := containerNamed(t, pod, runnerContainerName)
	var data *VolumeMount
	for i := range runner.VolumeMounts {
		mount := runner.VolumeMounts[i]
		if mount.MountPath == runnerDataDir {
			data = &runner.VolumeMounts[i]
			continue
		}
		if !mount.ReadOnly {
			t.Errorf("the runner can write to %s; the data mount is its only writable mount", mount.MountPath)
		}
	}
	if data == nil {
		t.Fatalf("the runner mounts nothing at %s", runnerDataDir)
	}
	volume := volumeNamed(t, pod.Volumes, data.Name)
	if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != Name+"-data" {
		t.Errorf("the data mount is backed by %+v, want the %s-data claim", volume, Name)
	}
}

func TestImagesArePinned(t *testing.T) {
	t.Parallel()
	if Version != "13.2.0" {
		t.Errorf("Version = %q, want the pinned 13.2.0", Version)
	}
	if Image != "data.forgejo.org/forgejo/runner:13.2.0" {
		t.Errorf("Image = %q, want the runner at the pinned version", Image)
	}
	if DindImage != "data.forgejo.org/oci/docker:dind" {
		t.Errorf("DindImage = %q, want the registry's dind tag", DindImage)
	}
	pod := mustBuild(t, goldenValues()).Deployment.Spec.Template.Spec
	if got := containerNamed(t, pod, dindContainerName).Image; got != DindImage {
		t.Errorf("engine image = %q, want %q", got, DindImage)
	}
}

// failingWriter refuses every write, standing in for a full disk or a closed
// pipe at the moment the manifest is written.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRenderReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	refused := errors.New("the disk is full")
	for _, tc := range []struct {
		name string
		call func(w io.Writer) error
	}{
		{"one document", func(w io.Writer) error { return YAML(w, m.Deployment) }},
		{"the stream", func(w io.Writer) error { return Render(w, m) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(failingWriter{err: refused})
			if err == nil {
				t.Fatal("the write was refused, but no error came back")
			}
			for _, want := range []string{"encoding the manifest", refused.Error()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
			// Close can report the write failure Encode already returned, so the
			// cause is counted: a reader debugging a failed write needs it once.
			if n := strings.Count(err.Error(), refused.Error()); n != 1 {
				t.Errorf("error = %q, names the cause %d times, want once", err, n)
			}
		})
	}
}

func TestRenderEmitsTheFourObjectsInOrder(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	docs := bytes.Split(buf.Bytes(), []byte("\n---\n"))
	if len(docs) != 4 {
		t.Fatalf("the stream holds %d documents, want four:\n%s", len(docs), buf.Bytes())
	}
	kinds := []string{
		"kind: PersistentVolumeClaim",
		"kind: PersistentVolumeClaim",
		"kind: ConfigMap",
		"kind: Deployment",
	}
	for i, kind := range kinds {
		if !bytes.Contains(docs[i], []byte(kind)) {
			t.Errorf("document %d does not contain %q", i+1, kind)
		}
	}
}

func TestObjectsDoNotShareLabelMaps(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	m.DindClaim.Metadata.Labels["edited"] = "true"
	for name, labels := range map[string]map[string]string{
		"runner claim": m.RunnerClaim.Metadata.Labels,
		"ConfigMap":    m.ConfigMap.Metadata.Labels,
		"Deployment":   m.Deployment.Metadata.Labels,
		"pod template": m.Deployment.Spec.Template.Metadata.Labels,
	} {
		if _, ok := labels["edited"]; ok {
			t.Errorf("editing the engine claim's labels also edited the %s's", name)
		}
	}
	if _, ok := m.Deployment.Spec.Selector.MatchLabels["edited"]; ok {
		t.Error("editing the engine claim's labels also edited the Deployment's selector")
	}
}

func TestNamespaceDefaultsToForgejo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, given, want string }{
		{"default", "", DefaultNamespace},
		{"stated", "runner-rehearsal", "runner-rehearsal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues()
			v.Namespace = tc.given
			m := mustBuild(t, v)
			for _, namespace := range []string{
				m.DindClaim.Metadata.Namespace,
				m.RunnerClaim.Metadata.Namespace,
				m.ConfigMap.Metadata.Namespace,
				m.Deployment.Metadata.Namespace,
			} {
				if namespace != tc.want {
					t.Errorf("namespace = %q, want %q", namespace, tc.want)
				}
			}
		})
	}
}

func TestValuesReachTheManifests(t *testing.T) {
	t.Parallel()
	v := Values{
		Namespace:     "runner-rehearsal",
		Labels:        []string{"gate:docker://gate:1"},
		Capacity:      4,
		ForgejoHostIP: "10.0.0.7",
		DindStorage:   "80Gi",
		RunnerStorage: "2Gi",
	}
	m := mustBuild(t, v)
	if got := m.Deployment.Metadata.Namespace; got != v.Namespace {
		t.Errorf("Deployment namespace = %q, want %q", got, v.Namespace)
	}
	if got := m.DindClaim.Spec.Resources.Requests["storage"]; got != v.DindStorage {
		t.Errorf("engine storage = %q, want %q", got, v.DindStorage)
	}
	if got := m.RunnerClaim.Spec.Resources.Requests["storage"]; got != v.RunnerStorage {
		t.Errorf("runner storage = %q, want %q", got, v.RunnerStorage)
	}
	pod := m.Deployment.Spec.Template.Spec
	if got := pod.HostAliases[0].IP; got != v.ForgejoHostIP {
		t.Errorf("host alias IP = %q, want %q", got, v.ForgejoHostIP)
	}
	doc := decodeConfig(t, m)
	if doc.Runner.Capacity != v.Capacity {
		t.Errorf("capacity = %d, want %d", doc.Runner.Capacity, v.Capacity)
	}
	if !strings.Contains(doc.Container.Options, "--add-host="+ForgejoHostname+":"+v.ForgejoHostIP) {
		t.Errorf("container.options %q does not name the host alias IP %q", doc.Container.Options, v.ForgejoHostIP)
	}
}

func TestBuildRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Values)
		want   string
	}{
		{"namespace is not a label", func(v *Values) { v.Namespace = "Runner_Rehearsal" }, "namespace"},
		{"labels are empty", func(v *Values) { v.Labels = nil }, "Values.Labels"},
		{"a label is empty", func(v *Values) { v.Labels = []string{"gate:docker://gate:1", ""} }, "Values.Labels[1]"},
		{"a label has whitespace", func(v *Values) { v.Labels = []string{"gate label"} }, "whitespace"},
		{"a label names no image", func(v *Values) { v.Labels = []string{"gate"} }, "Values.Labels[0]"},
		{"a label name is empty", func(v *Values) { v.Labels = []string{":docker://gate:1"} }, "Values.Labels[0]"},
		{"a label image is empty", func(v *Values) { v.Labels = []string{"gate:docker://"} }, "Values.Labels[0]"},
		{"host IP is empty", func(v *Values) { v.ForgejoHostIP = "" }, "Values.ForgejoHostIP"},
		{"host IP is a name", func(v *Values) { v.ForgejoHostIP = "forgejo.internal" }, "dotted IPv4"},
		{"host IP has whitespace", func(v *Values) { v.ForgejoHostIP = "192.168.5.2 " }, "whitespace"},
		{"host IP octet is over 255", func(v *Values) { v.ForgejoHostIP = "192.168.5.256" }, "dotted IPv4"},
		{"capacity is negative", func(v *Values) { v.Capacity = -1 }, "Values.Capacity"},
		{"runner CPU is not a quantity", func(v *Values) { v.RunnerResources.CPU = "one" }, "Values.RunnerResources.CPU"},
		{"runner CPU is zero", func(v *Values) { v.RunnerResources.CPU = "0" }, "Values.RunnerResources.CPU"},
		{"engine CPU is negative", func(v *Values) { v.DindResources.CPU = "-1" }, "Values.DindResources.CPU"},
		{"engine memory has no suffix", func(v *Values) { v.DindResources.Memory = "12" }, "Values.DindResources.Memory"},
		{"engine storage is not a quantity", func(v *Values) { v.DindStorage = "40GB" }, "engine claim storage"},
		{"engine storage is zero", func(v *Values) { v.DindStorage = "0Gi" }, "engine claim storage"},
		{"runner storage has no suffix", func(v *Values) { v.RunnerStorage = "10" }, "runner data claim storage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues()
			tc.mutate(&v)
			_, err := Build(v)
			if err == nil {
				t.Fatalf("Build accepted %+v", v)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestConfigYAMLQuotesAnyLabel drives the scalar quoting: a label holding a
// single quote and a YAML comment character must read back as the label it was,
// not as a document a reader would have to repair.
func TestConfigYAMLQuotesAnyLabel(t *testing.T) {
	t.Parallel()
	v := goldenValues()
	v.Labels = []string{"gate:docker://gate:1", "a'b#c:docker://gate:1"}
	doc := decodeConfig(t, mustBuild(t, v))
	if !slices.Equal(doc.Runner.Labels, v.Labels) {
		t.Errorf("labels read back as %q, want %q", doc.Runner.Labels, v.Labels)
	}
}
