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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/manifestdata"
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

// testProxyImage is the digest reference the proxy relays record, shaped the
// same way: another repository, so a test cannot pass the daemon's image off as
// the relay's.
const testProxyImage = "registry.example.com/gt/socat@sha256:" +
	"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

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
		// ProxyImage is a shape fixture: the goldens record the two relays the
		// digest gives the pod, and the digest itself is the overseer's.
		ProxyImage: testProxyImage,
		// Env is a daemon.env entry the compose writer forwards and no rule
		// filters: a plain provider setting the decision table keeps unchanged.
		Env: map[string]string{awsRegionEnv: "us-east-1"},
		// CPU and Memory are a shape fixture, not the daemon's share: the goldens
		// check that requests equal limits and that the units render, and the
		// amount is the caller's. No source records the daemon's measured use (see
		// Values.CPU), so these figures cite nothing and are not a default.
		CPU:                     "2",
		Memory:                  "4Gi",
		OMToken:                 true,
		ForgeHost:               "forgejo:3000",
		ConfirmContainerStopped: true,
		// A shape fixture: any gateway IPv4 renders the one hostAlias, and the
		// address is the caller's (see Values.HostGatewayIP).
		HostGatewayIP: "192.168.5.2",
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
		{"dolt-backup-claim.yaml", m.DoltBackupClaim},
		{"forgejo-service.yaml", m.ForgejoService},
		{"deployment.yaml", m.Deployment},
		{"rolebinding-sweep-runner.yaml", m.SweepRoleBinding},
		{"rolebinding-ci-cache-trim.yaml", m.CacheTrimRoleBinding},
		{"clusterrolebinding-sweep-reader.yaml", m.SweepReaderBinding},
	} {
		compareGolden(t, dir, doc.file, document(t, doc.value))
	}
	// One Secret per entry, then the daemon-openshell Secret.
	if len(m.Secrets) != len(v.Secrets)+1 {
		t.Fatalf("the renderer wrote %d Secrets for %d manifest entries, want one each and the openshell Secret", len(m.Secrets), len(v.Secrets))
	}
	for i, file := range v.Secrets {
		compareGolden(t, dir, "secret-"+file.Name+".yaml", document(t, m.Secrets[i]))
	}
	compareGolden(t, dir, "secret-openshell.yaml", document(t, m.Secrets[len(v.Secrets)]))
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
		"PersistentVolumeClaim/" + DoltBackupClaim,
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
		{"a malformed dolt-backup size", func(v *Values) { v.DoltBackupStorage = "20GB" }, "Values.DoltBackupStorage", ""},
		{"a zero dolt-backup size", func(v *Values) { v.DoltBackupStorage = "0Gi" }, "Values.DoltBackupStorage", ""},
		{"a loopback host gateway address", func(v *Values) { v.HostGatewayIP = "127.0.0.1" }, "Values.HostGatewayIP", "loopback"},
		{"an unspecified host gateway address", func(v *Values) { v.HostGatewayIP = "0.0.0.0" }, "Values.HostGatewayIP", "unspecified"},
		{"an IPv6 host gateway address", func(v *Values) { v.HostGatewayIP = "fd00::1" }, "Values.HostGatewayIP", ""},
		{"a host gateway that is not an address", func(v *Values) { v.HostGatewayIP = "host.gateway.local" }, "Values.HostGatewayIP", ""},
		{"an empty Forgejo service", func(v *Values) { v.ForgejoService = "" }, "Values.ForgejoService", "is empty"},
		{"an empty Forgejo namespace", func(v *Values) { v.ForgejoNamespace = "" }, "Values.ForgejoNamespace", "is empty"},
		{"a malformed Forgejo service", func(v *Values) { v.ForgejoService = "Forgejo_Svc" }, "Values.ForgejoService", ""},
		{"a malformed Forgejo namespace", func(v *Values) { v.ForgejoNamespace = "ci.prod" }, "Values.ForgejoNamespace", ""},
		{"a Secret entry with no name", func(v *Values) { v.Secrets[0].Name = "" }, "Values.Secrets[0]", "has no name"},
		{"a Secret entry that is not a DNS subdomain", func(v *Values) { v.Secrets[0].Name = "a_b" }, "Values.Secrets[0]", "DNS-1123 subdomain"},
		{"a Secret entry that is a path", func(v *Values) { v.Secrets[0].Name = "forgejo/landing.env" }, "Values.Secrets[0]", ""},
		{"a Secret entry that is absolute", func(v *Values) { v.Secrets[0].Name = "/etc/passwd" }, "Values.Secrets[0]", ""},
		{"a Secret entry that is ..", func(v *Values) { v.Secrets[0].Name = ".." }, "Values.Secrets[0]", ""},
		{"a Secret entry whose mode is readable by others", func(v *Values) { v.Secrets[0].Mode = "0644" }, "Values.Secrets[0]", "no group or other bits"},
		{"a Secret entry whose mode is not octal", func(v *Values) { v.Secrets[0].Mode = "rw-------" }, "Values.Secrets[0]", "octal"},
		{"a Secret entry whose mode has three digits", func(v *Values) { v.Secrets[0].Mode = "600" }, "Values.Secrets[0]", "four-digit octal"},
		{"a Secret entry whose mode has a leading digit", func(v *Values) { v.Secrets[0].Mode = "1600" }, "Values.Secrets[0]", "four-digit octal"},
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
		{"an env key the pod sets itself", func(v *Values) { v.Env[timeZoneEnv] = "Europe/Berlin" }, `Values.Env["TZ"]`, ""},
		{"an env key the pod drops", func(v *Values) { v.Env[cloudReportsEnv] = "/Users/Shared/gt-cloud/reports" }, `Values.Env["GT_CLOUD_REPORTS_DIR"]`, "drops"},
		{"an env name with no decision", func(v *Values) { v.Env["SOME_UNDECIDED"] = "x" }, `Values.Env["SOME_UNDECIDED"]`, "no decision"},
		{"a Dolt host the alias does not map", func(v *Values) { v.Env[doltServerHostEnv] = "localhost" }, `Values.Env["BEADS_DOLT_SERVER_HOST"]`, ""},
		{"a backup root away from the claim", func(v *Values) { v.Env[doltBackupRootEnv] = "/elsewhere/dolt" }, `Values.Env["GT_DOLT_BACKUP_ROOT"]`, ""},
		{"an env value that looks like a secret", func(v *Values) { v.Env[awsRegionEnv] = "ghp_0123456789abcdefghijklmnopqrstuvwxyz" }, `Values.Env["AWS_REGION"]`, ""},
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

// TestDoltBackupClaim holds the claim that carries the host's Dolt backups: it
// exists at the default size on the claims' class, and the pod mounts it at the
// path the compose file mounts read-write, while a rehearsal mounts it read-only
// so it can never write a backup of the live Dolt.
func TestDoltBackupClaim(t *testing.T) {
	t.Parallel()

	m := mustBuild(t, goldenValues(t))
	claim := m.DoltBackupClaim
	if claim.Metadata.Name != DoltBackupClaim || claim.Metadata.Namespace != DefaultNamespace {
		t.Errorf("the claim is %s/%s, want %s/%s", claim.Metadata.Namespace, claim.Metadata.Name, DefaultNamespace, DoltBackupClaim)
	}
	if got := claim.Spec.Resources.Requests["storage"]; got != DefaultDoltBackupStorage {
		t.Errorf("the %s claim's size = %q, want the default %q", DoltBackupClaim, got, DefaultDoltBackupStorage)
	}
	if got := claim.Spec.StorageClassName; got != DefaultStorageClass {
		t.Errorf("the %s claim's class = %q, want %q", DoltBackupClaim, got, DefaultStorageClass)
	}

	v := goldenValues(t)
	v.DoltBackupStorage = "40Gi"
	if got := mustBuild(t, v).DoltBackupClaim.Spec.Resources.Requests["storage"]; got != "40Gi" {
		t.Errorf("a caller's 40Gi rendered as %q, want the caller's size", got)
	}

	for _, tc := range []struct {
		name string
		v    Values
		// wantRO is what the mount at DoltBackupPath must carry.
		wantRO bool
	}{
		{"the live pod", goldenValues(t), false},
		{"a rehearsal", rehearsalValues(t), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := mustBuild(t, tc.v).Deployment.Spec.Template.Spec
			mount := volumeMountAt(pod.Containers[0].VolumeMounts, DoltBackupPath)
			if mount == nil {
				t.Fatalf("the pod mounts nothing at %s", DoltBackupPath)
			}
			if mount.Name != DoltBackupClaim {
				t.Errorf("the mount at %s carries the volume %q, want %q", DoltBackupPath, mount.Name, DoltBackupClaim)
			}
			if mount.ReadOnly != tc.wantRO {
				t.Errorf("the %s mount's readOnly = %t, want %t", DoltBackupClaim, mount.ReadOnly, tc.wantRO)
			}
			if got := volumeClaim(pod.Volumes, mount.Name); got != DoltBackupClaim {
				t.Errorf("the volume %q carries the claim %q, want %q", mount.Name, got, DoltBackupClaim)
			}
		})
	}
}

