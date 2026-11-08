package daemon

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townstate"
)

// updatingGoldens reports whether -update was passed. The flag is registered in
// TestMain, so no test can change it while another reads it.
func updatingGoldens() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// goldenValues is the configuration the goldens record: the shipped secrets
// manifest's entries, so a manifest entry with no Secret fails the goldens.
func goldenValues(t *testing.T) Values {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(packageDir(t), "..", "..", "daemon", "secrets.yaml"))
	if err != nil {
		t.Fatalf("reading the secrets manifest: %v", err)
	}
	manifest, err := templates.ParseSecretsManifest(data)
	if err != nil {
		t.Fatalf("parsing the secrets manifest: %v", err)
	}
	return Values{
		ForgejoService:   DefaultForgejoService,
		ForgejoNamespace: DefaultForgejoNamespace,
		Secrets:          manifest.Files,
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

// goldenReporter is the part of testing.T compareGolden reports through, so a
// test can drive the function the golden tests use and observe the failure it
// produces.
type goldenReporter interface {
	Helper()
	Error(args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// compareGolden compares got with the golden file dir/file, or rewrites it when
// -update is set. The directory is a parameter so a test can drive this function
// against a file it owns instead of rewriting testdata.
func compareGolden(t goldenReporter, dir, file string, got []byte) {
	t.Helper()
	filePath := filepath.Join(dir, file)
	if updatingGoldens() {
		if err := os.WriteFile(filePath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", filePath, err)
		}
		return
	}
	want, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading %s: %v", filePath, err)
	}
	if msg := goldenMismatch(filePath, got, want); msg != "" {
		t.Error(msg)
	}
}

// goldenMismatch returns the message a golden comparison reports, or "" when got
// is exactly want.
func goldenMismatch(filePath string, got, want []byte) string {
	if bytes.Equal(got, want) {
		return ""
	}
	return fmt.Sprintf("%s does not match the renderer; re-run with -update if the change is intended\n--- %s\n%s--- renderer\n%s", filePath, filePath, want, got)
}

// document encodes one object as a single YAML document.
func document(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := YAML(&buf, v); err != nil {
		t.Fatalf("encoding the document: %v", err)
	}
	return buf.Bytes()
}

func TestRenderGolden(t *testing.T) {
	v := goldenValues(t)
	m := mustBuild(t, v)
	dir := filepath.Join(packageDir(t), "testdata")

	for _, doc := range []struct {
		file  string
		value any
	}{
		{"namespace.yaml", m.Namespace},
		{"town-state-claim.yaml", m.TownStateClaim},
		{"landing-work-claim.yaml", m.LandingWorkClaim},
		{"forgejo-service.yaml", m.ForgejoService},
	} {
		compareGolden(t, dir, doc.file, document(t, doc.value))
	}
	if len(m.Secrets) != len(v.Secrets) {
		t.Fatalf("the renderer wrote %d Secrets for %d manifest entries, want one each", len(m.Secrets), len(v.Secrets))
	}
	for i, s := range m.Secrets {
		compareGolden(t, dir, "secret-"+v.Secrets[i].Name+".yaml", document(t, s))
	}
}

// TestRenderWritesTheObjectsInOrder holds the stream's order, which an apply
// reads: the namespace before what lands in it, and each Secret after the
// Service.
func TestRenderWritesTheObjectsInOrder(t *testing.T) {
	m := mustBuild(t, goldenValues(t))
	var out bytes.Buffer
	if err := Render(&out, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got []string
	dec := yaml.NewDecoder(&out)
	for {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the stream: %v", err)
		}
		got = append(got, obj.Kind+"/"+obj.Metadata.Name)
	}
	want := []string{
		"Namespace/" + DefaultNamespace,
		"PersistentVolumeClaim/" + TownStateClaim,
		"PersistentVolumeClaim/" + LandingWorkClaim,
		"Service/" + ForgejoName,
	}
	for _, file := range m.Secrets {
		want = append(want, "Secret/"+file.Metadata.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Render wrote %q, want %q", got, want)
	}
}

func TestRenderRefusesBadValues(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Values)
		field  string
		// reason is text the refusal must carry beyond the field's name, for a
		// row whose value a later grammar check would refuse anyway.
		reason string
	}{
		{"a malformed town-state size", func(v *Values) { v.TownStateStorage = "20GB" }, "Values.TownStateStorage", ""},
		{"a zero town-state size", func(v *Values) { v.TownStateStorage = "0Gi" }, "Values.TownStateStorage", ""},
		{"a malformed landing size", func(v *Values) { v.LandingWorkStorage = "twenty" }, "Values.LandingWorkStorage", ""},
		{"a zero landing size", func(v *Values) { v.LandingWorkStorage = "0Ti" }, "Values.LandingWorkStorage", ""},
		{"an empty Forgejo service", func(v *Values) { v.ForgejoService = "" }, "Values.ForgejoService", "is empty"},
		{"an empty Forgejo namespace", func(v *Values) { v.ForgejoNamespace = "" }, "Values.ForgejoNamespace", "is empty"},
		{"a malformed Forgejo service", func(v *Values) { v.ForgejoService = "Forgejo_Svc" }, "Values.ForgejoService", ""},
		{"a malformed Forgejo namespace", func(v *Values) { v.ForgejoNamespace = "ci.prod" }, "Values.ForgejoNamespace", ""},
		{"a Secret entry with no name", func(v *Values) { v.Secrets[0].Name = "" }, "Values.Secrets[0]", "has no name"},
		{"a Secret entry that is not a DNS subdomain", func(v *Values) { v.Secrets[0].Name = "a_b" }, "Values.Secrets[0]", "DNS-1123 subdomain"},
		{"a Secret entry that is a path", func(v *Values) { v.Secrets[0].Name = "forgejo/landing.env" }, "Values.Secrets[0]", ""},
		{"a Secret entry that is absolute", func(v *Values) { v.Secrets[0].Name = "/etc/passwd" }, "Values.Secrets[0]", ""},
		{"a Secret entry that is ..", func(v *Values) { v.Secrets[0].Name = ".." }, "Values.Secrets[0]", ""},
		{"a Secret entry with a control character", func(v *Values) { v.Secrets[0].Name = "a\tb" }, "Values.Secrets[0]", ""},
		{"a duplicate Secret entry", func(v *Values) { v.Secrets[1].Name = v.Secrets[0].Name }, "Values.Secrets[1]", ""},
		{"no Secret entries", func(v *Values) { v.Secrets = nil }, "Values.Secrets", ""},
		{"a malformed namespace", func(v *Values) { v.Namespace = "GT" }, "Values.Namespace", ""},
		{"a malformed storage class", func(v *Values) { v.StorageClass = "local path" }, "Values.StorageClass", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := goldenValues(t)
			tc.change(&v)
			_, err := Build(v)
			if err == nil {
				t.Fatalf("Build = a render, want a refusal naming %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Build refused with %q, want %s named", err, tc.field)
			}
			if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("Build refused with %q, want the reason %q", err, tc.reason)
			}
		})
	}
}

