package daemon

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townstate"
)

// updatingGoldens reports whether -update was passed. The flag is registered in
// TestMain, so no test can change it while another reads it.
func updatingGoldens() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// testImage is the digest reference the goldens record, shaped as
// deploy/values/images.yaml writes one.
const testImage = "registry.example.com/gt/gastown-daemon@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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
		Image:            testImage,
		// Env is a daemon.env entry the compose writer forwards and no rule
		// filters: a plain provider setting beside the credentials.
		Env: map[string]string{"AWS_REGION": "us-east-1"},
		// CPU and Memory are the share the deploy passes. The renderer has no
		// default for either, because no source records a figure (see Values.CPU),
		// so the goldens record an example share rather than a fallback.
		CPU:                     "2",
		Memory:                  "4Gi",
		OMToken:                 true,
		ForgeHost:               "forgejo:3000",
		ConfirmContainerStopped: true,
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

// compareGolden compares got with the golden file dir/file. Under -update it then
// rewrites the file, and the comparison has already failed the run if the golden
// changed, so a rewrite is never a silent pass. The directory is a parameter so a
// test can drive this function against a file it owns instead of testdata.
func compareGolden(t goldenReporter, dir, file string, got []byte) {
	t.Helper()
	filePath := filepath.Join(dir, file)
	want, err := os.ReadFile(filePath)
	switch {
	case err == nil:
	case updatingGoldens() && errors.Is(err, fs.ErrNotExist):
		// A new golden has no old bytes, so the comparison reports it as a change.
	default:
		t.Fatalf("reading %s: %v", filePath, err)
	}
	if msg := goldenMismatch(filePath, got, want); msg != "" {
		if updatingGoldens() {
			msg += "-update rewrites the golden after this comparison; run again without -update to confirm it\n"
		}
		t.Error(msg)
	}
	if updatingGoldens() {
		if err := os.WriteFile(filePath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", filePath, err)
		}
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
	t.Parallel()
	v := goldenValues(t)
	m := mustBuild(t, v)
	dir := filepath.Join(packageDir(t), "testdata")

	for _, doc := range []struct {
		file  string
		value any
	}{
		{"namespace.yaml", m.Namespace},
		{"service-account.yaml", m.ServiceAccount},
		{"town-state-claim.yaml", m.TownStateClaim},
		{"landing-work-claim.yaml", m.LandingWorkClaim},
		{"forgejo-service.yaml", m.ForgejoService},
		{"deployment.yaml", m.Deployment},
		{"rolebinding-sweep-runner.yaml", m.SweepRoleBinding},
		{"rolebinding-ci-cache-trim.yaml", m.CacheTrimRoleBinding},
		{"clusterrolebinding-sweep-reader.yaml", m.SweepReaderBinding},
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
	t.Parallel()
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
		"ServiceAccount/" + ServiceAccountName,
		"PersistentVolumeClaim/" + TownStateClaim,
		"PersistentVolumeClaim/" + LandingWorkClaim,
		"Service/" + ForgejoName,
	}
	for _, file := range m.Secrets {
		want = append(want, "Secret/"+file.Metadata.Name)
	}
	want = append(want,
		"Deployment/"+Name,
		"RoleBinding/"+m.SweepRoleBinding.Metadata.Name,
		"RoleBinding/"+m.CacheTrimRoleBinding.Metadata.Name,
		"ClusterRoleBinding/"+m.SweepReaderBinding.Metadata.Name,
	)
	if !slices.Equal(got, want) {
		t.Errorf("Render wrote %q, want %q", got, want)
	}
}

func TestRenderRefusesBadValues(t *testing.T) {
	t.Parallel()
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
		{"an image by tag", func(v *Values) { v.Image = "registry.example.com/gt/gastown-daemon:v1" }, "Values.Image", ""},
		{"a missing single-owner acknowledgement", func(v *Values) { v.ConfirmContainerStopped = false }, "Values.ConfirmContainerStopped", "the Docker Desktop container owns the town until it is stopped"},
		{"an om token with no Secret", func(v *Values) { v.Secrets = withoutSecret(v.Secrets, OMTokenFile) }, "Values.OMToken", ""},
		{"a forge host with no Secret", func(v *Values) { v.Secrets = withoutSecret(v.Secrets, ForgeTokenFile) }, "Values.ForgeHost", ""},
		{"a forge host that is a URL", func(v *Values) { v.ForgeHost = "https://forgejo:3000" }, "Values.ForgeHost", ""},
		{"a forge port outside the range", func(v *Values) { v.ForgeHost = "forgejo:70000" }, "Values.ForgeHost", ""},
		{"no CPU share", func(v *Values) { v.CPU = "" }, "Values.CPU", "no source gives the daemon's measured CPU share"},
		{"a malformed CPU share", func(v *Values) { v.CPU = "two" }, "Values.CPU", ""},
		{"a zero CPU share", func(v *Values) { v.CPU = "0m" }, "Values.CPU", ""},
		{"no memory share", func(v *Values) { v.Memory = "" }, "Values.Memory", "no source gives the daemon's measured memory share"},
		{"a memory share with no suffix", func(v *Values) { v.Memory = "4096" }, "Values.Memory", ""},
		{"a grace period under the drain bound", func(v *Values) { v.TerminationGraceSeconds = MinimumTerminationGraceSeconds - 1 }, "Values.TerminationGraceSeconds", ""},
		{"an env key that is not a variable name", func(v *Values) { v.Env["A-B"] = "x" }, `Values.Env["A-B"]`, ""},
		{"an env key the render sets itself", func(v *Values) { v.Env[TownRootEnv] = "/elsewhere" }, `Values.Env["GT_TOWN_ROOT"]`, ""},
		{"an env value that looks like a secret", func(v *Values) { v.Env["SOME_TOKEN"] = "ghp_0123456789abcdefghijklmnopqrstuvwxyz" }, `Values.Env["SOME_TOKEN"]`, ""},
		{"two Secret files whose volume names collide", func(v *Values) {
			v.Secrets = append(v.Secrets, templates.SecretFile{Name: "x.y"}, templates.SecretFile{Name: "x-y"})
		}, "Values.Secrets", ""},
		{"a Secret file whose volume name is too long", func(v *Values) {
			// Each label of the Secret name fits 63 characters, but the volume
			// name joins them, so only the volume-name check refuses it.
			v.Secrets = append(v.Secrets, templates.SecretFile{
				Name: strings.Repeat("a", 30) + "." + strings.Repeat("b", 30),
			})
		}, "Values.Secrets", "longer than 63 characters"},
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
	t.Parallel()
	v := goldenValues(t)
	v.ForgejoService, v.ForgejoNamespace = "", ""
	m := mustBuild(t, v)
	want := DefaultForgejoService + "." + DefaultForgejoNamespace + ".svc.cluster.local"
	if got := m.ForgejoService.Spec.ExternalName; got != want {
		t.Errorf("the Service resolves to %q, want the default pair %q", got, want)
	}
}

func TestServiceResolvesForgejo(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// checkMount returns what drifts between mount and the split m: a daemon-owned
// row the town-state mount lacks or holds out of the split's order, a row the
// split gives a session, an entry that is absolute or resolves outside the mount,
// and any row a mount other than the town-state one carries. It returns the
// findings instead of failing, so a fixture that must drift drives this check.
func checkMount(mount Mount, m *townstate.Manifest) []string {
	var want []string
	if mount.Volume == TownStateClaim {
		want = ownedBy(m, townstate.OwnerDaemon)
	}
	var findings []string
	if !slices.Equal(mount.Entries, want) {
		findings = append(findings, fmt.Sprintf("the %s mount carries %q, want %q: the split's daemon-owned rows, in the split's order", mount.Volume, mount.Entries, want))
	}
	sessionPaths := ownedBy(m, townstate.OwnerSession)
	for _, p := range mount.Entries {
		if slices.Contains(sessionPaths, p) {
			findings = append(findings, fmt.Sprintf("the %s volume carries the session-owned path %q", mount.Volume, p))
		}
		if path.IsAbs(p) {
			findings = append(findings, fmt.Sprintf("the %s volume carries %q, an absolute path; entries are relative to the town root", mount.Volume, p))
			continue
		}
		if joined := path.Join(mount.Path, p); !strings.HasPrefix(joined, mount.Path+"/") {
			findings = append(findings, fmt.Sprintf("the %s entry %q resolves to %q, outside the mount %q", mount.Volume, p, joined, mount.Path))
		}
	}
	return findings
}

// TestMountsMatchTheTownStateManifest holds the pod's real mount table to the
// town-state split. The town-state claim's contents come from `gt town state
// export`, which carries the daemon-owned rows and nothing else, so a
// session-owned tree never lands in the volume even though the mount path is the
// town root's own prefix.
func TestMountsMatchTheTownStateManifest(t *testing.T) {
	t.Parallel()
	manifest := townstate.Default()
	daemonPaths, sessionPaths := ownedBy(manifest, townstate.OwnerDaemon), ownedBy(manifest, townstate.OwnerSession)
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
	for _, m := range Mounts {
		for _, finding := range checkMount(m, manifest) {
			t.Error(finding)
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

// TestMountCheckReportsDrift drives every branch of checkMount against a split
// this test owns: a mount that matches passes, and each drift is reported under
// a finding naming it.
func TestMountCheckReportsDrift(t *testing.T) {
	t.Parallel()
	split := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: "daemon", Owner: townstate.OwnerDaemon, Why: "fixture"},
		{Path: "logs", Owner: townstate.OwnerDaemon, Why: "fixture"},
		{Path: "<rig>/polecats", Owner: townstate.OwnerSession, Why: "fixture"},
	}}
	escaping := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: "../escape", Owner: townstate.OwnerDaemon, Why: "fixture"},
	}}
	absolute := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: "/etc/passwd", Owner: townstate.OwnerDaemon, Why: "fixture"},
	}}
	town := func(entries ...string) Mount {
		return Mount{Volume: TownStateClaim, Path: TownRootPath, Entries: entries}
	}
	tests := []struct {
		name  string
		split *townstate.Manifest
		mount Mount
		// want names a finding the check must report; "" means it reports none.
		want string
	}{
		{"a mount that matches the split", split, town("daemon", "logs"), ""},
		{"a session-owned row carried", split, town("daemon", "logs", "<rig>/polecats"), `the session-owned path "<rig>/polecats"`},
		{"a daemon-owned row missing", split, town("daemon"), `carries ["daemon"]`},
		{"a daemon-owned row out of order", split, town("logs", "daemon"), `carries ["logs" "daemon"]`},
		{"an entry that resolves outside the mount", escaping, town("../escape"), `resolves to "/Users/sloan/escape", outside the mount`},
		{"an absolute entry", absolute, town("/etc/passwd"), "an absolute path"},
		{"a row on a volume that holds none", split, Mount{Volume: LandingWorkClaim, Path: LandingWorkPath, Entries: []string{"daemon"}}, `carries ["daemon"], want []`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := checkMount(tc.mount, tc.split)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Errorf("checkMount reported %q for a mount that matches the split", findings)
				}
				return
			}
			if !slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, tc.want) }) {
				t.Errorf("checkMount reported %q, want a finding naming %q", findings, tc.want)
			}
		})
	}
}

