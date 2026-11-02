package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// goldenDir holds the renderer package's goldens: the flags below are the
// Values it records, so gaterunner's output must reproduce those files.
const goldenDir = "../../deploy/k8s/forgejo-runner/testdata"

// goldenArgs are the flags matching the renderer's golden Values.
func goldenArgs() []string {
	return []string{
		"-label", "docker-cli:docker://data.forgejo.org/oci/docker:28.5-cli",
		"-label", "node:docker://data.forgejo.org/oci/node:20.19",
		"-label", "ci-base:docker://data.forgejo.org/oci/gastown-runner:go1.27-node20",
		"-forgejo-host-ip", "192.168.5.2",
	}
}

// runFor runs the command in-process and returns what it wrote and its code.
func runFor(t *testing.T, args []string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = run(args, &out, &errw)
	return out.String(), errw.String(), code
}

// documents splits the renderer's multi-document stream on the separator
// yaml.v3 writes between documents. No document's own text carries a line that
// starts with the separator: every value that reaches a document is validated
// (a label refuses whitespace, the host address is an IPv4 literal, sizes and
// shares match a one-line grammar, the namespace is a DNS label), so none can
// introduce a newline, and the embedded config.yml is an indented block.
func documents(t *testing.T, stream string) [][]byte {
	t.Helper()
	const separator = "\n---\n"
	var docs [][]byte
	rest := stream
	for {
		i := strings.Index(rest, separator)
		if i < 0 {
			return append(docs, []byte(rest))
		}
		docs = append(docs, []byte(rest[:i]+"\n"))
		rest = rest[i+len(separator):]
	}
}

// docRef is the part of a document that names it: its kind and object name.
type docRef struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

func docRefOf(t *testing.T, doc []byte) docRef {
	t.Helper()
	var ref docRef
	if err := yaml.Unmarshal(doc, &ref); err != nil {
		t.Fatalf("decoding a document: %v\n%s", err, doc)
	}
	return ref
}

func mustDecode(t *testing.T, doc []byte, into any) {
	t.Helper()
	if err := yaml.Unmarshal(doc, into); err != nil {
		t.Fatalf("decoding a document: %v\n%s", err, doc)
	}
}

func mustReadGolden(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(goldenDir, file))
	if err != nil {
		t.Fatalf("reading the renderer's golden %s: %v", file, err)
	}
	return raw
}

func TestRunRendersTheGoldenWithTheNamedFlags(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := runFor(t, goldenArgs())
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr is not empty: %q", stderr)
	}

	// Each golden is one document of the stream; comparing the document's raw
	// bytes holds the command to the renderer's exact output.
	goldens := map[string]string{
		"PersistentVolumeClaim/forgejo-runner-dind": "dind-claim.yaml",
		"PersistentVolumeClaim/forgejo-runner-data": "runner-claim.yaml",
		"Deployment/forgejo-runner":                 "deployment.yaml",
	}
	seen := map[string]bool{}
	for _, doc := range documents(t, stdout) {
		ref := docRefOf(t, doc)
		key := ref.Kind + "/" + ref.Metadata.Name
		if ref.Kind == "ConfigMap" {
			// The ConfigMap's golden records the config.yml it carries.
			var cm forgejorunner.ConfigMap
			mustDecode(t, doc, &cm)
			want := mustReadGolden(t, "config.yaml")
			if got := cm.Data["config.yml"]; got != string(want) {
				t.Errorf("the ConfigMap's config.yml does not match config.yaml byte for byte\n--- config.yaml\n%s--- gaterunner\n%s", want, got)
			}
			seen["ConfigMap/forgejo-runner-config"] = true
			continue
		}
		file, ok := goldens[key]
		if !ok {
			continue
		}
		want := mustReadGolden(t, file)
		if !bytes.Equal(doc, want) {
			t.Errorf("the %s document does not match %s byte for byte\n--- %s\n%s--- gaterunner\n%s", ref.Kind, file, file, want, doc)
		}
		seen[key] = true
	}
	wantSeen := []string{
		"PersistentVolumeClaim/forgejo-runner-dind",
		"PersistentVolumeClaim/forgejo-runner-data",
		"ConfigMap/forgejo-runner-config",
		"Deployment/forgejo-runner",
	}
	for _, key := range wantSeen {
		if !seen[key] {
			t.Errorf("stdout has no %s document", key)
		}
	}
}

func TestRunRefusesMissingRequiredFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"no -label", []string{"-forgejo-host-ip", "192.168.5.2"}, "-label"},
		{"no -forgejo-host-ip", []string{"-label", "gate:docker://example:1"}, "-forgejo-host-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runFor(t, tc.args)
			if code != 2 {
				t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantFlag) {
				t.Errorf("stderr does not name %s: %q", tc.wantFlag, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout is not empty: %q", stdout)
			}
		})
	}
}