// TestAnEmptyTargetTakesTheDefaultPair holds the default the package supplies: a
// caller that names no Forgejo Service renders the default pair.
func TestAnEmptyTargetTakesTheDefaultPair(t *testing.T) {
	v := goldenValues(t)
	v.ForgejoService, v.ForgejoNamespace = "", ""
	m := mustBuild(t, v)
	want := DefaultForgejoService + "." + DefaultForgejoNamespace + ".svc.cluster.local"
	if got := m.ForgejoService.Spec.ExternalName; got != want {
		t.Errorf("the Service resolves to %q, want the default pair %q", got, want)
	}
}

func TestServiceResolvesForgejo(t *testing.T) {
	m := mustBuild(t, goldenValues(t))
	s := m.ForgejoService
	if s.Metadata.Name != ForgejoName || s.Metadata.Namespace != DefaultNamespace {
		t.Errorf("the Service is %s/%s, want %s/%s", s.Metadata.Namespace, s.Metadata.Name, DefaultNamespace, ForgejoName)
	}
	if s.Spec.Type != "ExternalName" {
		t.Errorf("the Service type = %q, want ExternalName", s.Spec.Type)
	}
	if got, want := s.Spec.ExternalName, "forgejo.forgejo.svc.cluster.local"; got != want {
		t.Errorf("the Service resolves to %q, want %q", got, want)
	}
	if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != ForgejoPort || s.Spec.Ports[0].Protocol != "TCP" {
		t.Errorf("the Service publishes %+v, want one TCP port %d", s.Spec.Ports, ForgejoPort)
	}

	v := goldenValues(t)
	v.ForgejoService, v.ForgejoNamespace = "forge-prod", "ci"
	m = mustBuild(t, v)
	if got, want := m.ForgejoService.Spec.ExternalName, "forge-prod.ci.svc.cluster.local"; got != want {
		t.Errorf("a named target resolves to %q, want %q", got, want)
	}
}