// failingWriter fails every write, so Render's error path is driven through a
// writer that cannot accept the stream.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the disk is full") }

func TestRenderReportsAWriteError(t *testing.T) {
	t.Parallel()
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
// tests use, in whichever mode the run is in: a document that matches its golden
// must pass, and one that differs must be reported, so a hand-edited golden or a
// comparison that stopped comparing fails loudly, under -update too.
func TestGoldenMismatchIsReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const file = "claim.yaml"
	if err := os.WriteFile(filepath.Join(dir, file), []byte("apiVersion: v1\n"), 0o644); err != nil {
		t.Fatalf("writing the golden: %v", err)
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

// withoutSecret returns files without the entry named name.
func withoutSecret(files []templates.SecretFile, name string) []templates.SecretFile {
	out := make([]templates.SecretFile, 0, len(files))
	for _, file := range files {
		if file.Name != name {
			out = append(out, file)
		}
	}
	return out
}

// TestRefusesATagAndAnUnconfirmedOwner drives the two refusals the Deployment
// depends on: an image tag (the pod must run the commit the values file
// records) and a render without the single-owner acknowledgement.
func TestRefusesATagAndAnUnconfirmedOwner(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.Image = "registry.example.com/gt/gastown-daemon:v1"
	_, err := Build(v)
	if err == nil || !strings.Contains(err.Error(), "Values.Image") {
		t.Fatalf("Build with an image tag = %v, want a refusal naming Values.Image", err)
	}

	v = goldenValues(t)
	v.ConfirmContainerStopped = false
	_, err = Build(v)
	if err == nil {
		t.Fatal("Build without ConfirmContainerStopped = a render, want the single-owner refusal")
	}
	if !strings.Contains(err.Error(), "the Docker Desktop container owns the town until it is stopped") {
		t.Errorf("Build without ConfirmContainerStopped refused with %q, want the single-owner reason", err)
	}
}

// TestDeploymentGolden holds the Deployment to its golden and to the shape the
// pod relies on: one replica under Recreate, the digest image, requests equal
// to limits, the two probes and the single-owner annotation.
func TestDeploymentGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment.yaml", document(t, m.Deployment))

	d := m.Deployment
	if d.Spec.Replicas != 1 {
		t.Errorf("the Deployment runs %d replicas, want 1: two daemons would share the town's volume", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "Recreate" {
		t.Errorf("the Deployment strategy = %q, want Recreate, so a rollout never overlaps two pods", d.Spec.Strategy.Type)
	}
	if got := d.Metadata.Annotations[TownOwnerAnnotation]; got != TownOwnerPod {
		t.Errorf("the Deployment %s annotation = %q, want %q", TownOwnerAnnotation, got, TownOwnerPod)
	}
	if got := d.Spec.Template.Spec.ServiceAccountName; got != ServiceAccountName {
		t.Errorf("the pod runs as service account %q, want %q", got, ServiceAccountName)
	}
	if got := d.Spec.Template.Spec.TerminationGracePeriodSeconds; got < int64(MinimumTerminationGraceSeconds) {
		t.Errorf("the pod's grace period is %ds, want at least %ds", got, MinimumTerminationGraceSeconds)
	}

	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("the container image = %q, want the digest %q", c.Image, testImage)
	}
	if !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("the container image %q is not by digest", c.Image)
	}
	if !maps.Equal(c.Resources.Requests, c.Resources.Limits) {
		t.Errorf("the container requests %v and limits %v; requests equal limits", c.Resources.Requests, c.Resources.Limits)
	}
	if c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Fatalf("the container carries readiness %v and liveness %v, want both", c.ReadinessProbe, c.LivenessProbe)
	}
	if got, want := c.ReadinessProbe.Exec.Command, []string{"gt", "status", "--line"}; !slices.Equal(got, want) {
		t.Errorf("the readiness probe runs %q, want the health line %q", got, want)
	}
	if got, want := c.LivenessProbe.Exec.Command, []string{"gt", "daemon", "status"}; !slices.Equal(got, want) {
		t.Errorf("the liveness probe runs %q, want the running check %q", got, want)
	}
	if c.LivenessProbe.PeriodSeconds <= c.ReadinessProbe.PeriodSeconds || c.LivenessProbe.FailureThreshold <= c.ReadinessProbe.FailureThreshold {
		t.Errorf("the liveness probe (%ds period, %d failures) is not looser than readiness (%ds, %d)",
			c.LivenessProbe.PeriodSeconds, c.LivenessProbe.FailureThreshold,
			c.ReadinessProbe.PeriodSeconds, c.ReadinessProbe.FailureThreshold)
	}
}