func TestRunWritesNothingButYAMLToStdout(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := runFor(t, goldenArgs())
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr is not empty: %q", stderr)
	}

	dec := yaml.NewDecoder(strings.NewReader(stdout))
	var kinds []string
	for {
		var doc struct {
			Kind string `yaml:"kind"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stdout is not a YAML stream: %v\n%s", err, stdout)
		}
		kinds = append(kinds, doc.Kind)
	}
	want := []string{"PersistentVolumeClaim", "PersistentVolumeClaim", "ConfigMap", "Deployment"}
	if !slices.Equal(kinds, want) {
		t.Errorf("stdout holds documents of kinds %q, want %q", kinds, want)
	}
}

func TestRunAppliesTheOptionalFlags(t *testing.T) {
	t.Parallel()
	args := append(goldenArgs(),
		"-namespace", "ci",
		"-capacity", "4",
		"-dind-storage", "50Gi",
		"-runner-storage", "2Gi",
		"-runner-cpu", "500m",
		"-runner-memory", "2Gi",
		"-dind-cpu", "8",
		"-dind-memory", "16Gi",
	)
	stdout, stderr, code := runFor(t, args)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}

	var dindClaim, runnerClaim forgejorunner.PersistentVolumeClaim
	var configMap forgejorunner.ConfigMap
	var deployment forgejorunner.Deployment
	for _, doc := range documents(t, stdout) {
		ref := docRefOf(t, doc)
		switch ref.Kind + "/" + ref.Metadata.Name {
		case "PersistentVolumeClaim/forgejo-runner-dind":
			mustDecode(t, doc, &dindClaim)
		case "PersistentVolumeClaim/forgejo-runner-data":
			mustDecode(t, doc, &runnerClaim)
		case "ConfigMap/forgejo-runner-config":
			mustDecode(t, doc, &configMap)
		case "Deployment/forgejo-runner":
			mustDecode(t, doc, &deployment)
		}
	}

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"the Deployment namespace", deployment.Metadata.Namespace, "ci"},
		{"the engine claim namespace", dindClaim.Metadata.Namespace, "ci"},
		{"the runner claim namespace", runnerClaim.Metadata.Namespace, "ci"},
		{"the ConfigMap namespace", configMap.Metadata.Namespace, "ci"},
		{"the engine claim size", dindClaim.Spec.Resources.Requests["storage"], "50Gi"},
		{"the runner data claim size", runnerClaim.Spec.Resources.Requests["storage"], "2Gi"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	var config struct {
		Runner struct {
			Capacity int `yaml:"capacity"`
		} `yaml:"runner"`
	}
	if err := yaml.Unmarshal([]byte(configMap.Data["config.yml"]), &config); err != nil {
		t.Fatalf("decoding config.yml: %v", err)
	}
	if got := config.Runner.Capacity; got != 4 {
		t.Errorf("config.yml capacity = %d, want the -capacity 4", got)
	}

	containers := map[string]forgejorunner.Container{}
	for _, c := range deployment.Spec.Template.Spec.Containers {
		containers[c.Name] = c
	}
	for _, tc := range []struct {
		container string
		want      forgejorunner.Share
	}{
		{"runner", forgejorunner.Share{CPU: "500m", Memory: "2Gi"}},
		{"dind", forgejorunner.Share{CPU: "8", Memory: "16Gi"}},
	} {
		c, ok := containers[tc.container]
		if !ok {
			t.Fatalf("the Deployment has no %s container", tc.container)
		}
		for _, list := range []struct {
			name string
			got  forgejorunner.ResourceList
		}{
			{"requests", c.Resources.Requests},
			{"limits", c.Resources.Limits},
		} {
			if list.got.CPU != tc.want.CPU || list.got.Memory != tc.want.Memory {
				t.Errorf("the %s container's %s = %+v, want %+v", tc.container, list.name, list.got, tc.want)
			}
		}
	}
}

func TestRunRefusesBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"unknown flag",
			[]string{"-nope"},
			"-nope",
		},
		{
			"stray argument",
			append(goldenArgs(), "orphan.yaml"),
			"unexpected argument",
		},
		{
			"label with no image",
			[]string{"-label", "gate", "-forgejo-host-ip", "192.168.5.2"},
			"Values.Labels[0]",
		},
		{
			"host that is not an IPv4 address",
			[]string{"-label", "gate:docker://example:1", "-forgejo-host-ip", "forgejo"},
			"Values.ForgejoHostIP",
		},
		{
			"engine storage without a suffix",
			append(goldenArgs(), "-dind-storage", "40"),
			"engine claim storage",
		},
		{
			"runner CPU that is not a quantity",
			append(goldenArgs(), "-runner-cpu", "lots"),
			"Values.RunnerResources.CPU",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runFor(t, tc.args)
			if code != 2 {
				t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name %s: %q", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout is not empty: %q", stdout)
			}
		})
	}
}

// failingWriter refuses every write, as a closed pipe does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

func TestRunReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	if code := run(goldenArgs(), failingWriter{}, &errw); code != 1 {
		t.Errorf("exit %d, want 1 (stderr %q)", code, errw.String())
	}
	if !strings.Contains(errw.String(), "writing the manifests") {
		t.Errorf("stderr does not report the write failure: %q", errw.String())
	}
}
