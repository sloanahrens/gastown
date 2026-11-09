package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mustBuildHelper builds the helper or fails the test with BuildHelper's reason.
func mustBuildHelper(t *testing.T, v Values) Helper {
	t.Helper()
	h, err := BuildHelper(v)
	if err != nil {
		t.Fatalf("BuildHelper: %v", err)
	}
	return h
}

// helperSpec is the decoded spec of the rendered helper pod, so a check reads the
// YAML an apply reads rather than the struct this package built.
func helperSpec(t *testing.T, h Helper) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(document(t, h), &doc); err != nil {
		t.Fatalf("parsing the rendered helper pod: %v", err)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the rendered helper pod carries no spec: %s", document(t, h))
	}
	return spec
}

// TestHelperMountsTheTwoClaimsAndNothingElse holds the helper's mounts: exactly
// the daemon's town-state and landing-work claims, at the daemon's paths, both
// read-write, with no Secret volume and no service account token.
func TestHelperMountsTheTwoClaimsAndNothingElse(t *testing.T) {
	t.Parallel()
	h := mustBuildHelper(t, goldenValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata", "helper"), "pod.yaml", document(t, h))

	pod := h.Spec
	if len(pod.Volumes) != 2 {
		t.Fatalf("the helper declares %d volumes, want the town-state and landing-work claims alone", len(pod.Volumes))
	}
	for _, want := range []struct{ volume, claim, path string }{
		{TownStateClaim, TownStateClaim, TownRootPath},
		{LandingWorkClaim, LandingWorkClaim, LandingWorkPath},
	} {
		if got := volumeClaim(pod.Volumes, want.volume); got != want.claim {
			t.Errorf("the volume %q carries the claim %q, want %q", want.volume, got, want.claim)
		}
		mount := volumeMountAt(pod.Containers[0].VolumeMounts, want.path)
		if mount == nil {
			t.Fatalf("the helper mounts nothing at %s", want.path)
		}
		if mount.Name != want.volume {
			t.Errorf("the mount at %s carries the volume %q, want %q", want.path, mount.Name, want.volume)
		}
		if mount.ReadOnly {
			t.Errorf("the %s mount at %s is read-only; the seed writes it", want.volume, want.path)
		}
	}
	if len(pod.Containers[0].VolumeMounts) != 2 {
		t.Errorf("the helper mounts %d volumes, want the two claims alone", len(pod.Containers[0].VolumeMounts))
	}

	spec := helperSpec(t, h)
	volumes, ok := spec["volumes"].([]any)
	if !ok || len(volumes) != 2 {
		t.Fatalf("the rendered pod declares the volumes %v, want the two claims", spec["volumes"])
	}
	for _, v := range volumes {
		volume, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("the rendered volume %v is not a mapping", v)
		}
		if _, secret := volume["secret"]; secret {
			t.Errorf("the helper's volume %v mounts a Secret", volume)
		}
		if _, claim := volume["persistentVolumeClaim"]; !claim {
			t.Errorf("the helper's volume %v carries no claim", volume)
		}
	}

	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false: the helper calls no API", pod.AutomountServiceAccountToken)
	}
	if got, ok := spec["automountServiceAccountToken"]; !ok || got != false {
		t.Errorf("the rendered automountServiceAccountToken = %v (present %t), want false", got, ok)
	}
	if _, named := spec["serviceAccountName"]; named {
		t.Errorf("the helper names a serviceAccountName %v; it runs as the namespace's default account with no token", spec["serviceAccountName"])
	}
}