// TestPodHasNoPreStopAndGraceCoversTheDrain holds the shutdown contract: the pod
// carries no preStop hook, so SIGTERM plus the grace period is the drain, and
// the grace period is at least the drain bound the package computes from the
// daemon's two named budgets. It also holds the arithmetic the comments state:
// the drain bound is the shutdown budget plus the landing-pass drain bound, and
// the default grace is that sum plus the margin.
func TestPodHasNoPreStopAndGraceCoversTheDrain(t *testing.T) {
	t.Parallel()

	if got, want := DrainBound, daemonShutdownBudget+landingPassDrainBound; got != want {
		t.Fatalf("DrainBound = %s, want the daemon's shutdown budget plus the landing-pass drain bound = %s", got, want)
	}
	if got, want := DefaultTerminationGraceSeconds, int((DrainBound+terminationGraceMargin)/time.Second); got != want {
		t.Errorf("DefaultTerminationGraceSeconds = %d, want the drain bound plus the margin = %d", got, want)
	}
	if MinimumTerminationGraceSeconds < int(DrainBound/time.Second) {
		t.Errorf("MinimumTerminationGraceSeconds = %d, under the drain bound %s", MinimumTerminationGraceSeconds, DrainBound)
	}
	// The sums the constants' comments state, so editing a constant without the
	// comment it describes fails here: 30s + 2m = 2m30s is the floor, and
	// 2m30s + 30s is the default.
	if got, want := int(DrainBound/time.Second), 150; got != want {
		t.Errorf("the drain bound is %d seconds, want the 30s + 2m = 150 the comments state", got)
	}
	if got, want := DefaultTerminationGraceSeconds, 180; got != want {
		t.Errorf("the default grace is %d seconds, want the 150 + 30 = 180 the comments state", got)
	}

	m := mustBuild(t, goldenValues(t))
	pod := m.Deployment.Spec.Template.Spec
	if got, want := pod.TerminationGracePeriodSeconds, int64(DefaultTerminationGraceSeconds); got != want {
		t.Errorf("the pod's grace period = %ds, want the default %ds", got, want)
	}
	if got := pod.TerminationGracePeriodSeconds; got < int64(MinimumTerminationGraceSeconds) {
		t.Errorf("the pod's grace period = %ds, under the drain bound %ds", got, MinimumTerminationGraceSeconds)
	}

	// The emitted Deployment is what an apply reads, so the hook check reads it:
	// no container may carry a lifecycle block at all.
	var rendered struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name      string         `yaml:"name"`
						Lifecycle map[string]any `yaml:"lifecycle"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(document(t, m.Deployment), &rendered); err != nil {
		t.Fatalf("parsing the rendered Deployment: %v", err)
	}
	if len(rendered.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("the rendered Deployment holds no container, so the hook check reads nothing")
	}
	for _, c := range rendered.Spec.Template.Spec.Containers {
		if c.Lifecycle != nil {
			t.Errorf("container %q carries a lifecycle block %v; SIGTERM plus the grace period is the drain", c.Name, c.Lifecycle)
		}
	}

	// A grace period the caller names at the floor is carried unchanged, so the
	// refusal below the floor is the only bound on the value.
	v := goldenValues(t)
	v.TerminationGraceSeconds = MinimumTerminationGraceSeconds
	if got, want := mustBuild(t, v).Deployment.Spec.Template.Spec.TerminationGracePeriodSeconds, int64(MinimumTerminationGraceSeconds); got != want {
		t.Errorf("the pod's grace period = %ds for a caller's %ds, want the caller's value", got, want)
	}
}

// TestPodCommandEqualsTheComposeCommand renders the live compose file with the
// same credentials and holds the pod's arguments equal to its daemon command;
// an environment value that looks like a secret is refused, and the refusal
// names the key without echoing the value.
func TestPodCommandEqualsTheComposeCommand(t *testing.T) {
	t.Parallel()

	spec := templates.ComposeSpec{
		Project:        "gt-town",
		ContainerName:  "gt-daemon",
		Image:          "gt-daemon:abc1234",
		TownRoot:       TownRootPath,
		StopGrace:      templates.MinimumStopGrace,
		DoltHost:       "host.docker.internal",
		OMTokenFile:    "/host/om-daemon-token",
		OMConfigFile:   "/host/om-config.json",
		ForgeHost:      "forgejo:3000",
		ForgeTokenFile: "/host/forgejo-landing.env",
	}
	rendered, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		t.Fatalf("RenderDaemonCompose: %v", err)
	}
	var composeFile struct {
		Services map[string]struct {
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(rendered, &composeFile); err != nil {
		t.Fatalf("the rendered compose file does not parse: %v", err)
	}
	want := composeFile.Services["daemon"].Command
	if len(want) == 0 {
		t.Fatal("the compose daemon service carries no command, so the equality check reads nothing")
	}

	m := mustBuild(t, goldenValues(t))
	got := m.Deployment.Spec.Template.Spec.Containers[0].Args
	if !slices.Equal(got, want) {
		t.Errorf("the pod's args = %q, want the compose daemon command %q", got, want)
	}

	// A spec with no credentials renders no command, and the pod passes none.
	spec.OMTokenFile, spec.OMConfigFile, spec.ForgeHost, spec.ForgeTokenFile = "", "", "", ""
	rendered, err = templates.RenderDaemonCompose(spec)
	if err != nil {
		t.Fatalf("RenderDaemonCompose without credentials: %v", err)
	}
	if err := yaml.Unmarshal(rendered, &composeFile); err != nil {
		t.Fatalf("the rendered compose file does not parse: %v", err)
	}
	if want := composeFile.Services["daemon"].Command; len(want) != 0 {
		t.Fatalf("the compose daemon service carries %q without credentials, want no command", want)
	}
	v := goldenValues(t)
	v.OMToken, v.ForgeHost = false, ""
	if got := mustBuild(t, v).Deployment.Spec.Template.Spec.Containers[0].Args; len(got) != 0 {
		t.Errorf("the pod's args are %q without credentials, want none", got)
	}

	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	v = goldenValues(t)
	v.Env["FORGEJO_TOKEN"] = secret
	_, err = Build(v)
	if err == nil {
		t.Fatal("Build with a secret-looking env value = a render, want a refusal")
	}
	if !strings.Contains(err.Error(), "FORGEJO_TOKEN") {
		t.Errorf("Build refused with %q, want the key named", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the refusal echoes the secret value, and a manifest never carries one")
	}
}

// composeOnlyEnv are the names the compose render sets on the daemon service for
// the docker engine and the container's identity: the engine it reaches, the
// container's name and supervisor, and the takeover switch. The pod has no
// engine sidecar and takes its identity from its own pod, so it sets none of
// them; every other name of the service's environment must match the pod's.
var composeOnlyEnv = []string{"DOCKER_HOST", "GT_DAEMON_CONTAINER_NAME", "GT_DAEMON_SUPERVISOR", "GT_DAEMON_TAKEOVER"}

// TestPodEnvEqualsTheComposeEnv renders the live compose file with the pod's
// Values and holds the two environments equal name for name and value for
// value, after the compose-only names above: the pod's environment and the
// container's come from the same Values.Env map, so an entry that reaches one
// reaches the other, and a drift fails here naming both sets.
func TestPodEnvEqualsTheComposeEnv(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	spec := templates.ComposeSpec{
		Project:       "gt-town",
		ContainerName: "gt-daemon",
		Image:         "gt-daemon:abc1234",
		TownRoot:      TownRootPath,
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      "host.docker.internal",
		Env:           maps.Clone(v.Env),
		// The config directory is what makes the compose render set
		// XDG_CONFIG_HOME, the one render-owned name both services carry. No
		// Secrets are listed, so the render mounts the directory and reads no host
		// file.
		ConfigDir: "/Users/sloan/.config/gt",
	}
	rendered, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		t.Fatalf("RenderDaemonCompose: %v", err)
	}
	var composeFile struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(rendered, &composeFile); err != nil {
		t.Fatalf("the rendered compose file does not parse: %v", err)
	}
	composeEnv := maps.Clone(composeFile.Services["daemon"].Environment)
	if len(composeEnv) == 0 {
		t.Fatal("the compose daemon service carries no environment, so the equality check reads nothing")
	}
	for _, name := range composeOnlyEnv {
		delete(composeEnv, name)
	}

	pod := mustBuild(t, v).Deployment.Spec.Template.Spec.Containers[0]
	podEnv := make(map[string]string, len(pod.Env))
	for _, e := range pod.Env {
		podEnv[e.Name] = e.Value
	}

	if !maps.Equal(podEnv, composeEnv) {
		t.Errorf("the pod's environment and the compose daemon service's differ: pod names %q, compose names %q (after removing the compose-only names %q)",
			slices.Sorted(maps.Keys(podEnv)), slices.Sorted(maps.Keys(composeEnv)), composeOnlyEnv)
		for name, got := range podEnv {
			if want, ok := composeEnv[name]; ok && want != got {
				t.Errorf("env %s = %q in the pod, %q in the compose service", name, got, want)
			}
		}
	}
}

// TestLooksLikeSecretMatchesConfig holds the renderer's copy of the rule to
// config.LooksLikeSecret, which the import rule keeps it from calling.
func TestLooksLikeSecretMatchesConfig(t *testing.T) {
	t.Parallel()
	tests := []struct{ key, value string }{
		{"ANTHROPIC_AUTH_TOKEN", "sk-ant-0123456789abcdef"},
		{"FORGEJO_TOKEN", "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
		{"AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE"},
		{"AWS_SESSION_TOKEN", "ASIAIOSFODNN7EXAMPLE"},
		{"SOME_PATH", "/Users/sloan/.config/gt/forgejo-landing.env"},
		{"SOME_URL", "https://forgejo.example:3000/owner/repo"},
		{"SOME_REFERENCE", "${DEEPSEEK_API_KEY}"},
		{"SOME_NUMBER", "12345678901234567890"},
		{"SOME_SHORT_SECRET", "short"},
		{"TZ", "America/Chicago"},
		{"GIT_EMAIL", "gt@gastown.local"},
		{"SOME_TOKEN", "project-gastown-2026-11-08"},
		{"SOME_VALUE", "0123456789abcdef"},
		{"NORMAL", "a value with spaces in it"},
	}
	for _, tc := range tests {
		if got, want := looksLikeSecret(tc.key, tc.value), config.LooksLikeSecret(tc.key, tc.value); got != want {
			t.Errorf("looksLikeSecret(%q, %q) = %t, want config.LooksLikeSecret's %t", tc.key, tc.value, got, want)
		}
	}
}

// TestForgeHostMatchesConfigPattern holds the renderer's host check to
// config.ForgeHostPattern, which the entrypoint and the compose render read.
func TestForgeHostMatchesConfigPattern(t *testing.T) {
	t.Parallel()
	hosts := []string{
		"forgejo:3000", "forgejo.example:3000", "Forgejo:80", "f:1",
		"forgejo", "forgejo:0", "forgejo:70000", "https://forgejo:3000",
		"-forgejo:3000", "forgejo:3000/path", "forgejo:27a", "forgejo:",
	}
	for _, host := range hosts {
		m := config.ForgeHostPattern.FindStringSubmatch(host)
		configOK := m != nil
		if configOK {
			port, err := strconv.Atoi(m[1])
			configOK = err == nil && port >= 1 && port <= 65535
		}
		if got := validForgeHost(host) == nil; got != configOK {
			t.Errorf("validForgeHost(%q) = %t, want config.ForgeHostPattern's %t", host, got, configOK)
		}
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