// TestHostGatewayAlias holds the pod's route to the host's Dolt server: a
// gateway IPv4 address renders one hostAlias for DoltHostName, every address
// that names no other machine is refused with the value named, and an empty
// value renders no alias with a comment on the Deployment saying so.
func TestHostGatewayAlias(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.HostGatewayIP = "10.0.5.7"
	pod := mustBuild(t, v).Deployment.Spec.Template.Spec
	if len(pod.HostAliases) != 1 {
		t.Fatalf("the pod carries %d hostAliases, want one", len(pod.HostAliases))
	}
	if got := pod.HostAliases[0]; got.IP != "10.0.5.7" || !slices.Equal(got.Hostnames, []string{DoltHostName}) {
		t.Errorf("the hostAlias = %+v, want 10.0.5.7 -> %s", got, DoltHostName)
	}

	for _, refused := range []string{
		"127.0.0.1", "127.10.0.1", // loopback
		"0.0.0.0",        // unspecified
		"::1", "fd00::1", // IPv6
		"host.gateway.local", "10.0.5", "10.0.5.256", // not an address
	} {
		v := goldenValues(t)
		v.HostGatewayIP = refused
		_, err := Build(v)
		if err == nil {
			t.Errorf("Build with HostGatewayIP %q = a render, want a refusal naming the value", refused)
			continue
		}
		if !strings.Contains(err.Error(), "Values.HostGatewayIP") || !strings.Contains(err.Error(), refused) {
			t.Errorf("Build refused with %q, want Values.HostGatewayIP and the value %q named", err, refused)
		}
	}

	v = goldenValues(t)
	v.HostGatewayIP = ""
	// The proxy relays need a host too, and TestProxySidecars holds that
	// refusal; this case is the alias alone.
	v.ProxyImage = ""
	m := mustBuild(t, v)
	rendered := document(t, m.Deployment)
	var parsed struct {
		Spec struct {
			Template struct {
				Spec struct {
					HostAliases []HostAlias `yaml:"hostAliases"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(rendered, &parsed); err != nil {
		t.Fatalf("parsing the rendered Deployment: %v", err)
	}
	if aliases := parsed.Spec.Template.Spec.HostAliases; len(aliases) != 0 {
		t.Errorf("the pod's rendered hostAliases = %+v with no gateway address, want none", aliases)
	}
	if !bytes.Contains(rendered, []byte("# "+noHostAliasComment)) {
		t.Errorf("the Deployment carries no comment saying it has no hostAlias:\n%s", rendered)
	}
	// The comment reaches the stream an apply reads, not only a single document.
	var stream bytes.Buffer
	if err := Render(&stream, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(stream.Bytes(), []byte("# "+noHostAliasComment)) {
		t.Errorf("the rendered stream carries no comment saying the pod has no hostAlias:\n%s", stream.Bytes())
	}

	// The comment is about the absence, so it is gone once the alias is there.
	v.HostGatewayIP = "10.0.5.7"
	if bytes.Contains(document(t, mustBuild(t, v).Deployment), []byte(noHostAliasComment)) {
		t.Error("the Deployment with a hostAlias still carries the no-alias comment")
	}
}

// TestProxySidecars holds the two relays a live pod carries in the compose
// override's place: registry-proxy and gateway-proxy listen on 127.0.0.1 of the
// pod's network namespace and forward to the host gateway, so the sandbox
// sling's localhost:5001 and localhost:28443 reach the host as they do under
// compose. It also holds the refusals: a tag, and a proxy image with no gateway
// address, since neither can render a relay that forwards.
func TestProxySidecars(t *testing.T) {
	t.Parallel()

	pod := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec
	host := goldenValues(t).HostGatewayIP
	if len(pod.Containers) != 3 {
		t.Fatalf("the pod carries %d containers, want the daemon and its two relays", len(pod.Containers))
	}
	if got := pod.Containers[0].Name; got != Name {
		t.Errorf("the first container is %q, want the daemon %q", got, Name)
	}
	byName := make(map[string]Container, len(pod.Containers))
	for _, c := range pod.Containers {
		byName[c.Name] = c
	}
	for _, tc := range []struct {
		name string
		port int
	}{
		{RegistryProxyContainer, RegistryProxyPort},
		{GatewayProxyContainer, GatewayProxyPort},
	} {
		c, ok := byName[tc.name]
		if !ok {
			t.Fatalf("the pod carries no %s container", tc.name)
		}
		if c.Image != testProxyImage {
			t.Errorf("%s image = %q, want the proxy digest %q", tc.name, c.Image, testProxyImage)
		}
		wantArgs := []string{
			fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr,bind=127.0.0.1", tc.port),
			fmt.Sprintf("TCP:%s:%d", host, tc.port),
		}
		if !slices.Equal(c.Args, wantArgs) {
			t.Errorf("%s args = %q, want %q", tc.name, c.Args, wantArgs)
		}
		if c.Resources == nil {
			t.Fatalf("%s carries no resources, want the fixed share", tc.name)
		}
		if want := (map[string]string{"cpu": proxyCPU, "memory": proxyMemory}); !maps.Equal(c.Resources.Requests, want) {
			t.Errorf("%s requests = %v, want the fixed share %v", tc.name, c.Resources.Requests, want)
		}
		if !maps.Equal(c.Resources.Requests, c.Resources.Limits) {
			t.Errorf("%s requests %v and limits %v; requests equal limits", tc.name, c.Resources.Requests, c.Resources.Limits)
		}
		if len(c.Env) != 0 || len(c.VolumeMounts) != 0 {
			t.Errorf("%s carries env %v and mounts %v, want neither", tc.name, c.Env, c.VolumeMounts)
		}
	}

	// Without a proxy image the pod carries no relay, and the Deployment says
	// so rather than leaving a reader to assume one.
	v := goldenValues(t)
	v.ProxyImage = ""
	m := mustBuild(t, v)
	if got := len(m.Deployment.Spec.Template.Spec.Containers); got != 1 {
		t.Errorf("the pod carries %d containers with no ProxyImage, want the daemon alone", got)
	}
	if rendered := document(t, m.Deployment); !bytes.Contains(rendered, []byte("# "+noProxyComment)) {
		t.Errorf("the Deployment with no ProxyImage carries no comment saying the pod has no relay:\n%s", rendered)
	}

	// A tag names whatever the registry holds at pull time, so it is refused
	// with the value a caller can correct.
	for _, image := range []string{
		"registry.example.com/gt/socat:1.8.0.3",
		"registry.example.com/gt/socat@sha256:FEEDFACE",
	} {
		v := goldenValues(t)
		v.ProxyImage = image
		_, err := Build(v)
		if err == nil {
			t.Errorf("Build with ProxyImage %q = a render, want a refusal", image)
			continue
		}
		if !strings.Contains(err.Error(), "Values.ProxyImage") || !strings.Contains(err.Error(), image) {
			t.Errorf("Build refused with %q, want Values.ProxyImage and the value %q named", err, image)
		}
	}

	// A relay with no host to forward to would listen and never forward.
	v = goldenValues(t)
	v.HostGatewayIP = ""
	_, err := Build(v)
	if err == nil {
		t.Fatal("Build with ProxyImage set and no HostGatewayIP = a render, want a refusal")
	}
	if !strings.Contains(err.Error(), "Values.HostGatewayIP") {
		t.Errorf("Build refused with %q, want Values.HostGatewayIP named", err)
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
		{DoltBackupClaim, m.DoltBackupClaim},
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
	for _, claim := range []PersistentVolumeClaim{m.TownStateClaim, m.LandingWorkClaim, m.DoltBackupClaim} {
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
	// The daemon-openshell Secret follows the entries; TestOpenShellSecretHasOneKeyPerFile
	// holds its shape.
	if len(m.Secrets) != len(v.Secrets)+1 {
		t.Fatalf("the renderer wrote %d Secrets for %d manifest entries and the openshell Secret", len(m.Secrets), len(v.Secrets))
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
func checkMount(mount Mount, m *manifestdata.Manifest) []string {
	var want []string
	if mount.Volume == TownStateClaim {
		want = ownedBy(m, manifestdata.OwnerDaemon)
	}
	var findings []string
	if !slices.Equal(mount.Entries, want) {
		findings = append(findings, fmt.Sprintf("the %s mount carries %q, want %q: the split's daemon-owned rows, in the split's order", mount.Volume, mount.Entries, want))
	}
	sessionPaths := ownedBy(m, manifestdata.OwnerSession)
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
	manifest := (*manifestdata.Manifest)(townstate.Default())
	daemonPaths, sessionPaths := ownedBy(manifest, manifestdata.OwnerDaemon), ownedBy(manifest, manifestdata.OwnerSession)
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
	backup, ok := mountFor(DoltBackupClaim)
	if !ok {
		t.Fatalf("Mounts holds no %s volume", DoltBackupClaim)
	}
	if got, want := backup.Path, DoltBackupPath; got != want {
		t.Errorf("the %s mount is at %q and DoltBackupPath is %q, want the compose mount's %q", DoltBackupClaim, got, DoltBackupPath, want)
	}
	for _, p := range []string{TownRootPath, LandingWorkPath, DoltBackupPath, TempPath} {
		if !path.IsAbs(p) {
			t.Errorf("the mount path %q is not absolute; the pod mounts it at the path the container uses", p)
		}
	}

	// Each mount the live compose daemon carries beyond the town bind and the
	// Secrets has a row in the render or a named reason it has none.
	// TestEveryComposeMountHasAPodAnswer holds the table's own rows.
	rendered := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec.Containers[0].VolumeMounts
	for _, m := range composeMounts {
		if finding := podAnswerFinding(m, rendered); finding != "" {
			t.Error(finding)
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
			findings := checkMount(tc.mount, (*manifestdata.Manifest)(tc.split))
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

// testPolecatImage is the polecat image digest the polecat-image golden records,
// shaped as the values file writes one. Its repository and digest differ from the
// daemon's and the relays', so a test cannot pass one off as another.
const testPolecatImage = "registry.example.com/gt/gastown-polecat@sha256:" +
	"5e7a1c0d9b3f24e68a0c1d7f9b2e4a6c8d0f1b3e5a7c9d2f4b6e8a0c1d3f5b7e"

// TestPolecatImageIsAnnotatedAndProjected holds a digest PolecatImage to the pod
// template's annotation, to a read-only downward-API file at PolecatImagePath, and
// to the golden that shows both.
func TestPolecatImageIsAnnotatedAndProjected(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.PolecatImage = testPolecatImage
	m := mustBuild(t, v)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-polecat-image.yaml", document(t, m.Deployment))

	pod := m.Deployment.Spec.Template
	if got := pod.Metadata.Annotations["gastown.dev/polecat-image"]; got != testPolecatImage {
		t.Errorf("the pod template's gastown.dev/polecat-image annotation = %q, want the digest %q", got, testPolecatImage)
	}

	var vol *Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "polecat-image" {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.DownwardAPI == nil {
		t.Fatalf("the pod carries no downward-API volume named polecat-image: %+v", pod.Spec.Volumes)
	}
	items := vol.DownwardAPI.Items
	if len(items) != 1 || items[0].Path != "polecat-image" || items[0].FieldRef == nil ||
		items[0].FieldRef.FieldPath != "metadata.annotations['gastown.dev/polecat-image']" {
		t.Errorf("the volume's items = %+v, want the annotation projected as the file polecat-image", items)
	}
	if mode := vol.DownwardAPI.DefaultMode; mode == nil || *mode != 0o444 {
		t.Errorf("the volume's defaultMode = %v, want 0444: the file is read-only", mode)
	}

	var mount *VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].MountPath == "/run/gt/image/polecat-image" {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("the daemon container mounts nothing at /run/gt/image/polecat-image")
	}
	if mount.Name != "polecat-image" || mount.SubPath != "polecat-image" || !mount.ReadOnly {
		t.Errorf("the polecat-image mount = %+v, want the volume's file read-only by subPath", mount)
	}
}

// TestPolecatImageEmptyRendersToday holds the Deployment with no PolecatImage to
// the golden the render wrote before the field existed, byte for byte.
func TestPolecatImageEmptyRendersToday(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.PolecatImage = ""
	m := mustBuild(t, v)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment.yaml", document(t, m.Deployment))
}

// TestPolecatImageRefusesATag holds a tag, a digest-less reference and a short
// digest to a refusal that names the field and the value, as ProxyImage's does.
func TestPolecatImageRefusesATag(t *testing.T) {
	t.Parallel()
	for _, image := range []string{
		"registry.example.com/gt/gastown-polecat:1.8.0.3",
		"registry.example.com/gt/gastown-polecat",
		"registry.example.com/gt/gastown-polecat@sha256:FEEDFACE",
	} {
		v := goldenValues(t)
		v.PolecatImage = image
		_, err := Build(v)
		if err == nil {
			t.Errorf("Build with PolecatImage %q = a render, want a refusal", image)
			continue
		}
		if !strings.Contains(err.Error(), "Values.PolecatImage") || !strings.Contains(err.Error(), image) {
			t.Errorf("Build refused with %q, want Values.PolecatImage and the value %q named", err, image)
		}
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
	if got := *d.Spec.Template.Spec.TerminationGracePeriodSeconds; got < int64(MinimumTerminationGraceSeconds) {
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

// daemonDuration returns the duration internal/daemon declares for name in the
// given source file. It reads the declaration as text, because the import rule
// keeps internal/daemon out of this package. The right-hand side is a literal
// `N * time.Unit`, or the name of one other constant in the same file, which is
// followed once (ShutdownBudget = doltServerStopBudget). Anything else fails the
// test, so a declaration the test cannot read is never taken as a zero budget.
func daemonDuration(t *testing.T, file, name string) time.Duration {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(packageDir(t), "..", "..", "..", "internal", "daemon", file))
	if err != nil {
		t.Fatalf("reading internal/daemon/%s: %v", file, err)
	}
	rhs := daemonDeclaration(t, data, file, name)
	if alias := regexp.MustCompile(`^[A-Za-z_]\w*$`); alias.MatchString(rhs) {
		rhs = daemonDeclaration(t, data, file, rhs)
	}
	lit := regexp.MustCompile(`^(\d+)\s*\*\s*time\.(Second|Minute)$`).FindStringSubmatch(rhs)
	if lit == nil {
		t.Fatalf("internal/daemon/%s: %s = %q, want a literal N * time.Second or time.Minute", file, name, rhs)
	}
	n, err := strconv.Atoi(lit[1])
	if err != nil {
		t.Fatalf("internal/daemon/%s: %s = %q: %v", file, name, rhs, err)
	}
	if lit[2] == "Minute" {
		return time.Duration(n) * time.Minute
	}
	return time.Duration(n) * time.Second
}

// daemonDeclaration returns the right-hand side of the top-level declaration of
// name in data, and fails the test when the file declares no such name.
func daemonDeclaration(t *testing.T, data []byte, file, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*(?:const\s+)?` + regexp.QuoteMeta(name) + `\s*=\s*(.+?)\s*$`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("internal/daemon/%s declares no %s", file, name)
	}
	return string(m[1])
}

// TestDeploymentRendersAZeroGracePeriod holds that a grace period of zero renders
// as 0. An omitted terminationGracePeriodSeconds takes the cluster's default of
// 30 seconds, which is not the zero the caller named. Build defaults a zero, so
// this calls deployment, the function Build renders the pod with.
func TestDeploymentRendersAZeroGracePeriod(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.TerminationGraceSeconds = 0
	var rendered struct {
		Spec struct {
			Template struct {
				Spec struct {
					TerminationGracePeriodSeconds *int64 `yaml:"terminationGracePeriodSeconds"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	dep, err := deployment(v, DefaultNamespace)
	if err != nil {
		t.Fatalf("rendering the Deployment: %v", err)
	}
	if err := yaml.Unmarshal(document(t, dep), &rendered); err != nil {
		t.Fatalf("parsing the rendered Deployment: %v", err)
	}
	got := rendered.Spec.Template.Spec.TerminationGracePeriodSeconds
	if got == nil {
		t.Fatal("the rendered Deployment omits terminationGracePeriodSeconds, so a zero grace period takes the cluster default instead of 0")
	}
	if *got != 0 {
		t.Errorf("the rendered grace period = %d, want 0 for a zero Values.TerminationGraceSeconds", *got)
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
	// The two budgets are declared in internal/daemon. The copies above must match
	// those declarations, so a change to either budget there fails this test.
	if got := daemonDuration(t, "dolt.go", "ShutdownBudget"); got != daemonShutdownBudget {
		t.Errorf("internal/daemon ShutdownBudget is %s, but daemonShutdownBudget is %s", got, daemonShutdownBudget)
	}
	if got := daemonDuration(t, "upgrade_restart.go", "postLandRestartCap"); got != landingPassDrainBound {
		t.Errorf("internal/daemon postLandRestartCap is %s, but landingPassDrainBound is %s", got, landingPassDrainBound)
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
	if got, want := *pod.TerminationGracePeriodSeconds, int64(DefaultTerminationGraceSeconds); got != want {
		t.Errorf("the pod's grace period = %ds, want the default %ds", got, want)
	}
	if got := *pod.TerminationGracePeriodSeconds; got < int64(MinimumTerminationGraceSeconds) {
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
	if got, want := *mustBuild(t, v).Deployment.Spec.Template.Spec.TerminationGracePeriodSeconds, int64(MinimumTerminationGraceSeconds); got != want {
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
	v.Env[awsRegionEnv] = secret
	_, err = Build(v)
	if err == nil {
		t.Fatal("Build with a secret-looking env value = a render, want a refusal")
	}
	if !strings.Contains(err.Error(), awsRegionEnv) {
		t.Errorf("Build refused with %q, want the key named", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the refusal echoes the secret value, and a manifest never carries one")
	}
}

// liveEnv is the map the compose writer hands the pod as Values.Env: the
// daemon.env entries the pod keeps, with the live values where gt-70plvt records
// them. The kubeconfig and the clock are not here: the compose render sets those
// itself, as the pod does. The entries the pod drops are macOnlyEnv's.
func liveEnv() map[string]string {
	return map[string]string{
		doltPoolReadTimeoutEnv: "30s",
		doltServerHostEnv:      DoltHostName,
		doltServerPortEnv:      "3307",
		doltBackupRootEnv:      DoltBackupPath,
		awsRegionEnv:           "us-east-1",
	}
}

// macOnlyEnv is the daemon.env entries the Mac's container carries and the pod
// drops: the cloud directories under /Users/Shared/gt-cloud, which the host
// wrapper mounts and the pod does not. Values.Env refuses them.
func macOnlyEnv() map[string]string {
	return map[string]string{
		cloudReportsEnv:  "/Users/Shared/gt-cloud/reports",
		cloudReleasesEnv: "/Users/Shared/gt-cloud/releases",
		cloudOffEnv:      "/Users/Shared/gt-cloud/off",
	}
}

// liveDaemonEnv is the Mac's whole daemon.env: the entries the pod keeps and the
// entries it drops. The compose writer renders it as ComposeSpec.Env.
func liveDaemonEnv() map[string]string {
	env := liveEnv()
	maps.Copy(env, macOnlyEnv())
	return env
}

// liveComposeSpec is the live compose spec the checks render. It carries the
// Mac's daemon.env as Env and every client field the render derives a name from:
// KUBECONFIG, the clock, the config directory and HM_CONFIG.
func liveComposeSpec(t *testing.T, env map[string]string) templates.ComposeSpec {
	t.Helper()
	return templates.ComposeSpec{
		Project:       "gt-town",
		ContainerName: "gt-daemon",
		Image:         "gt-daemon:abc1234",
		TownRoot:      TownRootPath,
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      DoltHostName,
		Env:           maps.Clone(env),
		KubeconfigFiles: []string{
			TownRootPath + "/.runtime/container/kubeconfig.yaml",
			TownRootPath + "/.runtime/sbx-k8s/kubeconfig",
		},
		TimeZone:     PodTimeZone,
		HMConfigFile: filepath.Join(t.TempDir(), "hm-config.yaml"),
		// The config directory is what makes the compose render set
		// XDG_CONFIG_HOME, the render-owned name both services carry. No Secrets
		// are listed, so the render mounts the directory and reads no host file.
		ConfigDir: t.TempDir(),
	}
}

// composeDaemonEnv renders the spec and returns the daemon service's environment.
func composeDaemonEnv(t *testing.T, spec templates.ComposeSpec) map[string]string {
	t.Helper()
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
	return composeFile.Services["daemon"].Environment
}

// liveEnvNames are every name the live compose daemon service carries, read from
// the rendered file, and every entry of the Mac's daemon.env. The compose body's
// fixed lines and the client names it derives come from the render itself, so a
// name added there needs a decision without a second list here.
func liveEnvNames(t *testing.T) []string {
	t.Helper()
	names := composeDaemonEnv(t, liveComposeSpec(t, liveDaemonEnv()))
	maps.Copy(names, liveDaemonEnv())
	return slices.Sorted(maps.Keys(names))
}

// podEnvMap indexes the container's environment by name.
func podEnvMap(pod Container) map[string]string {
	out := make(map[string]string, len(pod.Env))
	for _, e := range pod.Env {
		out[e.Name] = e.Value
	}
	return out
}

// TestPodEnvDecisions holds the decision table to the live name set: every name
// the compose writer or daemon.env carries has exactly one decision, the golden
// records the decided pod environment, and a name with no decision is refused
// naming it rather than carried undecided.
func TestPodEnvDecisions(t *testing.T) {
	t.Parallel()

	for _, name := range liveEnvNames(t) {
		decision, ok := podEnvDecisions[name]
		if !ok {
			t.Errorf("the pod has no decision for %s, a name the compose service or daemon.env carries; record it as kept, set by the pod, or dropped with a reason", name)
			continue
		}
		kinds := 0
		if decision.keep {
			kinds++
		}
		if decision.value != "" {
			kinds++
		}
		if decision.drop != "" {
			kinds++
		}
		if kinds != 1 {
			t.Errorf("podEnvDecisions[%s] = %+v, want exactly one of keep, value or drop", name, decision)
		}
	}
	for name, reason := range composeOnlyEnv {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("composeOnlyEnv[%s] names no reason, so the difference is dropped silently", name)
		}
	}

	// The zone is the live one: the 04:30 maintenance window reads local time.
	if PodTimeZone != "America/Chicago" {
		t.Errorf("PodTimeZone = %q, want the live container's America/Chicago", PodTimeZone)
	}
	if _, err := time.LoadLocation(PodTimeZone); err != nil {
		t.Errorf("PodTimeZone %q does not load as a zone: %v", PodTimeZone, err)
	}

	undecided := goldenValues(t)
	undecided.Env["SOME_UNDECIDED"] = "x"
	_, err := Build(undecided)
	if err == nil || !strings.Contains(err.Error(), "SOME_UNDECIDED") {
		t.Errorf("Build with an undecided env name = %v, want a refusal naming SOME_UNDECIDED", err)
	}

	v := goldenValues(t)
	v.Env = liveEnv()
	m := mustBuild(t, v)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "pod-env.yaml",
		document(t, m.Deployment.Spec.Template.Spec.Containers[0].Env))
}

// TestPodEnvEqualsTheComposeEnv renders the live compose file with the pod's
// Values and holds the pod's environment equal to the compose service's after
// the names composeOnlyEnv drops: the pod keeps the same Dolt names and value
// for value, sets its own identity, and the four compose-only names plus the
// Mac kubeconfig and cloud paths are absent from the pod. Every difference is
// named, with its reason, in composeOnlyEnv or is a value the pod sets itself.
func TestPodEnvEqualsTheComposeEnv(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.Env = liveEnv()
	composeFileEnv := composeDaemonEnv(t, liveComposeSpec(t, liveDaemonEnv()))
	composeEnv := maps.Clone(composeFileEnv)
	if len(composeEnv) == 0 {
		t.Fatal("the compose daemon service carries no environment, so the equality check reads nothing")
	}

	pod := mustBuild(t, v).Deployment.Spec.Template.Spec.Containers[0]
	podEnv := podEnvMap(pod)

	// The dropped names must be present in the compose environment and absent
	// from the pod's, so removing them below is not a check over nothing.
	for name := range composeOnlyEnv {
		if _, ok := composeEnv[name]; !ok {
			t.Fatalf("the compose service carries no %s, so the drop it is meant to prove reads nothing", name)
		}
		delete(composeEnv, name)
		if _, carried := podEnv[name]; carried {
			t.Errorf("the pod carries %s = %q, which the pod drops: %s", name, podEnv[name], composeOnlyEnv[name])
		}
	}

	// The pod's identity is not the compose service's: the container name is
	// the Deployment's, stable across restarts, and the supervisor is what the
	// entrypoint requires.
	for _, name := range []string{ContainerNameEnv, ContainerModeEnv} {
		want := podEnvDecisions[name].value
		if got, ok := podEnv[name]; !ok || got != want {
			t.Errorf("the pod's %s = %q (present %t), want the decided %q", name, got, ok, want)
		}
		delete(composeEnv, name)
		delete(podEnv, name)
	}
	switch got := composeFileEnv[ContainerNameEnv]; got {
	case "":
		t.Errorf("the compose service carries no %s, so the changed decision reads nothing", ContainerNameEnv)
	case Name:
		t.Errorf("the compose service's %s is the pod's %q, so the changed decision is not exercised", ContainerNameEnv, got)
	}

	if !maps.Equal(podEnv, composeEnv) {
		t.Errorf("the pod's environment and the compose daemon service's differ after the compose-only names %q: pod names %q, compose names %q",
			slices.Sorted(maps.Keys(composeOnlyEnv)), slices.Sorted(maps.Keys(podEnv)), slices.Sorted(maps.Keys(composeEnv)))
		for name, got := range podEnv {
			if want, ok := composeEnv[name]; ok && want != got {
				t.Errorf("env %s = %q in the pod, %q in the compose service", name, got, want)
			}
		}
		for name, want := range composeEnv {
			if _, ok := podEnv[name]; !ok {
				t.Errorf("env %s = %q in the compose service, absent from the pod", name, want)
			}
		}
	}
}

// townRootKubeconfigs reports every environment value that names a kubeconfig
// under the town root. The Mac writes .runtime/container/kubeconfig.yaml and
// .runtime/sbx-k8s/kubeconfig there; a pod carrying either path would send
// kubectl at a file and an API address that are not its own.
func townRootKubeconfigs(env []EnvVar) []string {
	var out []string
	for _, e := range env {
		for _, p := range strings.Split(e.Value, ":") {
			if !strings.HasPrefix(p, TownRootPath+"/") {
				continue
			}
			if strings.Contains(strings.ToLower(path.Base(p)), "kubeconfig") {
				out = append(out, e.Name+"="+e.Value)
			}
		}
	}
	return out
}

// TestPodKubeconfigIsNotATownRootFile holds the kubeconfig decision: a caller
// that hands the pod the Mac's two kubeconfig files is refused, naming the key,
// because the pod reaches the cluster through its service account and a dropped
// name is refused rather than discarded.
func TestPodKubeconfigIsNotATownRootFile(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.Env[kubeconfigEnv] = TownRootPath + "/.runtime/container/kubeconfig.yaml:" +
		TownRootPath + "/.runtime/sbx-k8s/kubeconfig"
	if _, err := Build(v); err == nil || !strings.Contains(err.Error(), kubeconfigEnv) {
		t.Errorf("Build with the Mac's %s = %v, want a refusal naming the key", kubeconfigEnv, err)
	}
	// The check reports both files of the live value, so the empty result above
	// is not a check over nothing.
	if got := townRootKubeconfigs([]EnvVar{{Name: kubeconfigEnv, Value: v.Env[kubeconfigEnv]}}); len(got) != 2 {
		t.Errorf("townRootKubeconfigs reported %q for the live value, want both files named", got)
	}
}

// composeMount is one mount the live compose daemon carries beyond the town bind
// and the Secrets: its container path and the pod's answer. A row with a Volume
// answers the mount with a read-only mount of that volume's SubPath at Path, as
// the compose token mounts are; a row with no Volume names the Reason the pod
// carries no mount for it.
type composeMount struct {
	Name    string
	Path    string
	Volume  string
	SubPath string
	Reason  string
}

// composeMounts is the pod's answer to each mount the compose daemon adds
// (internal/templates/daemon_compose.go liveTokenMounts and
// daemon_compose_client.go liveClientMounts). TestEveryComposeMountHasAPodAnswer
// holds each row.
var composeMounts = []composeMount{
	{
		// The daemon-openshell Secret serves the sandbox seats' gateway calls from
		// the pod (gt-nr548n).
		Name: "openshell config", Path: manifestdata.LiveOpenShellConfigPath,
		Volume: openShellVolumeName,
	},
	{
		Name: "om config", Path: manifestdata.LiveOMConfigPath,
		Volume: TownStateClaim, SubPath: OMConfigSubPath,
	},
	{
		Name: "claude beads", Path: ClaudeBeadsRoot,
		Reason: "dropped for the pod: the reason is written in noClaudeBeadsComment",
	},
}

// podAnswerFinding returns what is wrong with the pod's answer to m against the
// rendered daemon mounts, or "" when the answer holds. An answered mount must be
// a read-only mount of its volume's subPath at the compose path; an unanswered
// one must name a reason. The finding names the mount.
func podAnswerFinding(m composeMount, rendered []VolumeMount) string {
	if m.Volume == "" {
		if m.Reason == "" {
			return fmt.Sprintf("the compose %s mount at %s has no pod row and no reason the pod has none", m.Name, m.Path)
		}
		return ""
	}
	got := volumeMountAt(rendered, m.Path)
	switch {
	case got == nil:
		return fmt.Sprintf("the compose %s mount at %s has no pod row: the pod mounts nothing there", m.Name, m.Path)
	case got.Name != m.Volume || got.SubPath != m.SubPath:
		return fmt.Sprintf("the pod's mount at %s carries volume %q subPath %q, want the compose %s mount's volume %q subPath %q", m.Path, got.Name, got.SubPath, m.Name, m.Volume, m.SubPath)
	case !got.ReadOnly:
		return fmt.Sprintf("the pod's mount for the compose %s mount at %s is writable; the compose mount is read-only", m.Name, m.Path)
	}
	return ""
}

// claudeBeadsFinding returns what is wrong with the claude beads root on the
// rendered Deployment, or "" when no container mounts it and the head comment
// names it. The drop is enforced by the absent mount; the comment only tells a
// reader of the manifest. A mount is a finding even with the comment present.
// The finding names the root.
func claudeBeadsFinding(d Deployment) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		if volumeMountAt(c.VolumeMounts, ClaudeBeadsRoot) != nil {
			return fmt.Sprintf("the pod mounts %s, which the render drops: the pod has no claude beads database", ClaudeBeadsRoot)
		}
	}
	if !strings.Contains(d.headComment, ClaudeBeadsRoot) {
		return fmt.Sprintf("the pod mounts no %s and the Deployment's head comment names no %s", ClaudeBeadsRoot, ClaudeBeadsRoot)
	}
	return ""
}

// TestPodDoesNotReadStaleKubeconfigs holds the kubeconfig decision on the whole
// render, not only on the environment value: no env value, argument, mount path
// or subPath names the .runtime container or sbx-k8s directory, the two
// kubeconfig files the Mac writes with its own API addresses. The one reference
// the pod makes into .runtime/container is the om config's subPath, which carries
// no credential, so the test names it and holds it to that one path.
func TestPodDoesNotReadStaleKubeconfigs(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec
	staleDirs := []string{".runtime/container", ".runtime/sbx-k8s"}
	var refs []string
	scan := func(where, s string) {
		for _, dir := range staleDirs {
			if strings.Contains(s, dir) {
				refs = append(refs, where+" "+s)
			}
		}
	}
	for _, c := range pod.Containers {
		for _, e := range c.Env {
			if e.Name == kubeconfigEnv {
				t.Errorf("the pod sets %s, which names the Mac's kubeconfigs", kubeconfigEnv)
			}
			scan("env "+e.Name, e.Value)
		}
		for _, a := range c.Args {
			scan("argument", a)
		}
		for _, m := range c.VolumeMounts {
			scan("mount at", m.MountPath)
			if m.SubPath != "" {
				scan("subPath of "+m.MountPath, m.SubPath)
			}
		}
	}
	// The path is spelled out, not read from OMConfigSubPath: a constant the test
	// compares against cannot catch that constant moving into a stale directory.
	want := "subPath of " + manifestdata.LiveOMConfigPath + " .runtime/container/om-config.json"
	if len(refs) != 1 || refs[0] != want {
		t.Errorf("the render names %q under the stale directories, want only %q", refs, want)
	}
}

// TestEveryComposeMountHasAPodAnswer holds each mount the live compose daemon
// carries beyond the town bind and the Secrets to a pod answer: a render row at
// the compose path, or a named reason the pod has none. An entry with neither is
// refused, naming the mount.
func TestEveryComposeMountHasAPodAnswer(t *testing.T) {
	t.Parallel()
	rendered := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec.Containers[0].VolumeMounts

	names := make(map[string]bool, len(composeMounts))
	for _, m := range composeMounts {
		names[m.Name] = true
		if finding := podAnswerFinding(m, rendered); finding != "" {
			t.Error(finding)
		}
	}
	for _, want := range []string{"openshell config", "om config", "claude beads"} {
		if !names[want] {
			t.Errorf("composeMounts has no %q entry, though the compose daemon mounts it", want)
		}
	}

	neither := composeMount{Name: "an unanswered mount", Path: "/run/gt/xdg/unanswered"}
	if got := podAnswerFinding(neither, rendered); !strings.Contains(got, "an unanswered mount") {
		t.Errorf("podAnswerFinding on an entry with neither a row nor a reason = %q, want a finding naming it", got)
	}
	absent := composeMount{Name: "an om config the pod does not mount", Path: manifestdata.LiveOMConfigPath, Volume: TownStateClaim, SubPath: OMConfigSubPath}
	if got := podAnswerFinding(absent, nil); !strings.Contains(got, "an om config the pod does not mount") {
		t.Errorf("podAnswerFinding on a row the render does not carry = %q, want a finding naming it", got)
	}
	wrongSubPath := composeMount{Name: "an om config at the wrong subPath", Path: manifestdata.LiveOMConfigPath, Volume: TownStateClaim, SubPath: "elsewhere/om-config.json"}
	if got := podAnswerFinding(wrongSubPath, rendered); !strings.Contains(got, "an om config at the wrong subPath") {
		t.Errorf("podAnswerFinding on a render with another subPath = %q, want a finding naming it", got)
	}
	writable := append([]VolumeMount(nil), rendered...)
	if m := volumeMountAt(writable, manifestdata.LiveOMConfigPath); m != nil {
		m.ReadOnly = false
	}
	var om composeMount
	for _, m := range composeMounts {
		if m.Name == "om config" {
			om = m
		}
	}
	if got := podAnswerFinding(om, writable); !strings.Contains(got, "is writable") {
		t.Errorf("podAnswerFinding on a writable om config mount = %q, want a finding saying it is writable", got)
	}
}

// TestOMConfigComesFromThePrivateTownStateRow holds the om config's source: the
// file sits under the .runtime/container row, which is daemon-owned and Private,
// so the town-state volume carries it and the copy keeps the mode the daemon
// wrote it with. A non-Private row would lose that mode on the copy. The
// config's own mode is set by the compose writer, not by this row.
func TestOMConfigComesFromThePrivateTownStateRow(t *testing.T) {
	t.Parallel()
	manifest := (*manifestdata.Manifest)(townstate.Default())
	var covering *manifestdata.Entry
	for i := range manifest.Entries {
		e := &manifest.Entries[i]
		if OMConfigSubPath == e.Path || strings.HasPrefix(OMConfigSubPath, e.Path+"/") {
			covering = e
		}
	}
	if covering == nil {
		t.Fatalf("no manifest row covers %s, so the town-state volume does not carry the om config", OMConfigSubPath)
	}
	if covering.Path != ".runtime/container" || covering.Owner != manifestdata.OwnerDaemon || !covering.Private {
		t.Errorf("the row covering %s is %+v, want the daemon-owned Private .runtime/container row", OMConfigSubPath, *covering)
	}
}

// TestClaudeBeadsDirIsMountedOrDropped holds the claude beads decision: the pod
// mounts nothing at the host's ~/.claude/.beads, and the Deployment names the
// root. A mount in any container is refused, naming the root, even when the
// comment is present; so is a render that names the root nowhere.
func TestClaudeBeadsDirIsMountedOrDropped(t *testing.T) {
	t.Parallel()
	dropped := mustBuild(t, goldenValues(t)).Deployment
	if finding := claudeBeadsFinding(dropped); finding != "" {
		t.Error(finding)
	}
	if !bytes.Contains(document(t, dropped), []byte(ClaudeBeadsRoot)) {
		t.Errorf("the rendered Deployment does not name %s, so the drop is unexplained", ClaudeBeadsRoot)
	}

	mounted := mustBuild(t, goldenValues(t)).Deployment
	c := &mounted.Spec.Template.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, VolumeMount{Name: "claude-beads", MountPath: ClaudeBeadsRoot})
	if finding := claudeBeadsFinding(mounted); !strings.Contains(finding, ClaudeBeadsRoot) {
		t.Errorf("claudeBeadsFinding on a render that mounts %s = %q, want a finding naming it", ClaudeBeadsRoot, finding)
	}

	neither := mustBuild(t, goldenValues(t)).Deployment
	neither.headComment = ""
	if finding := claudeBeadsFinding(neither); !strings.Contains(finding, ClaudeBeadsRoot) {
		t.Errorf("claudeBeadsFinding on a render with neither a mount nor the comment = %q, want a finding naming %s", finding, ClaudeBeadsRoot)
	}
}

// openShellWantPaths is the openshell config tree's eleven files, each key mapped
// to the nested path the pod mounts it at. It is written out here, not read from
// openShellKeys, so a key that drifts from its path fails against this table.
var openShellWantPaths = map[string]string{
	"gateway.toml":                       "gateway.toml",
	"active_gateway":                     "active_gateway",
	"gateways__k8s__metadata.json":       "gateways/k8s/metadata.json",
	"gateways__k8s__last_sandbox":        "gateways/k8s/last_sandbox",
	"gateways__k8s__mtls__ca.crt":        "gateways/k8s/mtls/ca.crt",
	"gateways__k8s__mtls__tls.crt":       "gateways/k8s/mtls/tls.crt",
	"gateways__k8s__mtls__tls.key":       "gateways/k8s/mtls/tls.key",
	"gateways__openshell__metadata.json": "gateways/openshell/metadata.json",
	"gateways__openshell__mtls__ca.crt":  "gateways/openshell/mtls/ca.crt",
	"gateways__openshell__mtls__tls.crt": "gateways/openshell/mtls/tls.crt",
	"gateways__openshell__mtls__tls.key": "gateways/openshell/mtls/tls.key",
}

// openShellSecretFor returns the daemon-openshell Secret m renders, failing the
// test when there is none.
func openShellSecretFor(t *testing.T, m Manifests) Secret {
	t.Helper()
	for _, s := range m.Secrets {
		if s.Metadata.Name == "daemon-openshell" {
			return s
		}
	}
	t.Fatal("the render writes no daemon-openshell Secret")
	return Secret{}
}

// TestOpenShellSecretHasOneKeyPerFile holds the Secret's shape: one key per file
// of the openshell tree, no key holds a slash, and every key is the file's nested
// path with each folder separator written as a double underscore (gt-nr548n).
func TestOpenShellSecretHasOneKeyPerFile(t *testing.T) {
	t.Parallel()
	s := openShellSecretFor(t, mustBuild(t, goldenValues(t)))
	if len(s.StringData) != len(openShellWantPaths) {
		t.Errorf("the daemon-openshell Secret has %d keys, want %d, one per file", len(s.StringData), len(openShellWantPaths))
	}
	for key := range s.StringData {
		if strings.Contains(key, "/") {
			t.Errorf("the daemon-openshell key %q holds a slash; a Secret key cannot", key)
		}
		want, ok := openShellWantPaths[key]
		if !ok {
			t.Errorf("the daemon-openshell Secret has key %q, which names no file of the openshell tree", key)
			continue
		}
		if got := strings.ReplaceAll(key, "__", "/"); got != want {
			t.Errorf("key %q maps to %q, want %q", key, got, want)
		}
	}
}

// TestOpenShellVolumeMapsEveryKeyAndIsPrivate holds the volume: exactly one item
// per key, each item mapping its key to the nested path, and defaultMode 0400 so
// the tls.key files are private in the pod.
func TestOpenShellVolumeMapsEveryKeyAndIsPrivate(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))
	var vol *Volume
	for i := range m.Deployment.Spec.Template.Spec.Volumes {
		if v := &m.Deployment.Spec.Template.Spec.Volumes[i]; v.Name == "daemon-openshell" {
			vol = v
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatal("the pod has no daemon-openshell Secret volume")
	}
	if vol.Secret.SecretName != "daemon-openshell" {
		t.Errorf("the volume reads Secret %q, want daemon-openshell", vol.Secret.SecretName)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o400 {
		t.Errorf("the volume's defaultMode = %v, want 0400", vol.Secret.DefaultMode)
	}
	if len(vol.Secret.Items) != len(openShellWantPaths) {
		t.Errorf("the volume has %d items, want %d, one per key", len(vol.Secret.Items), len(openShellWantPaths))
	}
	for _, item := range vol.Secret.Items {
		if want := openShellWantPaths[item.Key]; want == "" || item.Path != want {
			t.Errorf("item %q maps to %q, want %q", item.Key, item.Path, want)
		}
	}
}

// TestOpenShellMountIsReadOnlyAtConfigHome holds the mount: the openshell config
// is mounted read-only at the exact path beneath XDG_CONFIG_HOME, and that env
// value names the mount's parent, so the daemon's openshell finds its gateways
// there (gt-nr548n).
func TestOpenShellMountIsReadOnlyAtConfigHome(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec
	const mountAt = "/run/gt/xdg/openshell"
	c := pod.Containers[0]
	m := volumeMountAt(c.VolumeMounts, mountAt)
	if m == nil {
		t.Fatalf("the daemon container mounts nothing at %s", mountAt)
	}
	if m.Name != "daemon-openshell" || m.SubPath != "" {
		t.Errorf("the mount at %s carries volume %q subPath %q, want the daemon-openshell volume and no subPath", mountAt, m.Name, m.SubPath)
	}
	if !m.ReadOnly {
		t.Errorf("the mount at %s is writable, want read-only", mountAt)
	}
	var configHome string
	for _, e := range c.Env {
		if e.Name == ConfigHomeEnv {
			configHome = e.Value
		}
	}
	if path.Dir(mountAt) != configHome {
		t.Errorf("%s = %q, want the openshell mount's parent %q", ConfigHomeEnv, configHome, path.Dir(mountAt))
	}
}

// TestOpenShellIsAbsentFromRehearsal holds the rehearsal's credential allowlist:
// a rehearsal renders neither the openshell Secret, its volume nor its mount.
func TestOpenShellIsAbsentFromRehearsal(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, rehearsalValues(t))
	for _, s := range m.Secrets {
		if s.Metadata.Name == "daemon-openshell" {
			t.Errorf("the rehearsal renders the daemon-openshell Secret, which it must not carry")
		}
	}
	for _, v := range m.Deployment.Spec.Template.Spec.Volumes {
		if v.Name == "daemon-openshell" {
			t.Errorf("the rehearsal renders the daemon-openshell volume")
		}
	}
	if volumeMountAt(m.Deployment.Spec.Template.Spec.Containers[0].VolumeMounts, "/run/gt/xdg/openshell") != nil {
		t.Errorf("the rehearsal mounts the openshell config")
	}
}

// openShellModeFinding returns what is wrong with a Secret volume's mode under an
// image whose final stage carries users and a pod whose security context is sc,
// or "" when the daemon can read the volume. The files are root-owned, so a
// non-root daemon reads them only through group read on an fsGroup, or world
// read; an fsGroup alone does not admit a 0400 file.
func openShellModeFinding(mode int32, users []string, sc *PodSecurityContext) string {
	if len(users) == 0 {
		return ""
	}
	if (sc != nil && sc.FSGroup != nil && mode&0o040 != 0) || mode&0o004 != 0 {
		return ""
	}
	return fmt.Sprintf("the daemon image's final stage carries %q, so the daemon pod does not run as root; a non-root daemon cannot read a root-owned %04o Secret; set fsGroup and 0440, or keep root", users, mode)
}

// TestOpenShellModeFitsTheImageUser ties the openshell volume's mode to the user
// the daemon image runs as. The image's final stage is read the way
// TestHelperRunsAsTheDaemonImageDoes reads it: a USER there makes the pod a
// non-root daemon, which cannot read the root-owned 0400 files (gt-nr548n).
func TestOpenShellModeFitsTheImageUser(t *testing.T) {
	t.Parallel()
	path := filepath.Join(packageDir(t), "..", "..", "daemon", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	pod := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec
	var vol *Volume
	for i := range pod.Volumes {
		if v := &pod.Volumes[i]; v.Name == "daemon-openshell" {
			vol = v
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.DefaultMode == nil {
		t.Fatal("the pod has no daemon-openshell Secret volume with a defaultMode")
	}
	if finding := openShellModeFinding(*vol.Secret.DefaultMode, userInstructions(string(data)), pod.SecurityContext); finding != "" {
		t.Error(finding)
	}
}

// TestOpenShellModeFindingRefusesARootOnlyFileUnderAUser holds each row of the
// mode check: a USER with a file the user cannot read is named, and each way out
// (keeping root, or group read through an fsGroup) passes.
func TestOpenShellModeFindingRefusesARootOnlyFileUnderAUser(t *testing.T) {
	t.Parallel()
	user := []string{"USER agent"}
	gid := int64(1000)
	withFSGroup := &PodSecurityContext{FSGroup: &gid}
	for _, tc := range []struct {
		name  string
		mode  int32
		users []string
		sc    *PodSecurityContext
		want  string
	}{
		{name: "root keeps the 0400 files", mode: 0o400},
		{name: "a USER with 0400", mode: 0o400, users: user, want: "cannot read a root-owned 0400 Secret"},
		{name: "a USER with 0400 and an fsGroup", mode: 0o400, users: user, sc: withFSGroup, want: "cannot read a root-owned 0400 Secret"},
		{name: "a USER with 0440 and no fsGroup", mode: 0o440, users: user, want: "cannot read a root-owned 0440 Secret"},
		{name: "a USER with 0440 and an fsGroup", mode: 0o440, users: user, sc: withFSGroup},
		{name: "a USER with world read", mode: 0o444, users: user},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := openShellModeFinding(tc.mode, tc.users, tc.sc)
			if tc.want == "" && got != "" {
				t.Errorf("openShellModeFinding = %q, want no finding", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("openShellModeFinding = %q, want a finding containing %q", got, tc.want)
			}
		})
	}
}

// TestSecretEntryNamedOpenShellIsRefused holds the volume-name collision: a
// listed Secret file named openshell would take the openshell volume's name, so
// the render refuses it, naming the entry.
func TestSecretEntryNamedOpenShellIsRefused(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.Secrets = append(v.Secrets, manifestdata.SecretFile{Name: "openshell", Consumer: "a test", Mode: "0600"})
	if _, err := Build(v); err == nil || !strings.Contains(err.Error(), `"openshell"`) {
		t.Errorf("Build with a Secret file named openshell = %v, want a refusal naming it", err)
	}
}

// TestPodEnvConstantsMatchConfig holds the names and value the pod sets to
// internal/config's declarations: the entrypoint and DetectSelf read those, so
// a drift here starts the pod in no mode or records no owner id. The import rule
// (imports_test.go) reads only non-test files, so this test imports internal/config
// and compares the exported constants rather than their source text.
func TestPodEnvConstantsMatchConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		configValue string
		ourValue    string
	}{
		{"SupervisorContainer", config.SupervisorContainer, SupervisorContainer},
		{"ContainerModeEnv", config.ContainerModeEnv, ContainerModeEnv},
		{"ContainerNameEnv", config.ContainerNameEnv, ContainerNameEnv},
		{"TakeoverEnv", config.TakeoverEnv, TakeoverEnv},
	} {
		if tc.configValue != tc.ourValue {
			t.Errorf("internal/config %s = %q, but the renderer uses %q", tc.name, tc.configValue, tc.ourValue)
		}
	}
}

// TestPodSetsContainerModeAndAStableOwnerName holds the pod's start: `gt daemon
// container-entrypoint` refuses to start anything unless GT_DAEMON_SUPERVISOR
// is "container" (internal/cmd/daemon_container_entrypoint.go,
// runContainerEntrypoint), and daemon.DetectSelf refuses container mode with no
// GT_DAEMON_CONTAINER_NAME, never guessing an owner id (internal/daemon/
// daemon.go, DetectSelf). The name is the Deployment's, so it is the same owner
// after a restart under Recreate with one replica.
//
// The startup owner probe cannot ask an engine in a pod: containerOwnerRunning
// reads templates.ContainerJobState, an unreachable engine is an error,
// known=false, and CheckTakeover refuses on unknown rather than reading it as
// stopped (internal/daemon/startup_guard.go, internal/daemon/takeover.go). The
// pod's own restart passes only because the recorded owner equals Self, which
// is {container, GT_DAEMON_CONTAINER_NAME}; a recorded Docker container owner
// stays refused in the pod. podEnvDecisions drops GT_DAEMON_TAKEOVER, so the pod
// has no takeover switch; only the operator's `--takeover` start on the Mac,
// which the compose file passes GT_DAEMON_TAKEOVER through for, proves that
// owner stopped (gt-wgr6ck).
func TestPodSetsContainerModeAndAStableOwnerName(t *testing.T) {
	t.Parallel()

	first := mustBuild(t, goldenValues(t))
	second := mustBuild(t, goldenValues(t))
	podFirst := podEnvMap(first.Deployment.Spec.Template.Spec.Containers[0])

	if got := podFirst[ContainerModeEnv]; got != SupervisorContainer {
		t.Errorf("the pod's %s = %q, want %q; the entrypoint starts nothing without it", ContainerModeEnv, got, SupervisorContainer)
	}
	name, ok := podFirst[ContainerNameEnv]
	if !ok || name == "" {
		t.Fatalf("the pod carries no %s = %q; DetectSelf refuses container mode without it", ContainerNameEnv, name)
	}
	if name != Name || first.Deployment.Metadata.Name != Name {
		t.Errorf("the pod's %s = %q and the Deployment is %q, want both %q: the owner id must survive a restart", ContainerNameEnv, name, first.Deployment.Metadata.Name, Name)
	}
	if got := podEnvMap(second.Deployment.Spec.Template.Spec.Containers[0])[ContainerNameEnv]; got != name {
		t.Errorf("a second render names the owner %q, the first %q: the owner id must be stable", got, name)
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

// ledgerReaderName is the manifest entry of the cloud ledger reader's DSN file.
const ledgerReaderName = "ledger-reader.dsn"

// TestRenderCarriesTheLedgerReaderSecret holds the reader's DSN through the
// Secrets loop (gt-nlf1i6): a Secret for the entry that holds only its
// placeholder, a read-only mount at the path the cloud settings name, and that
// path resolved by the settings to the Postgres source. With the entry not
// listed, neither the Secret nor the mount is rendered.
func TestRenderCarriesTheLedgerReaderSecret(t *testing.T) {
	t.Parallel()
	const dsnPath = manifestdata.LiveConfigDirPath + "/" + ledgerReaderName

	v := goldenValues(t)
	m := mustBuild(t, v)
	secretName := SecretNamePrefix + ledgerReaderName
	var s *Secret
	for i := range m.Secrets {
		if m.Secrets[i].Metadata.Name == secretName {
			s = &m.Secrets[i]
		}
	}
	if s == nil {
		t.Fatalf("the render writes no Secret %s for the ledger reader", secretName)
	}
	if len(s.StringData) != 1 || s.StringData[ledgerReaderName] != "" {
		t.Errorf("Secret %s holds %v, want only the empty placeholder for %s", secretName, s.StringData, ledgerReaderName)
	}

	pod := m.Deployment.Spec.Template.Spec
	vol := volumeNamed(pod.Volumes, secretVolumeName(ledgerReaderName))
	switch {
	case vol == nil || vol.Secret == nil:
		t.Fatal("the pod has no Secret volume for the ledger reader")
	case vol.Secret.SecretName != secretName:
		t.Errorf("the ledger reader volume reads Secret %q, want %q", vol.Secret.SecretName, secretName)
	case vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o600:
		t.Errorf("the ledger reader volume's defaultMode = %v, want 0600 so the reader's mode check passes", vol.Secret.DefaultMode)
	}
	mnt := volumeMountAt(pod.Containers[0].VolumeMounts, dsnPath)
	switch {
	case mnt == nil:
		t.Fatalf("the daemon container mounts nothing at %s", dsnPath)
	case mnt.Name != secretVolumeName(ledgerReaderName) || mnt.SubPath != ledgerReaderName:
		t.Errorf("the mount at %s carries volume %q subPath %q, want the ledger reader's volume and subPath", dsnPath, mnt.Name, mnt.SubPath)
	case !mnt.ReadOnly:
		t.Errorf("the mount at %s is writable, want read-only", dsnPath)
	}

	ts := config.TownSettings{Cloud: &config.CloudConfig{LedgerDSNFile: dsnPath}}
	if got := ts.ResolveCloud(func(string) string { return "" }); !got.Postgres || got.DSNFile != dsnPath {
		t.Errorf("the pod's cloud settings resolve to %+v, want the Postgres source reading %s", got, dsnPath)
	}

	// Unlisted, the entry renders no Secret and no mount.
	without := goldenValues(t)
	without.Secrets = slices.DeleteFunc(without.Secrets, func(f manifestdata.SecretFile) bool { return f.Name == ledgerReaderName })
	m2 := mustBuild(t, without)
	for _, sec := range m2.Secrets {
		if sec.Metadata.Name == secretName {
			t.Errorf("a render without the entry still writes Secret %s", secretName)
		}
	}
	if mnt := volumeMountAt(m2.Deployment.Spec.Template.Spec.Containers[0].VolumeMounts, dsnPath); mnt != nil {
		t.Errorf("a render without the entry still mounts %s", dsnPath)
	}
}

// TestSecretVolumesCarryTheirManifestMode holds every Secret volume the loop
// renders to its entry's manifest mode as the defaultMode. The reader refuses a
// file readable by group or others, so a volume left at the Kubernetes default
// 0644 would fail the cloud step closed (gt-nlf1i6).
func TestSecretVolumesCarryTheirManifestMode(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	pod := mustBuild(t, v).Deployment.Spec.Template.Spec
	for _, file := range v.Secrets {
		vol := volumeNamed(pod.Volumes, secretVolumeName(file.Name))
		if vol == nil || vol.Secret == nil || vol.Secret.DefaultMode == nil {
			t.Errorf("entry %q has no Secret volume with a defaultMode", file.Name)
			continue
		}
		want, err := strconv.ParseInt(file.Mode, 8, 32)
		if err != nil {
			t.Fatalf("entry %q mode %q does not parse as octal: %v", file.Name, file.Mode, err)
		}
		if got := *vol.Secret.DefaultMode; int64(got) != want {
			t.Errorf("entry %q volume defaultMode = %o, want its manifest mode %s", file.Name, got, file.Mode)
		}
	}
}

// TestSecretModeRefusalSeparatesTheEntryFromTheReason holds the wording of a
// Secret entry's mode refusal: the index and name are followed by a colon before
// the reason, as the other Secret refusals are written (gt-4si2b0).
func TestSecretModeRefusalSeparatesTheEntryFromTheReason(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.Secrets[0].Mode = "0644"
	_, err := Build(v)
	want := fmt.Sprintf(`Values.Secrets[0] %q: mode "0644" is not`, v.Secrets[0].Name)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Build refused with %v, want the refusal to read %q", err, want)
	}
}

// TestSecretVolumesFitTheImageUser holds every Secret volume the loop renders to
// the user the daemon image runs as, as TestOpenShellModeFitsTheImageUser holds
// the openshell volume. An entry's manifest mode is root-only, so a USER in the
// image would leave the daemon unable to read it (gt-4si2b0).
func TestSecretVolumesFitTheImageUser(t *testing.T) {
	t.Parallel()
	path := filepath.Join(packageDir(t), "..", "..", "daemon", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	v := goldenValues(t)
	spec := mustBuild(t, v).Deployment.Spec.Template.Spec
	users := userInstructions(string(data))
	for _, file := range v.Secrets {
		vol := volumeNamed(spec.Volumes, secretVolumeName(file.Name))
		if vol == nil || vol.Secret == nil || vol.Secret.DefaultMode == nil {
			t.Errorf("entry %q has no Secret volume with a defaultMode", file.Name)
			continue
		}
		if finding := openShellModeFinding(*vol.Secret.DefaultMode, users, spec.SecurityContext); finding != "" {
			t.Errorf("entry %q: %s", file.Name, finding)
		}
	}
}

// volumeNamed returns the pod volume named name, or nil.
func volumeNamed(volumes []Volume, name string) *Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}
