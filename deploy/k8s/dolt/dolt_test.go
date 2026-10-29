package dolt

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGoldens records the manifests on purpose. Reading the goldens is the
// default, so a change to the rendered output fails loudly (gt-cfjn7).
var updateGoldens = flag.Bool("update", false, "rewrite testdata/ with the builder's current output")

// goldenConfig is the configuration the goldens record.
func goldenConfig() Config {
	return Config{
		Storage:            "10Gi",
		RootHost:           "%",
		RootPasswordSecret: "dolt-root",
	}
}

func TestManifestsMatchGoldens(t *testing.T) {
	t.Parallel()
	m, err := Build(goldenConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	objects := []struct {
		file string
		obj  any
	}{
		{"claim.yaml", m.Claim},
		{"service.yaml", m.Service},
		{"statefulset.yaml", m.StatefulSet},
	}
	for _, o := range objects {
		got, err := YAML(o.obj)
		if err != nil {
			t.Fatalf("YAML(%s): %v", o.file, err)
		}
		path := filepath.Join("testdata", o.file)
		if *updateGoldens {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s does not match the builder; re-run with -update if the change is intended\n--- %s\n%s--- builder\n%s", path, path, want, got)
		}
	}
}

func TestImagePinnedTo232(t *testing.T) {
	t.Parallel()
	if Version != "2.3.2" {
		t.Errorf("Version = %q, want the pinned 2.3.2", Version)
	}
	m, err := Build(goldenConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	container := m.StatefulSet.Spec.Template.Spec.Containers[0]
	if want := "dolthub/dolt-sql-server:2.3.2"; container.Image != want {
		t.Errorf("image = %q, want %q", container.Image, want)
	}
	if !strings.HasSuffix(container.Image, ":"+Version) {
		t.Errorf("image = %q, want it tagged with Version %q", container.Image, Version)
	}
}

func TestServiceExposesPort3307ForThePod(t *testing.T) {
	t.Parallel()
	m, err := Build(goldenConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m.Service.Spec.ClusterIP != "None" {
		t.Errorf("clusterIP = %q, want None: the Service is headless", m.Service.Spec.ClusterIP)
	}
	ports := m.Service.Spec.Ports
	if len(ports) != 1 {
		t.Fatalf("the Service publishes %d ports, want exactly one", len(ports))
	}
	port := ports[0]
	if port.Port != 3307 {
		t.Errorf("port = %d, want 3307", port.Port)
	}
	if port.Name != PortName {
		t.Errorf("port name = %q, want %q", port.Name, PortName)
	}
	container := m.StatefulSet.Spec.Template.Spec.Containers[0]
	if len(container.Ports) != 1 {
		t.Fatalf("the container declares %d ports, want exactly one", len(container.Ports))
	}
	cp := container.Ports[0]
	if port.TargetPort != cp.Name {
		t.Errorf("targetPort = %q, want the container port name %q", port.TargetPort, cp.Name)
	}
	if cp.ContainerPort != ContainerPort {
		t.Errorf("containerPort = %d, want %d", cp.ContainerPort, ContainerPort)
	}
	podLabels := m.StatefulSet.Spec.Template.Metadata.Labels
	for k, v := range m.Service.Spec.Selector {
		if podLabels[k] != v {
			t.Errorf("the Service selects %s=%s, which the pod template does not carry", k, v)
		}
	}
}

func TestRootPasswordIsAlwaysAReference(t *testing.T) {
	t.Parallel()
	c := goldenConfig()
	c.RootPasswordKey = "root-password"
	m, err := Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := m.StatefulSet.Spec.Template.Spec.Containers[0].Env
	var found bool
	for _, e := range env {
		if e.Name != "DOLT_ROOT_PASSWORD" {
			continue
		}
		found = true
		if e.Value != "" {
			t.Errorf("DOLT_ROOT_PASSWORD carries the value %q; root's password comes from a Secret", e.Value)
		}
		ref := e.ValueFrom
		if ref == nil || ref.SecretKeyRef == nil {
			t.Fatalf("DOLT_ROOT_PASSWORD = %+v, want a secretKeyRef", e)
		}
		if ref.SecretKeyRef.Name != c.RootPasswordSecret || ref.SecretKeyRef.Key != c.RootPasswordKey {
			t.Errorf("secretKeyRef = %+v, want %s/%s", ref.SecretKeyRef, c.RootPasswordSecret, c.RootPasswordKey)
		}
	}
	if !found {
		t.Fatal("the container sets no DOLT_ROOT_PASSWORD")
	}
}

func TestNamespaceDefaultsToOpenshell(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, given, want string }{
		{"default", "", DefaultNamespace},
		{"stated", "dolt-rehearsal", "dolt-rehearsal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := goldenConfig()
			c.Namespace = tc.given
			m, err := Build(c)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for _, namespace := range []string{m.Claim.Metadata.Namespace, m.Service.Metadata.Namespace, m.StatefulSet.Metadata.Namespace} {
				if namespace != tc.want {
					t.Errorf("namespace = %q, want %q", namespace, tc.want)
				}
			}
		})
	}
}

func TestConfigReachesTheManifests(t *testing.T) {
	t.Parallel()
	m, err := Build(Config{
		Namespace:          "dolt-rehearsal",
		Storage:            "5Gi",
		RootHost:           "localhost",
		RootPasswordSecret: "other-root",
		RootPasswordKey:    "db-password",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"namespace: dolt-rehearsal", "storage: 5Gi", "value: localhost", "name: other-root", "key: db-password"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the rendered manifests do not contain %q", want)
		}
	}
}

func TestBuildRefusesInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"namespace is not a label", func(c *Config) { c.Namespace = "Dolt_Rehearsal" }, "namespace"},
		{"storage is empty", func(c *Config) { c.Storage = "" }, "storage"},
		{"storage is not a quantity", func(c *Config) { c.Storage = "10GB" }, "storage"},
		{"storage has no suffix", func(c *Config) { c.Storage = "10" }, "storage"},
		{"storage is zero", func(c *Config) { c.Storage = "0Gi" }, "storage"},
		{"root host is empty", func(c *Config) { c.RootHost = "" }, "root host"},
		{"root host has whitespace", func(c *Config) { c.RootHost = "127.0.0.1, localhost" }, "root host"},
		{"password Secret is empty", func(c *Config) { c.RootPasswordSecret = "" }, "password Secret"},
		{"password Secret is not a label", func(c *Config) { c.RootPasswordSecret = "Dolt_Root" }, "password Secret"},
		{"password key is not a Secret key", func(c *Config) { c.RootPasswordKey = "root password" }, "password key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := goldenConfig()
			tc.mutate(&c)
			_, err := Build(c)
			if err == nil {
				t.Fatalf("Build accepted %+v", c)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestObjectsDoNotShareLabelMaps(t *testing.T) {
	t.Parallel()
	m, err := Build(goldenConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m.Claim.Metadata.Labels["edited"] = "true"
	for name, labels := range map[string]map[string]string{
		"Service":      m.Service.Metadata.Labels,
		"StatefulSet":  m.StatefulSet.Metadata.Labels,
		"pod template": m.StatefulSet.Spec.Template.Metadata.Labels,
	} {
		if _, ok := labels["edited"]; ok {
			t.Errorf("editing the claim's labels also edited the %s's", name)
		}
	}
	if _, ok := m.Service.Spec.Selector["edited"]; ok {
		t.Error("editing the claim's labels also edited the Service's selector")
	}
}

func TestRenderEmitsTheThreeObjectsInOrder(t *testing.T) {
	t.Parallel()
	m, err := Build(goldenConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	docs := bytes.Split(out, []byte("\n---\n"))
	if len(docs) != 3 {
		t.Fatalf("the stream holds %d documents, want three:\n%s", len(docs), out)
	}
	for i, kind := range []string{"kind: PersistentVolumeClaim", "kind: Service", "kind: StatefulSet"} {
		if !bytes.Contains(docs[i], []byte(kind)) {
			t.Errorf("document %d does not contain %q", i+1, kind)
		}
	}
}

func TestBuilderImportsNoApplyPath(t *testing.T) {
	t.Parallel()
	dir := packageDir(t)
	files := goFiles(t, dir)
	if len(files) == 0 {
		t.Fatal("the package holds no non-test Go file, so the import check read nothing")
	}
	for _, file := range files {
		for _, imp := range importsOf(t, filepath.Join(dir, file)) {
			if forbiddenImport(imp) {
				t.Errorf("%s imports %q: the builder renders manifests and reaches no cluster", file, imp)
			}
		}
	}
}