// TestHelperRefusesTheLiveClaimsWhileTheContainerRuns holds the single-writer
// rule: a live render without ConfirmContainerStopped is refused naming the
// Docker Desktop container, while a rehearsal and a confirmed live render both
// stand.
func TestHelperRefusesTheLiveClaimsWhileTheContainerRuns(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.ConfirmContainerStopped = false
	_, err := BuildHelper(v)
	if err == nil {
		t.Fatal("BuildHelper without ConfirmContainerStopped = a pod, want the single-writer refusal")
	}
	for _, want := range []string{"Values.ConfirmContainerStopped", "the Docker Desktop container", TownStateClaim} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("BuildHelper refused with %q, want %q named", err, want)
		}
	}

	rehearsal := rehearsalValues(t)
	h, err := BuildHelper(rehearsal)
	if err != nil {
		t.Fatalf("BuildHelper of a rehearsal without ConfirmContainerStopped: %v", err)
	}
	if got := h.Metadata.Namespace; got != rehearsal.Namespace {
		t.Errorf("the rehearsal helper lands in %q, want the rehearsal's own %q", got, rehearsal.Namespace)
	}

	confirmed := goldenValues(t)
	h, err = BuildHelper(confirmed)
	if err != nil {
		t.Fatalf("BuildHelper of a confirmed live render: %v", err)
	}
	if got := h.Metadata.Namespace; got != DefaultNamespace {
		t.Errorf("the confirmed helper lands in %q, want %q", got, DefaultNamespace)
	}
}

// TestHelperCarriesNoEnvAndNoAlias holds the helper bare: no environment, no
// hostAlias and no credential, even when the caller hands it the live compose
// environment and the whole secrets manifest.
func TestHelperCarriesNoEnvAndNoAlias(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.Env = liveEnv()
	v.HostGatewayIP = "192.168.5.2"

	h := mustBuildHelper(t, v)
	if len(h.Spec.HostAliases) != 0 {
		t.Errorf("the helper carries the hostAliases %+v; it reaches no host service", h.Spec.HostAliases)
	}
	for _, c := range h.Spec.Containers {
		if len(c.Env) != 0 {
			t.Errorf("the container %q carries the environment %q, want none", c.Name, c.Env)
		}
	}

	spec := helperSpec(t, h)
	if got, ok := spec["hostAliases"]; ok {
		t.Errorf("the rendered pod carries hostAliases %v, want none", got)
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("the rendered pod carries the containers %v, want one", spec["containers"])
	}
	for _, c := range containers {
		container, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("the rendered container %v is not a mapping", c)
		}
		if got, carried := container["env"]; carried {
			t.Errorf("the rendered container carries env %v, want none", got)
		}
	}
	for _, key := range []string{"envFrom", "secret", "serviceAccount"} {
		if got, carried := spec[key]; carried {
			t.Errorf("the rendered pod carries %s %v, want none", key, got)
		}
	}
}

// TestHelperIsTheSeedHelperPod holds the pod's identity: the name, namespace,
// image, the sleep that replaces the entrypoint, the label that tells it from the
// daemon pod, and the absence of the Deployment's selector.
func TestHelperIsTheSeedHelperPod(t *testing.T) {
	t.Parallel()
	h := mustBuildHelper(t, goldenValues(t))

	if h.Kind != "Pod" || h.APIVersion != "v1" {
		t.Errorf("the helper is %s/%s, want v1/Pod", h.APIVersion, h.Kind)
	}
	if h.Metadata.Name != HelperName {
		t.Errorf("the helper pod is named %q, want %q", h.Metadata.Name, HelperName)
	}
	if h.Metadata.Namespace != DefaultNamespace {
		t.Errorf("the helper lands in %q, want %q", h.Metadata.Namespace, DefaultNamespace)
	}
	if got := h.Metadata.Labels[HelperRoleLabel]; got != HelperRoleValue {
		t.Errorf("the helper's %s label = %q, want %q", HelperRoleLabel, got, HelperRoleValue)
	}
	for name, value := range selectorLabels() {
		if h.Metadata.Labels[name] == value {
			t.Errorf("the helper carries %s=%q, which is the Deployment's selector; the ReplicaSet would take it for a daemon pod", name, value)
		}
	}

	if len(h.Spec.Containers) != 1 {
		t.Fatalf("the helper runs %d containers, want one", len(h.Spec.Containers))
	}
	c := h.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("the helper's image = %q, want the digest %q", c.Image, testImage)
	}
	if len(c.Args) != 0 || len(c.Env) != 0 {
		t.Errorf("the helper's container carries args %q and env %q; it runs sleep and nothing else", c.Args, c.Env)
	}
	if got, want := c.Command, []string{"sleep", HelperSleepBound}; !slices.Equal(got, want) {
		t.Errorf("the helper's command = %q, want %q", got, want)
	}
	if c.Resources != nil {
		t.Errorf("the helper requests %+v; no source gives a sleeping pod a share", *c.Resources)
	}

	named := goldenValues(t)
	named.Namespace = "gt-seed"
	if got := mustBuildHelper(t, named).Metadata.Namespace; got != "gt-seed" {
		t.Errorf("the helper lands in %q for a caller's namespace, want gt-seed", got)
	}
}