func TestClaimsCarryTheDefaults(t *testing.T) {
	m := mustBuild(t, goldenValues(t))
	for _, tc := range []struct {
		name  string
		claim PersistentVolumeClaim
	}{
		{TownStateClaim, m.TownStateClaim},
		{LandingWorkClaim, m.LandingWorkClaim},
	} {
		claim := tc.claim
		if claim.Metadata.Name != tc.name || claim.Metadata.Namespace != DefaultNamespace {
			t.Errorf("the claim is %s/%s, want %s/%s", claim.Metadata.Namespace, claim.Metadata.Name, DefaultNamespace, tc.name)
		}
		if !slices.Equal(claim.Spec.AccessModes, []string{"ReadWriteOnce"}) {
			t.Errorf("the %s claim's access modes = %q, want ReadWriteOnce", tc.name, claim.Spec.AccessModes)
		}
		if got := claim.Spec.StorageClassName; got != DefaultStorageClass {
			t.Errorf("the %s claim's class = %q, want %q", tc.name, got, DefaultStorageClass)
		}
		if got := claim.Spec.Resources.Requests["storage"]; got != "20Gi" {
			t.Errorf("the %s claim's size = %q, want 20Gi", tc.name, got)
		}
		if got := claim.Metadata.Annotations[SizeEnforcedAnnotation]; got != "false" {
			t.Errorf("the %s claim's %s annotation = %q, want false", tc.name, SizeEnforcedAnnotation, got)
		}
	}
}

// TestSizeAnnotationOnlyOnTheUnenforcedClass holds the other branch: a class
// whose provisioner enforces the size carries no annotation, so the claim does
// not repeat a warning that does not apply.
func TestSizeAnnotationOnlyOnTheUnenforcedClass(t *testing.T) {
	v := goldenValues(t)
	v.StorageClass = "standard-rwo"
	m := mustBuild(t, v)
	for _, claim := range []PersistentVolumeClaim{m.TownStateClaim, m.LandingWorkClaim} {
		if got := claim.Spec.StorageClassName; got != "standard-rwo" {
			t.Errorf("the %s claim's class = %q, want standard-rwo", claim.Metadata.Name, got)
		}
		if value, ok := claim.Metadata.Annotations[SizeEnforcedAnnotation]; ok {
			t.Errorf("the %s claim on an enforcing class carries %s: %q", claim.Metadata.Name, SizeEnforcedAnnotation, value)
		}
	}
}

func TestNamespaceIsClusterScoped(t *testing.T) {
	n := mustBuild(t, goldenValues(t)).Namespace
	if n.Metadata.Name != DefaultNamespace {
		t.Errorf("the Namespace is %q, want %q", n.Metadata.Name, DefaultNamespace)
	}
	if n.Metadata.Namespace != "" {
		t.Errorf("the Namespace carries the namespace field %q; a namespace is cluster-scoped", n.Metadata.Namespace)
	}
	if n.Kind != "Namespace" {
		t.Errorf("the object's kind = %q, want Namespace", n.Kind)
	}
}

// TestSecretsHoldNoBytes holds the renderer's promise: one Secret per manifest
// entry, its name and source named, and an empty placeholder where the operator
// puts the value.
func TestSecretsHoldNoBytes(t *testing.T) {
	v := goldenValues(t)
	m := mustBuild(t, v)
	if len(m.Secrets) != len(v.Secrets) {
		t.Fatalf("the renderer wrote %d Secrets for %d manifest entries", len(m.Secrets), len(v.Secrets))
	}
	for i, file := range v.Secrets {
		s := m.Secrets[i]
		if got, want := s.Metadata.Name, SecretNamePrefix+file.Name; got != want {
			t.Errorf("entry %q rendered as Secret %q, want %q", file.Name, got, want)
		}
		if s.Metadata.Namespace != DefaultNamespace {
			t.Errorf("Secret %s lands in %q, want %q", s.Metadata.Name, s.Metadata.Namespace, DefaultNamespace)
		}
		if got := s.Metadata.Annotations[SourceFileAnnotation]; got != file.Name {
			t.Errorf("Secret %s names its source %q, want %q", s.Metadata.Name, got, file.Name)
		}
		if len(s.StringData) != 1 {
			t.Errorf("Secret %s holds %d keys, want only its placeholder", s.Metadata.Name, len(s.StringData))
		}
		if got, ok := s.StringData[file.Name]; !ok || got != "" {
			t.Errorf("Secret %s's placeholder for %q = %q (present %t), want an empty string", s.Metadata.Name, file.Name, got, ok)
		}
	}
}

// mountFor returns the mount of one volume.
func mountFor(volume string) (Mount, bool) {
	for _, m := range Mounts {
		if m.Volume == volume {
			return m, true
		}
	}
	return Mount{}, false
}

func TestMountsMatchTheTownStateManifest(t *testing.T) {
	t.Parallel()
	manifest := townstate.Default()
	var daemonPaths, sessionPaths []string
	for _, e := range manifest.Entries {
		switch e.Owner {
		case townstate.OwnerDaemon:
			daemonPaths = append(daemonPaths, e.Path)
		case townstate.OwnerSession:
			sessionPaths = append(sessionPaths, e.Path)
		}
	}
	if len(daemonPaths) == 0 || len(sessionPaths) == 0 {
		t.Fatalf("the manifest split into %d daemon-owned and %d session-owned entries; a check over either empty set would pass for the wrong reason", len(daemonPaths), len(sessionPaths))
	}

	townState, ok := mountFor(TownStateClaim)
	if !ok {
		t.Fatalf("Mounts holds no %s volume, so no daemon-owned path is mounted", TownStateClaim)
	}
	if townState.Path != TownRootPath {
		t.Errorf("the %s mount is at %q, want the town root %q", TownStateClaim, townState.Path, TownRootPath)
	}

	got := slices.Clone(townState.Entries)
	slices.Sort(got)
	want := slices.Clone(daemonPaths)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the %s mount carries %q, want the manifest's daemon-owned set %q; add or remove the row in Mounts", TownStateClaim, got, want)
	}

	// The claim's contents come from `gt town state export`, which carries the
	// rows above and nothing else, so a session-owned tree never lands in the
	// volume even though the mount path is the town root's own prefix.
	for _, m := range Mounts {
		for _, p := range m.Entries {
			if slices.Contains(sessionPaths, p) {
				t.Errorf("the %s volume carries the session-owned path %q", m.Volume, p)
			}
		}
	}

	// Every carried path lands under its volume's path. A top-level daemon-owned
	// entry is a direct child of the town-state mount.
	for _, m := range Mounts {
		for _, p := range m.Entries {
			if path.IsAbs(p) {
				t.Errorf("the %s volume carries %q, an absolute path; entries are relative to the town root", m.Volume, p)
				continue
			}
			if joined := path.Join(m.Path, p); !strings.HasPrefix(joined, m.Path+"/") {
				t.Errorf("the %s entry %q resolves to %q, outside the mount %q", m.Volume, p, joined, m.Path)
			}
		}
	}
	for _, p := range daemonPaths {
		if strings.Contains(p, "/") {
			continue
		}
		if got, want := path.Join(townState.Path, p), townState.Path+"/"+p; got != want {
			t.Errorf("the top-level entry %q lands at %q, want a direct child %q", p, got, want)
		}
	}

	// The landing and tmp mounts are at the paths the daemon's settings name:
	// patrols.landing_worker.work_root and the agent env blocks' GOTMPDIR and
	// TMPDIR.
	landing, ok := mountFor(LandingWorkClaim)
	if !ok {
		t.Fatalf("Mounts holds no %s volume", LandingWorkClaim)
	}
	if got, want := landing.Path, "/Users/sloan/gt-landing.noindex"; got != want || LandingWorkPath != want {
		t.Errorf("the %s mount is at %q and LandingWorkPath is %q, want the setting's %q", LandingWorkClaim, got, LandingWorkPath, want)
	}
	tmp, ok := mountFor(TempVolume)
	if !ok {
		t.Fatalf("Mounts holds no %s volume", TempVolume)
	}
	if got, want := tmp.Path, "/Users/Shared/gt-gotmp.noindex"; got != want || TempPath != want {
		t.Errorf("the %s mount is at %q and TempPath is %q, want the settings' %q", TempVolume, got, TempPath, want)
	}
	for _, p := range []string{TownRootPath, LandingWorkPath, TempPath} {
		if !path.IsAbs(p) {
			t.Errorf("the mount path %q is not absolute; the pod mounts it at the path the container uses", p)
		}
	}
}