// TestHelperRefusesWhatItCannotRender drives each refusal: an image that is a tag
// rather than a digest, a namespace that is not a DNS-1123 label, and a rehearsal
// that would mount a claim the live render owns.
func TestHelperRefusesWhatItCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Values)
		field  string
	}{
		{"an image by tag", func(v *Values) { v.Image = "registry.example.com/gt/gastown-daemon:v1" }, "Values.Image"},
		{"a malformed namespace", func(v *Values) { v.Namespace = "GT" }, "Values.Namespace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := goldenValues(t)
			tc.change(&v)
			_, err := BuildHelper(v)
			if err == nil {
				t.Fatalf("BuildHelper = a pod, want a refusal naming %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("BuildHelper refused with %q, want %s named", err, tc.field)
			}
		})
	}

	for _, tc := range []struct{ name, namespace string }{
		{"no namespace", ""},
		{"the live namespace", DefaultNamespace},
		{"the sweep's namespace", SweepNamespace},
		{"the cache trim's namespace", CacheTrimNamespace},
		{"the Forgejo namespace", "gt-forgejo"},
	} {
		t.Run("a rehearsal in "+tc.name, func(t *testing.T) {
			v := rehearsalValues(t)
			v.Namespace = tc.namespace
			if tc.namespace == "gt-forgejo" {
				v.ForgejoNamespace = tc.namespace
			}
			_, err := BuildHelper(v)
			if err == nil {
				t.Fatalf("BuildHelper of a rehearsal in %q = a pod; it would mount a claim the live render owns", tc.namespace)
			}
			if !strings.Contains(err.Error(), "Values.Namespace") {
				t.Errorf("BuildHelper refused with %q, want Values.Namespace named", err)
			}
		})
	}

	// The helper needs none of the Deployment's shares or credentials: what the
	// daemon cannot be rendered without, a sleeping pod does not ask for.
	bare := Values{Image: testImage, ConfirmContainerStopped: true}
	if _, err := BuildHelper(bare); err != nil {
		t.Errorf("BuildHelper with no CPU, memory, env or Secrets: %v, want a pod", err)
	}
}

// TestHelperRunsAsTheDaemonImageDoes holds the helper's pinned identity to the
// daemon's: the image's final stage has no USER instruction, so the daemon pod
// runs as root, and the helper pins root so the files the seed writes belong to
// the user the daemon rewrites them as. A USER in the image would make the two
// disagree, and this test fails first.
func TestHelperRunsAsTheDaemonImageDoes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(packageDir(t), "..", "..", "daemon", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if users := userInstructions(string(data)); len(users) != 0 {
		t.Fatalf("%s's final stage carries %q; the helper pins root, so the daemon and the seed would write as different users", path, users)
	}

	daemonPod := mustBuild(t, goldenValues(t)).Deployment.Spec.Template.Spec
	if daemonPod.SecurityContext != nil {
		t.Errorf("the daemon pod carries the securityContext %+v; this check assumes it inherits the image's user", daemonPod.SecurityContext)
	}

	h := mustBuildHelper(t, goldenValues(t))
	sc := h.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != helperUserID || sc.RunAsGroup == nil || *sc.RunAsGroup != helperUserID {
		t.Fatalf("the helper's pod securityContext = %+v, want runAsUser and runAsGroup %d, the user the image runs as", sc, helperUserID)
	}
}

// userInstructions returns the USER instructions of a Dockerfile's final stage,
// the stage the container runs. A line's first word decides the instruction, and
// a comment or a blank line is none.
func userInstructions(body string) []string {
	var users []string
	inFinal := false
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			users = nil
			inFinal = true
		case "USER":
			if inFinal {
				users = append(users, line)
			}
		}
	}
	return users
}