// failingWriter fails every write, so Render's error path is driven through a
// writer that cannot accept the stream.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the disk is full") }

func TestRenderReportsAWriteError(t *testing.T) {
	m := mustBuild(t, goldenValues(t))
	err := Render(failingWriter{}, m)
	if err == nil {
		t.Fatal("Render = no error, want the writer's error")
	}
	if !strings.Contains(err.Error(), "the disk is full") {
		t.Errorf("Render reported %q, want the writer's error", err)
	}
}

// TestGoldenMismatchIsReported drives compareGolden, the comparison the golden
// tests use: a document that matches its golden must pass, and one that differs
// must be reported, so a hand-edited golden or a comparison that stopped
// comparing fails loudly.
func TestGoldenMismatchIsReported(t *testing.T) {
	dir := t.TempDir()
	const file = "claim.yaml"
	if err := os.WriteFile(filepath.Join(dir, file), []byte("apiVersion: v1\n"), 0o644); err != nil {
		t.Fatalf("writing the golden: %v", err)
	}
	if updatingGoldens() {
		t.Skip("-update rewrites the file instead of comparing, so this mode has no mismatch to report")
	}

	matched := &goldenRecorder{}
	compareGolden(matched, dir, file, []byte("apiVersion: v1\n"))
	if len(matched.messages) != 0 {
		t.Fatalf("comparing a document with its own golden reported %q, want no failure", matched.messages)
	}

	mismatched := &goldenRecorder{}
	compareGolden(mismatched, dir, file, []byte("kind: PersistentVolumeClaim\n"))
	if len(mismatched.messages) != 1 || !strings.Contains(mismatched.messages[0], file) {
		t.Errorf("comparing a different document reported %q, want one failure naming %s", mismatched.messages, file)
	}
}

// goldenRecorder collects the failures compareGolden reports, so a test can
// observe them instead of failing.
type goldenRecorder struct {
	messages []string
}

func (r *goldenRecorder) Helper() {}

func (r *goldenRecorder) Error(args ...any) {
	r.messages = append(r.messages, fmt.Sprint(args...))
}

func (r *goldenRecorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *goldenRecorder) Fatalf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}
