package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// driftDigest is an image reference the fixture's rollouts move.
func driftDigest(name string) string {
	return "registry.example.test/" + name + "@sha256:" + strings.Repeat("0", 64)
}

// driftFingerprintOf computes the fingerprint a drift line must carry for value,
// on its own rather than through the code under test: the first 12 hex digits of
// the SHA-256 of encoding/json's encoding.
func driftFingerprintOf(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %#v for its fingerprint: %v", value, err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// driftPrints is the marker of a drift line that prints its values: none.
const driftPrints = ""

// driftWantSide is one side's text in a drift line: its compact JSON when the
// line's marker is driftPrints, else its fingerprint. The table rows decide the
// marker; this only writes the text it names.
func driftWantSide(t *testing.T, marker string, value any) string {
	t.Helper()
	if marker != driftPrints {
		return driftFingerprintOf(t, value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %#v for its line: %v", value, err)
	}
	if len(raw) > driftReadableLimit {
		t.Fatalf("encoding %s is past the %d-byte limit; the expectation needs a cut", raw, driftReadableLimit)
	}
	return string(raw)
}

// driftWantLine is the line the compare must report for a field the two sides
// carry differently, under the marker its row states.
func driftWantLine(t *testing.T, path, marker string, rendered, live any) string {
	t.Helper()
	return path + ": rendered " + driftWantSide(t, marker, rendered) + ", observed " + driftWantSide(t, marker, live) + marker
}

// driftWantAbsentLine is the line for a field or list entry the live object lacks,
// under the marker its row states.
func driftWantAbsentLine(t *testing.T, path, marker string, rendered any) string {
	t.Helper()
	return path + ": rendered " + driftWantSide(t, marker, rendered) + ", observed absent" + marker
}

// driftMirrorMarker is the marker a line for path carries, worked out the way the
// compare works it out. Drift tests in other files still call the mirror below,
// so it stays; the table rows in this file state their marker literally instead.
func driftMirrorMarker(t *testing.T, path string, values ...any) string {
	t.Helper()
	if !driftValueSafe(path) {
		return driftHiddenMarker
	}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encoding %#v for its line: %v", value, err)
		}
		if configTextSecretWord.MatchString(string(raw)) {
			return driftSecretMarker
		}
	}
	return driftPrints
}

// driftWantDifference is driftWantLine under the marker the compare gives path.
// The drift tests in other files call it (gt-2h7fjc).
func driftWantDifference(t *testing.T, path string, rendered, live any) string {
	t.Helper()
	return driftWantLine(t, path, driftMirrorMarker(t, path, rendered, live), rendered, live)
}

// driftWantAbsent is driftWantAbsentLine under the marker the compare gives path.
func driftWantAbsent(t *testing.T, path string, rendered any) string {
	t.Helper()
	return driftWantAbsentLine(t, path, driftMirrorMarker(t, path, rendered), rendered)
}

// driftLineShape is a drift line that hides its values: a JSON path, a
// fingerprint of the rendered side, a fingerprint or absent observed side, and
// the marker that says why, never a value (gt-wi4885, gt-2h7fjc).
var driftLineShape = regexp.MustCompile(`^.+: rendered sha256:[0-9a-f]{12}, observed (sha256:[0-9a-f]{12}|absent)(` +
	regexp.QuoteMeta(driftHiddenMarker) + `|` + regexp.QuoteMeta(driftSecretMarker) + `)$`)

// driftReadableLineShape is a drift line that prints its values: a JSON path and
// each side, with no marker.
var driftReadableLineShape = regexp.MustCompile(`^.+: rendered .+, observed .+$`)

// driftFingerprintSide matches a side that is a bare fingerprint, which only a
// hidden line may carry: a printed side is JSON, so a string is quoted.
var driftFingerprintSide = regexp.MustCompile(`: rendered sha256:|, observed sha256:`)

// driftLineShapeOK holds line to its shape: a line that carries a hidden marker
// is only fingerprints, and any other line is a path and each side's JSON with
// no fingerprint side.
func driftLineShapeOK(line string) bool {
	if strings.HasSuffix(line, driftHiddenMarker) || strings.HasSuffix(line, driftSecretMarker) {
		return driftLineShape.MatchString(line)
	}
	return driftReadableLineShape.MatchString(line) && !driftFingerprintSide.MatchString(line)
}

// driftRendered is a rendered daemon Deployment decoded from the render's own
// YAML: numbers are ints and the object carries only the fields the render
// names.
func driftRendered() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "daemon",
			"namespace": "gt",
			"labels": map[string]any{
				"app.kubernetes.io/name": "daemon",
			},
			"annotations": map[string]any{"gt/town-owner": "pod"},
		},
		"spec": map[string]any{
			"replicas": 1,
			"strategy": map[string]any{"type": "Recreate"},
			"selector": map[string]any{
				"matchLabels": map[string]any{"app.kubernetes.io/name": "daemon"},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"app.kubernetes.io/name": "daemon"},
					"annotations": map[string]any{
						"gastown.dev/polecat-image": driftDigest("polecat"),
						"gastown.dev/pod-template":  "v1",
					},
				},
				"spec": map[string]any{
					"serviceAccountName": "daemon",
					"hostAliases": []any{map[string]any{
						"ip":        "192.168.5.2",
						"hostnames": []any{"host.docker.internal"},
					}},
					"initContainers": []any{map[string]any{
						"name":    "registry-proxy",
						"image":   driftDigest("proxy"),
						"command": []any{"/bin/sh", "-c", "sleep infinity"},
						"args":    []any{"TCP-LISTEN:5001,fork,reuseaddr,bind=127.0.0.1"},
					}},
					"containers": []any{map[string]any{
						"name":  "daemon",
						"image": driftDigest("daemon"),
						"args":  []any{"--om-token-file", "/run/secrets/om-token"},
						"env": []any{
							map[string]any{"name": "GT_TOWN_ROOT", "value": k8sdaemon.TownRootPath},
							map[string]any{"name": "TZ", "value": "America/Chicago"},
						},
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "2", "memory": "1024Mi"},
							"limits":   map[string]any{"cpu": "2", "memory": "1024Mi"},
						},
						"ports": []any{map[string]any{"containerPort": 8080}},
						"volumeMounts": []any{
							map[string]any{"name": "town-state", "mountPath": k8sdaemon.TownRootPath},
							map[string]any{
								"name":      "daemon-promote-om-key",
								"mountPath": "/run/gt/xdg/gt/promote-om.key",
								"subPath":   "promote-om.key",
								"readOnly":  true,
							},
						},
						"readinessProbe": map[string]any{
							"exec":          map[string]any{"command": []any{"gt", "daemon", "ready"}},
							"periodSeconds": 10,
						},
					}},
					"volumes": []any{map[string]any{
						"name":                  "town-state",
						"persistentVolumeClaim": map[string]any{"claimName": "town-state"},
					}},
				},
			},
		},
	}
}

// driftLive returns the rendered object as the cluster returns it to
// `kubectl get deployment -o json`: numbers are float64, the API has
// canonicalized the memory quantity, and the server has added the fields and
// defaults the render never names.
func driftLive(rendered map[string]any) map[string]any {
	live := driftJSONDecoded(rendered).(map[string]any)

	metadata := live["metadata"].(map[string]any)
	metadata["generation"] = 3
	metadata["resourceVersion"] = "12345"
	metadata["uid"] = "5d0e6a2c-0000-4000-8000-000000000000"
	metadata["creationTimestamp"] = "2026-11-15T10:00:00Z"
	metadata["annotations"] = map[string]any{
		"gt/town-owner":                     "pod",
		"deployment.kubernetes.io/revision": "4",
	}

	templateMeta := driftTemplateMeta(live)
	templateMeta["creationTimestamp"] = nil
	templateMeta["annotations"].(map[string]any)[restartedAtAnnotation] = "2026-11-15T10:30:00Z"

	podSpec := driftPodSpec(live)
	podSpec["dnsPolicy"] = "ClusterFirst"
	podSpec["restartPolicy"] = "Always"
	podSpec["terminationGracePeriodSeconds"] = 30

	spec := live["spec"].(map[string]any)
	spec["progressDeadlineSeconds"] = 600
	spec["revisionHistoryLimit"] = 10

	for _, entry := range driftContainers(live) {
		container := entry.(map[string]any)
		container["terminationMessagePath"] = "/dev/termination-log"
		container["imagePullPolicy"] = "IfNotPresent"
		// The API canonicalizes a quantity: 1024Mi comes back as 1Gi.
		resources := container["resources"].(map[string]any)
		for _, block := range []string{"requests", "limits"} {
			amounts := resources[block].(map[string]any)
			if amounts["memory"] == "1024Mi" {
				amounts["memory"] = "1Gi"
			}
		}
	}
	for _, entry := range driftContainers(live) {
		for _, port := range entry.(map[string]any)["ports"].([]any) {
			port.(map[string]any)["protocol"] = "TCP"
		}
	}

	return live
}

// driftJSONDecoded returns a deep copy of value with every int as a float64,
// the way encoding/json decodes a cluster's JSON.
func driftJSONDecoded(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[key] = driftJSONDecoded(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = driftJSONDecoded(child)
		}
		return out
	case int:
		return float64(v)
	}
	return value
}

func driftPodSpec(object map[string]any) map[string]any {
	return object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func driftTemplateMeta(object map[string]any) map[string]any {
	return object["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
}

func driftContainers(object map[string]any) []any {
	return driftPodSpec(object)["containers"].([]any)
}

// driftPodContainers returns the pod spec's init containers and then its regular
// ones: every container the API server fills its defaults in for.
func driftPodContainers(object map[string]any) []any {
	spec := driftPodSpec(object)
	var entries []any
	if initContainers, ok := spec["initContainers"].([]any); ok {
		entries = append(entries, initContainers...)
	}
	if containers, ok := spec["containers"].([]any); ok {
		entries = append(entries, containers...)
	}
	return entries
}

// driftEntry returns the entry named name in the pod spec's list.
func driftEntry(t *testing.T, object map[string]any, list, name string) map[string]any {
	t.Helper()
	for _, entry := range driftPodSpec(object)[list].([]any) {
		container, ok := entry.(map[string]any)
		if ok && container["name"] == name {
			return container
		}
	}
	t.Fatalf("fixture: no %s named %q", list, name)
	return nil
}

func driftContainer(t *testing.T, object map[string]any, name string) map[string]any {
	t.Helper()
	return driftEntry(t, object, "containers", name)
}

func driftInitContainer(t *testing.T, object map[string]any, name string) map[string]any {
	t.Helper()
	return driftEntry(t, object, "initContainers", name)
}

func driftEnvOf(t *testing.T, container map[string]any, name string) map[string]any {
	t.Helper()
	for _, entry := range container["env"].([]any) {
		env, ok := entry.(map[string]any)
		if ok && env["name"] == name {
			return env
		}
	}
	t.Fatalf("fixture: no env var named %q", name)
	return nil
}

// TestDeploymentDriftReportsEveryFieldTheRenderNames drives the compare over a
// live object that drifted one field at a time, and requires the line to name
// where it drifted and both values.
func TestDeploymentDriftReportsEveryFieldTheRenderNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rendered func(t *testing.T, rendered map[string]any)
		live     func(t *testing.T, live map[string]any)
		want     []string
	}{
		{
			name: "an env var",
			live: func(t *testing.T, live map[string]any) {
				driftEnvOf(t, driftContainer(t, live, "daemon"), "TZ")["value"] = "UTC"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].env[name=TZ].value", driftHiddenMarker, "America/Chicago", "UTC")},
		},
		{
			name: "an argument",
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["args"].([]any)[1] = "/run/secrets/other-token"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].args[1]", driftSecretMarker, "/run/secrets/om-token", "/run/secrets/other-token")},
		},
		{
			name: "a command",
			live: func(t *testing.T, live map[string]any) {
				command := driftInitContainer(t, live, "registry-proxy")["command"].([]any)
				command[2] = "exit 1"
			},
			want: []string{driftWantLine(t, "spec.template.spec.initContainers[name=registry-proxy].command[2]", driftHiddenMarker, "sleep infinity", "exit 1")},
		},
		{
			name: "a positional list the live object made longer",
			live: func(t *testing.T, live map[string]any) {
				initContainer := driftInitContainer(t, live, "registry-proxy")
				initContainer["command"] = append(initContainer["command"].([]any), "--extra")
			},
			want: []string{driftWantLine(t, "spec.template.spec.initContainers[name=registry-proxy].command", driftHiddenMarker,
				[]any{"/bin/sh", "-c", "sleep infinity"}, []any{"/bin/sh", "-c", "sleep infinity", "--extra"})},
		},
		{
			name: "a positional list the live object made shorter",
			live: func(t *testing.T, live map[string]any) {
				initContainer := driftInitContainer(t, live, "registry-proxy")
				initContainer["command"] = initContainer["command"].([]any)[:2]
			},
			want: []string{driftWantLine(t, "spec.template.spec.initContainers[name=registry-proxy].command", driftHiddenMarker,
				[]any{"/bin/sh", "-c", "sleep infinity"}, []any{"/bin/sh", "-c"})},
		},
		{
			name: "a probe",
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["readinessProbe"].(map[string]any)["periodSeconds"] = 15
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].readinessProbe.periodSeconds", driftHiddenMarker, 10, 15)},
		},
		{
			name: "a mount",
			live: func(t *testing.T, live map[string]any) {
				mounts := driftContainer(t, live, "daemon")["volumeMounts"].([]any)
				mounts[1].(map[string]any)["subPath"] = "promote-om.key.old"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].volumeMounts[mountPath=/run/gt/xdg/gt/promote-om.key].subPath", driftHiddenMarker,
				"promote-om.key", "promote-om.key.old")},
		},
		{
			name: "a resource limit",
			live: func(t *testing.T, live map[string]any) {
				resources := driftContainer(t, live, "daemon")["resources"].(map[string]any)
				resources["limits"].(map[string]any)["memory"] = "2Gi"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].resources.limits.memory", driftPrints, "1024Mi", "2Gi")},
		},
		{
			name: "a quantity exponent beyond anything a quantity fits",
			rendered: func(t *testing.T, rendered map[string]any) {
				resources := driftContainer(t, rendered, "daemon")["resources"].(map[string]any)
				resources["requests"].(map[string]any)["cpu"] = "1e999999999999"
			},
			live: func(t *testing.T, live map[string]any) {
				resources := driftContainer(t, live, "daemon")["resources"].(map[string]any)
				resources["requests"].(map[string]any)["cpu"] = "1"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].resources.requests.cpu", driftPrints, "1e999999999999", "1")},
		},
		{
			name: "a volume",
			live: func(t *testing.T, live map[string]any) {
				volumes := driftPodSpec(live)["volumes"].([]any)
				volumes[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "other"
			},
			want: []string{driftWantLine(t, "spec.template.spec.volumes[name=town-state].persistentVolumeClaim.claimName", driftHiddenMarker, "town-state", "other")},
		},
		{
			name: "a host alias",
			live: func(t *testing.T, live map[string]any) {
				alias := driftPodSpec(live)["hostAliases"].([]any)[0].(map[string]any)
				alias["hostnames"].([]any)[0] = "host.docker.internal.old"
			},
			want: []string{driftWantLine(t, "spec.template.spec.hostAliases[ip=192.168.5.2].hostnames[0]", driftHiddenMarker,
				"host.docker.internal", "host.docker.internal.old")},
		},
		{
			name: "the strategy",
			live: func(t *testing.T, live map[string]any) {
				live["spec"].(map[string]any)["strategy"].(map[string]any)["type"] = "RollingUpdate"
			},
			want: []string{driftWantLine(t, "spec.strategy.type", driftPrints, "Recreate", "RollingUpdate")},
		},
		{
			name: "the replicas",
			live: func(t *testing.T, live map[string]any) {
				live["spec"].(map[string]any)["replicas"] = 2
			},
			want: []string{driftWantLine(t, "spec.replicas", driftPrints, 1, 2)},
		},
		{
			name: "a label",
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/name"] = "other"
			},
			want: []string{driftWantLine(t, "metadata.labels.app.kubernetes.io/name", driftHiddenMarker, "daemon", "other")},
		},
		{
			name: "a pod-template annotation",
			live: func(t *testing.T, live map[string]any) {
				driftTemplateMeta(live)["annotations"].(map[string]any)["gastown.dev/pod-template"] = "v2"
			},
			want: []string{driftWantLine(t, "spec.template.metadata.annotations.gastown.dev/pod-template", driftPrints, "v1", "v2")},
		},
		{
			name: "an annotation that is named image",
			rendered: func(t *testing.T, rendered map[string]any) {
				rendered["metadata"].(map[string]any)["annotations"].(map[string]any)["image"] = "daemon"
			},
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["annotations"].(map[string]any)["image"] = "other"
			},
			want: []string{driftWantLine(t, "metadata.annotations.image", driftHiddenMarker, "daemon", "other")},
		},
		{
			name: "a label named like a server-set key",
			rendered: func(t *testing.T, rendered map[string]any) {
				rendered["metadata"].(map[string]any)["labels"].(map[string]any)["uid"] = "a"
			},
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["labels"].(map[string]any)["uid"] = "b"
			},
			want: []string{driftWantLine(t, "metadata.labels.uid", driftHiddenMarker, "a", "b")},
		},
		{
			name: "a quantity exponent past the bound, spelled as digits",
			rendered: func(t *testing.T, rendered map[string]any) {
				driftContainer(t, rendered, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "1e20"
			},
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "100000000000000000000"
			},
			want: []string{driftWantLine(t, "spec.template.spec.containers[name=daemon].resources.requests.cpu", driftPrints,
				"1e20", "100000000000000000000")},
		},
		{
			name: "a port the live object does not carry",
			live: func(t *testing.T, live map[string]any) {
				ports := driftContainer(t, live, "daemon")["ports"].([]any)
				ports[0].(map[string]any)["containerPort"] = 9090
			},
			want: []string{driftWantAbsentLine(t, "spec.template.spec.containers[name=daemon].ports[containerPort=8080]", driftHiddenMarker,
				map[string]any{"containerPort": 8080})},
		},
		{
			name: "an env var the live object lacks",
			live: func(t *testing.T, live map[string]any) {
				container := driftContainer(t, live, "daemon")
				container["env"] = []any{driftEnvOf(t, container, "GT_TOWN_ROOT")}
			},
			want: []string{driftWantAbsentLine(t, "spec.template.spec.containers[name=daemon].env[name=TZ]", driftHiddenMarker,
				map[string]any{"name": "TZ", "value": "America/Chicago"})},
		},
		{
			name: "a field the live object lacks",
			live: func(t *testing.T, live map[string]any) {
				delete(driftPodSpec(live), "serviceAccountName")
			},
			want: []string{driftWantAbsentLine(t, "spec.template.spec.serviceAccountName", driftHiddenMarker, "daemon")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRendered()
			live := driftLive(rendered)
			if tc.rendered != nil {
				tc.rendered(t, rendered)
			}
			if tc.live != nil {
				tc.live(t, live)
			}
			got, err := objectDrift("Deployment", rendered, live)
			if err != nil {
				t.Fatalf("deploymentDrift: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("drift =\n%q\nwant\n%q", got, tc.want)
			}
			for _, line := range got {
				if !driftLineShapeOK(line) {
					t.Errorf("drift line %q is not its path and each side's text, fingerprints only under the hidden marker", line)
				}
			}
		})
	}
}

// TestDeploymentDriftNeverPrintsAValue holds every drift line to naming where the
// two sides differ and never what they hold: a live env var and an argument that
// carry a token are as invisible in the line as any other field (gt-wi4885).
func TestDeploymentDriftNeverPrintsAValue(t *testing.T) {
	t.Parallel()
	const token = "0f3c9a1e5b7d-token-of-the-town"
	rendered := driftRendered()
	live := driftLive(rendered)
	container := driftContainer(t, live, "daemon")
	driftEnvOf(t, container, "TZ")["value"] = token
	container["args"].([]any)[1] = token
	got, err := objectDrift("Deployment", rendered, live)
	if err != nil {
		t.Fatalf("deploymentDrift: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("drift = %q, want the env var and the argument", got)
	}
	for _, line := range got {
		if strings.Contains(line, token) {
			t.Errorf("drift line %q carries the live value", line)
		}
		if strings.Contains(line, "America/Chicago") || strings.Contains(line, "/run/secrets/om-token") {
			t.Errorf("drift line %q carries the rendered value", line)
		}
		if !driftLineShapeOK(line) {
			t.Errorf("drift line %q is not its path and each side's text, fingerprints only under the hidden marker", line)
		}
	}
}

// TestDeploymentDriftIgnoresImageRolls holds the compare to the differences a
// cluster in step still reports: an image roll, a rollout's annotations and the
// fields only the server sets.
func TestDeploymentDriftIgnoresImageRolls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rendered func(t *testing.T, rendered map[string]any)
		live     func(t *testing.T, live map[string]any)
	}{
		{name: "a live object the server filled in and canonicalized"},
		{
			name: "a container image",
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["image"] = driftDigest("daemon-rolled")
			},
		},
		{
			name: "an init container image",
			live: func(t *testing.T, live map[string]any) {
				driftInitContainer(t, live, "registry-proxy")["image"] = driftDigest("proxy-rolled")
			},
		},
		{
			name: "the polecat-image annotation",
			live: func(t *testing.T, live map[string]any) {
				driftTemplateMeta(live)["annotations"].(map[string]any)[k8sdaemon.PolecatImageAnnotation] = driftDigest("polecat-rolled")
			},
		},
		{
			name: "the restartedAt annotation",
			rendered: func(t *testing.T, rendered map[string]any) {
				driftTemplateMeta(rendered)["annotations"].(map[string]any)[restartedAtAnnotation] = "2026-11-15T10:00:00Z"
			},
			live: func(t *testing.T, live map[string]any) {
				driftTemplateMeta(live)["annotations"].(map[string]any)[restartedAtAnnotation] = "2026-11-15T11:00:00Z"
			},
		},
		{
			name: "a server-set field the render names",
			rendered: func(t *testing.T, rendered map[string]any) {
				rendered["metadata"].(map[string]any)["creationTimestamp"] = nil
			},
		},
		{
			name: "a millicore spelling the API canonicalized",
			rendered: func(t *testing.T, rendered map[string]any) {
				driftContainer(t, rendered, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "1000m"
			},
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "1"
			},
		},
		{
			name: "an exponent spelling the API canonicalized",
			rendered: func(t *testing.T, rendered map[string]any) {
				driftContainer(t, rendered, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "1e3"
			},
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "1k"
			},
		},
		{
			name: "a server-set metadata field the render names",
			rendered: func(t *testing.T, rendered map[string]any) {
				rendered["metadata"].(map[string]any)["generation"] = 1
			},
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["generation"] = 3
			},
		},
		{
			name: "an env order the server changed",
			live: func(t *testing.T, live map[string]any) {
				container := driftContainer(t, live, "daemon")
				env := container["env"].([]any)
				container["env"] = []any{env[1], env[0]}
			},
		},
		{
			name: "a container only the live object carries",
			live: func(t *testing.T, live map[string]any) {
				driftPodSpec(live)["containers"] = append([]any{map[string]any{
					"name":  "metrics",
					"image": driftDigest("metrics"),
				}}, driftContainers(live)...)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRendered()
			live := driftLive(rendered)
			if tc.rendered != nil {
				tc.rendered(t, rendered)
			}
			if tc.live != nil {
				tc.live(t, live)
			}
			got, err := objectDrift("Deployment", rendered, live)
			if err != nil {
				t.Fatalf("deploymentDrift: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("drift = %q, want none", got)
			}
		})
	}
}

// TestDeploymentDriftRefusesAnUnreadableSide holds the compare to the refusal:
// an object that decoded to nothing is unreadable, so it is an error and never
// a list that reads as a cluster in step.
func TestDeploymentDriftRefusesAnUnreadableSide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rendered map[string]any
		live     map[string]any
	}{
		{name: "a nil render", rendered: nil, live: driftRendered()},
		{name: "an empty render", rendered: map[string]any{}, live: driftRendered()},
		{name: "a nil object", rendered: driftRendered(), live: nil},
		{name: "an empty object", rendered: driftRendered(), live: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := objectDrift("Deployment", tc.rendered, tc.live)
			if err == nil {
				t.Fatalf("drift = %q with no error, want the unreadable side refused", got)
			}
			if got != nil {
				t.Errorf("got %q beside the error, want no differences", got)
			}
		})
	}
}

// TestDeploymentDriftRefusesAValueItCannotFingerprint holds a side with no
// canonical JSON to a failed run: a line cannot stand in for a value it could not
// fingerprint, since the stand-in would be the value (gt-wi4885).
func TestDeploymentDriftRefusesAValueItCannotFingerprint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rendered func(rendered, live map[string]any)
	}{
		{
			name: "the rendered side",
			rendered: func(rendered, _ map[string]any) {
				driftEnvOf(t, driftContainer(t, rendered, "daemon"), "TZ")["value"] = math.NaN()
			},
		},
		{
			name: "the live side",
			rendered: func(_, live map[string]any) {
				driftEnvOf(t, driftContainer(t, live, "daemon"), "TZ")["value"] = math.NaN()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRendered()
			live := driftLive(rendered)
			tc.rendered(rendered, live)
			got, err := objectDrift("Deployment", rendered, live)
			if err == nil {
				t.Fatalf("drift = %q with no error, want the value refused", got)
			}
			if got != nil {
				t.Errorf("got %q beside the error, want no differences", got)
			}
			if want := "no canonical JSON"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if want := "env[name=TZ].value"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name the path %s", err, want)
			}
		})
	}
}

// TestDriftValueSafe holds the reviewed safe list to what the operator accepted:
// the paths whose values a drift line may print, and none that can hold a secret
// (gt-graamb). The rows are written out rather than derived from driftValueSafe,
// so widening the list, as marking an env path safe would, turns a row red.
func TestDriftValueSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		safe bool
	}{
		{"spec.replicas", true},
		{"spec.strategy.rollingUpdate.maxSurge", true},
		{"spec.template.spec.terminationGracePeriodSeconds", true},
		{"spec.template.spec.containers[name=daemon].resources", true},
		{"spec.template.spec.containers[name=daemon].resources.requests.cpu", true},
		{"spec.template.spec.initContainers[name=dind].resources.limits.nvidia.com/gpu", true},
		{"spec.template.spec.containers[name=daemon].args", true},
		{"spec.template.spec.ephemeralContainers[name=debug].args[1]", true},
		{"spec.template.metadata.labels.app.kubernetes.io/name", true},
		{"spec.template.metadata.annotations.gastown.dev/pod-template", true},
		{"spec.template.spec.containers[name=daemon].env[name=TZ].value", false},
		{"spec.template.spec.volumes[name=town-state].persistentVolumeClaim.claimName", false},
		{"spec.template.spec.containers[name=daemon].env[name=TOKEN].valueFrom.secretKeyRef.key", false},
		{"data.config.yml", false},
		{"spec.template.spec.containers[name=daemon].command[0]", false},
		{"spec.template.spec.containers[name=daemon].args[0].value", false},
		{"spec.template.spec.containers[name=daemon", false},
		{"spec.template.spec.containers[name=x].args[0].env[name=TZ].value", false},
		{"metadata.labels.app", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			if got := driftValueSafe(tc.path); got != tc.safe {
				t.Errorf("driftValueSafe(%q) = %v, want %v", tc.path, got, tc.safe)
			}
		})
	}
}

// TestDriftReadableCutsAValueAtTheLimit holds a printed value to
// driftReadableLimit bytes: a value at the limit prints whole, a longer one is cut
// with an ellipsis, and the cut never splits a rune (gt-b8mgln).
func TestDriftReadableCutsAValueAtTheLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "a value of exactly 200 bytes is printed whole",
			value: strings.Repeat("a", 198),
			want:  `"` + strings.Repeat("a", 198) + `"`,
		},
		{
			name:  "a value one byte over the limit is cut to 197 bytes and an ellipsis",
			value: strings.Repeat("a", 199),
			want:  `"` + strings.Repeat("a", 196) + "...",
		},
		{
			name:  "a map prints as compact JSON with its keys sorted",
			value: map[string]any{"b": 1, "a": "x"},
			want:  `{"a":"x","b":1}`,
		},
		{
			name:  "a cut that would split a two-byte rune stops before the rune",
			value: strings.Repeat("a", 195) + "é" + strings.Repeat("a", 10),
			want:  `"` + strings.Repeat("a", 195) + "...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := driftReadableValue(tc.value)
			if err != nil {
				t.Fatalf("driftReadableValue returned error %v, want %q", err, tc.want)
			}
			if got != tc.want {
				t.Errorf("driftReadableValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDriftReadableRefusesAValueItCannotPrint holds a value with no JSON to an
// error and no text, since an empty string would read as a value that printed
// (gt-b8mgln).
func TestDriftReadableRefusesAValueItCannotPrint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value any
	}{
		{name: "a channel", value: make(chan int)},
		{name: "a func", value: func() {}},
		{name: "a NaN float", value: math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := driftReadableValue(tc.value)
			if err == nil {
				t.Fatalf("driftReadableValue(%s) = %q, want an error", tc.name, got)
			}
			if got != "" {
				t.Errorf("driftReadableValue(%s) returned %q with its error, want no text", tc.name, got)
			}
		})
	}
}

// driftWithDind adds a dind init container that requests 8 CPUs to object's pod
// spec, the container the gate's dind request sits on.
func driftWithDind(object map[string]any) {
	podSpec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	podSpec["initContainers"] = append(podSpec["initContainers"].([]any), map[string]any{
		"name":      "dind",
		"image":     driftDigest("dind"),
		"resources": map[string]any{"requests": map[string]any{"cpu": "8", "memory": "16Gi"}},
	})
}

// TestDriftReadablePrintsReviewedValues holds a path the reviewed list allows to
// printing each side as its compact JSON, and a path off the list to its
// fingerprints and the hidden marker (gt-2h7fjc). The compare walks a request's
// quantities by key, so a changed cpu is named at its own key.
func TestDriftReadablePrintsReviewedValues(t *testing.T) {
	t.Parallel()
	const dindCPU = "spec.template.spec.initContainers[name=dind].resources.requests.cpu"
	const tz = "spec.template.spec.containers[name=daemon].env[name=TZ].value"
	cases := []struct {
		name string
		live func(t *testing.T, live map[string]any)
		want []string
	}{
		{
			name: "a changed dind request prints both quantities",
			live: func(t *testing.T, live map[string]any) {
				driftInitContainer(t, live, "dind")["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "4"
			},
			want: []string{dindCPU + `: rendered "8", observed "4"`},
		},
		{
			name: "an env value off the list keeps its fingerprints and the hidden marker",
			live: func(t *testing.T, live map[string]any) {
				driftEnvOf(t, driftContainer(t, live, "daemon"), "TZ")["value"] = "UTC"
			},
			want: []string{tz + ": rendered " + driftFingerprintOf(t, "America/Chicago") + ", observed " + driftFingerprintOf(t, "UTC") + driftHiddenMarker},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRendered()
			driftWithDind(rendered)
			live := driftLive(rendered)
			tc.live(t, live)
			got, err := objectDrift("Deployment", rendered, live)
			if err != nil {
				t.Fatalf("deploymentDrift: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("drift =\n%q\nwant\n%q", got, tc.want)
			}
			for _, line := range got {
				if !driftLineShapeOK(line) {
					t.Errorf("drift line %q is not its path and each side's text, fingerprints only under the hidden marker", line)
				}
			}
		})
	}
}

// TestDriftReadableRefusesAReviewedSideWithNoJSON holds a value on a reviewed path
// with no JSON to a failed run and no lines: a line cannot stand in for a value it
// could not print (gt-2h7fjc).
func TestDriftReadableRefusesAReviewedSideWithNoJSON(t *testing.T) {
	t.Parallel()
	rendered := driftRendered()
	live := driftLive(rendered)
	rendered["spec"].(map[string]any)["replicas"] = math.NaN()
	got, err := objectDrift("Deployment", rendered, live)
	if err == nil {
		t.Fatalf("drift = %q with no error, want the reviewed value refused", got)
	}
	if got != nil {
		t.Errorf("got %q beside the error, want no differences", got)
	}
	for _, want := range []string{"no JSON to print", "spec.replicas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
}

// TestDriftReadableFingerprintsASecretArg holds an args entry that names a secret
// word, on either side, to its fingerprint line with the hidden marker, and never
// to a value the line could print. An argument with no secret word still prints
// its values (gt-2h7fjc).
func TestDriftReadableFingerprintsASecretArg(t *testing.T) {
	t.Parallel()
	const arg = "spec.template.spec.containers[name=daemon].args[1]"
	cases := []struct {
		name     string
		rendered string
		observed string
		want     string
		leaks    []string
	}{
		{
			name:     "an observed argument names a token",
			rendered: "--max-workers=2",
			observed: "--token=abc",
			want:     driftWantLine(t, arg, driftSecretMarker, "--max-workers=2", "--token=abc"),
			leaks:    []string{"--token=abc", "--max-workers=2"},
		},
		{
			name:     "a rendered argument names a password",
			rendered: "--db-password=s3cr3t",
			observed: "--db-password=hunter2",
			want:     driftWantLine(t, arg, driftSecretMarker, "--db-password=s3cr3t", "--db-password=hunter2"),
			leaks:    []string{"s3cr3t", "hunter2"},
		},
		{
			name:     "an argument with no secret word prints its values",
			rendered: "--max-jobs=2",
			observed: "--max-jobs=4",
			want:     arg + `: rendered "--max-jobs=2", observed "--max-jobs=4"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRendered()
			driftContainer(t, rendered, "daemon")["args"].([]any)[1] = tc.rendered
			live := driftLive(rendered)
			driftContainer(t, live, "daemon")["args"].([]any)[1] = tc.observed
			got, err := objectDrift("Deployment", rendered, live)
			if err != nil {
				t.Fatalf("deploymentDrift: %v", err)
			}
			if !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("drift =\n%q\nwant\n%q", got, tc.want)
			}
			for _, line := range got {
				if !driftLineShapeOK(line) {
					t.Errorf("drift line %q is not its path and each side's text, fingerprints only under the hidden marker", line)
				}
				for _, leak := range tc.leaks {
					if strings.Contains(line, leak) {
						t.Errorf("drift line %q carries the argument %q", line, leak)
					}
				}
			}
		})
	}
}

// TestDriftMarkerLineShape holds the shape check to the marker rule: a line whose
// sides are fingerprints carries one of the two hidden markers, and a line with no
// marker prints its values (gt-2h7fjc). Each row states its shape literally.
func TestDriftMarkerLineShape(t *testing.T) {
	t.Parallel()
	const fingerprint = "sha256:0123456789ab"
	cases := []struct {
		name string
		line string
		ok   bool
	}{
		{name: "fingerprints under the path marker", line: "a.b: rendered " + fingerprint + ", observed " + fingerprint + driftHiddenMarker, ok: true},
		{name: "fingerprints under the secret marker", line: "a.b: rendered " + fingerprint + ", observed " + fingerprint + driftSecretMarker, ok: true},
		{name: "an absent side under a marker", line: "a.b: rendered " + fingerprint + ", observed absent" + driftSecretMarker, ok: true},
		{name: "fingerprints with no marker", line: "a.b: rendered " + fingerprint + ", observed " + fingerprint, ok: false},
		{name: "a fingerprint and an absent side with no marker", line: "a.b: rendered " + fingerprint + ", observed absent", ok: false},
		{name: "values with no marker", line: `a.b: rendered "8", observed "4"`, ok: true},
		{name: "values under a marker", line: `a.b: rendered "8", observed "4"` + driftHiddenMarker, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := driftLineShapeOK(tc.line); got != tc.ok {
				t.Errorf("driftLineShapeOK(%q) = %t, want %t", tc.line, got, tc.ok)
			}
		})
	}
}

// TestClusterDriftObjectKinds holds the compare to every kind it walks, not only
// a Deployment: a Role's changed rule field and a ValidatingAdmissionPolicy's
// changed validation are reported as their path and a fingerprint of each side,
// never a value, and the refusal names the kind it could not read (gt-5gtb1x).
func TestClusterDriftObjectKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		kind          string
		objectName    string
		namespace     string
		rendered      map[string]any
		mutate        func(t *testing.T, live map[string]any)
		path          string
		renderedValue any
		liveValue     any
	}{
		{
			name:       "a Role",
			kind:       roleKind,
			objectName: updateRoleName,
			namespace:  updateRBACNamespace,
			rendered: map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       roleKind,
				"metadata":   map[string]any{"name": updateRoleName, "namespace": updateRBACNamespace},
				"rules": []any{map[string]any{
					"apiGroups":     []any{""},
					"resourceNames": []any{"bd-gateway"},
					"resources":     []any{"pods"},
					"verbs":         []any{"get", "patch"},
				}},
			},
			mutate: func(t *testing.T, live map[string]any) {
				rule := live["rules"].([]any)[0].(map[string]any)
				rule["verbs"].([]any)[1] = "watch"
			},
			// "patch", not "list": the hidden marker's words name the reviewed
			// list, so the leak check below would find "list" in the line.
			path:          "rules[0].verbs[1]",
			renderedValue: "patch",
			liveValue:     "watch",
		},
		{
			name:       "a ValidatingAdmissionPolicy",
			kind:       validatingPolicyKind,
			objectName: updatePolicyName,
			rendered: map[string]any{
				"apiVersion": "admissionregistration.k8s.io/v1",
				"kind":       validatingPolicyKind,
				"metadata":   map[string]any{"name": updatePolicyName},
				"spec": map[string]any{
					"failurePolicy": "Fail",
					"validations": []any{map[string]any{
						"expression": "request.operation == 'UPDATE'",
						"message":    "updates go through the Job",
					}},
				},
			},
			mutate: func(t *testing.T, live map[string]any) {
				validation := live["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)
				validation["expression"] = "request.operation == 'DELETE'"
			},
			path:          "spec.validations[0].expression",
			renderedValue: "request.operation == 'UPDATE'",
			liveValue:     "request.operation == 'DELETE'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The leak check below searches the printed line, marker included.
			// A fixture value inside a marker's words would match the marker
			// rather than a leak, so the fixture is refused first (gt-2h7fjc).
			for _, marker := range []string{driftHiddenMarker, driftSecretMarker} {
				for _, value := range []any{tc.renderedValue, tc.liveValue} {
					if text := fmt.Sprint(value); strings.Contains(marker, text) {
						t.Errorf("fixture value %q is a substring of marker %q, so the leak check cannot search for it", text, marker)
					}
				}
			}
			live := driftJSONDecoded(tc.rendered).(map[string]any)
			tc.mutate(t, live)
			got, err := objectDrift(tc.kind, tc.rendered, live)
			if err != nil {
				t.Fatalf("objectDrift(%s): %v", tc.kind, err)
			}
			want := driftWantLine(t, tc.path, driftHiddenMarker, tc.renderedValue, tc.liveValue)
			if !slices.Equal(got, []string{want}) {
				t.Fatalf("drift = %q, want %q", got, want)
			}
			for _, value := range []any{tc.renderedValue, tc.liveValue} {
				if text := fmt.Sprint(value); strings.Contains(got[0], text) {
					t.Errorf("drift line %q carries the value %q", got[0], text)
				}
			}
			// The command prints a line under the "<kind>/<name>" its target
			// names, so the report says which kind differed although the line
			// itself holds only the path and the fingerprints.
			var target driftTarget
			if tc.namespace == "" {
				target = clusterScopedDriftTarget(tc.kind, tc.objectName, tc.rendered)
			} else {
				target = namespacedDriftTarget(tc.kind, tc.objectName, tc.namespace, tc.rendered)
			}
			if prefix := strings.ToLower(tc.kind) + "/" + tc.objectName + ": " + tc.path + ": rendered "; !strings.HasPrefix(target.resource+": "+got[0], prefix) {
				t.Errorf("report %q does not name the kind, name and path with %q", target.resource+": "+got[0], prefix)
			}
			// The refusal names the kind it could not read.
			if _, err := objectDrift(tc.kind, nil, live); err == nil {
				t.Errorf("objectDrift(%s, nil, live) = no error, want the unreadable render refused", tc.kind)
			} else if prefix := strings.ToLower(tc.kind) + " drift:"; !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("error %q does not open with %q", err.Error(), prefix)
			}
		})
	}
}

// driftRunnerConfigText is the gate runner's config.yml the compare reads: the
// label its jobs run on and the container options it passes.
func driftRunnerConfigText(label, options string) string {
	return "log:\n  level: info\nrunner:\n  labels:\n    - \"" + label + "\"\ncontainer:\n  options: " + options + "\n"
}

// driftRunnerConfigMap is a ConfigMap holding value under key, as the compare reads it.
func driftRunnerConfigMap(name, key string, value any) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": "forgejo"},
		"data":       map[string]any{key: value},
	}
}

// TestDriftConfigTextPrintsChangedLinesAsPairs holds each changed line to a '-'
// observed and a '+' rendered pair, in file order, with no unchanged line (gt-svm7ds).
func TestDriftConfigTextPrintsChangedLinesAsPairs(t *testing.T) {
	t.Parallel()
	rendered := driftRunnerConfigMap(runnerConfigMapName, "config.yml", driftRunnerConfigText("ci-base:docker://registry.example.test/ci-base:latest", "--cpus 4"))
	live := driftRunnerConfigMap(runnerConfigMapName, "config.yml", driftRunnerConfigText("ci-base:docker://registry.example.test/ci-base:old", "--cpus 8"))
	got, err := objectDrift("ConfigMap", rendered, live)
	if err != nil {
		t.Fatalf("objectDrift: %v", err)
	}
	want := []string{
		"data.config.yml:",
		`-     - "ci-base:docker://registry.example.test/ci-base:old"`,
		`+     - "ci-base:docker://registry.example.test/ci-base:latest"`,
		"-   options: --cpus 8",
		"+   options: --cpus 4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("drift =\n%q\nwant\n%q", got, want)
	}
}

// TestDriftConfigTextPrintsNothingWhenUnchanged holds an unchanged config to no line (gt-svm7ds).
func TestDriftConfigTextPrintsNothingWhenUnchanged(t *testing.T) {
	t.Parallel()
	text := driftRunnerConfigText("ci-base:docker://registry.example.test/ci-base:latest", "--cpus 4")
	rendered := driftRunnerConfigMap(runnerConfigMapName, "config.yml", text)
	live := driftRunnerConfigMap(runnerConfigMapName, "config.yml", text)
	got, err := objectDrift("ConfigMap", rendered, live)
	if err != nil {
		t.Fatalf("objectDrift: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("drift = %q, want none for an unchanged config", got)
	}
}

// TestDriftConfigTextFingerprintsATokenLine fingerprints a changed token line on
// each side and still prints its neighbour (gt-wi4885, gt-svm7ds).
func TestDriftConfigTextFingerprintsATokenLine(t *testing.T) {
	t.Parallel()
	const renderedToken = `  FORGEJO_TOKEN: "rendered-token-value"`
	const observedToken = `  FORGEJO_TOKEN: "observed-token-value"`
	rendered := driftRunnerConfigMap(runnerConfigMapName, "config.yml", "runner:\n  labels:\n    - \"ci-base:latest\"\nenvs:\n"+renderedToken+"\n")
	live := driftRunnerConfigMap(runnerConfigMapName, "config.yml", "runner:\n  labels:\n    - \"ci-base:old\"\nenvs:\n"+observedToken+"\n")
	got, err := objectDrift("ConfigMap", rendered, live)
	if err != nil {
		t.Fatalf("objectDrift: %v", err)
	}
	want := []string{
		"data.config.yml:",
		`-     - "ci-base:old"`,
		`+     - "ci-base:latest"`,
		"- " + driftFingerprintOf(t, observedToken),
		"+ " + driftFingerprintOf(t, renderedToken),
	}
	if !slices.Equal(got, want) {
		t.Errorf("drift =\n%q\nwant\n%q", got, want)
	}
}

// TestDriftConfigTextStopsAtFortyLines holds the printed changes to forty lines
// and counts the rest, so a fully changed config cannot flood a terminal.
func TestDriftConfigTextStopsAtFortyLines(t *testing.T) {
	t.Parallel()
	var renderedText, liveText strings.Builder
	for i := range 45 {
		fmt.Fprintf(&renderedText, "option-%02d: rendered-%02d\n", i, i)
		fmt.Fprintf(&liveText, "option-%02d: observed-%02d\n", i, i)
	}
	rendered := driftRunnerConfigMap(runnerConfigMapName, "config.yml", renderedText.String())
	live := driftRunnerConfigMap(runnerConfigMapName, "config.yml", liveText.String())
	got, err := objectDrift("ConfigMap", rendered, live)
	if err != nil {
		t.Fatalf("objectDrift: %v", err)
	}
	want := []string{"data.config.yml:"}
	for i := range 20 {
		want = append(want,
			fmt.Sprintf("- option-%02d: observed-%02d", i, i),
			fmt.Sprintf("+ option-%02d: rendered-%02d", i, i))
	}
	want = append(want, "... 50 more changed lines not shown")
	if !slices.Equal(got, want) {
		t.Errorf("drift =\n%q\nwant\n%q", got, want)
	}
}

// TestDriftConfigTextKeepsFingerprintsOffTheRunnerConfig holds the text compare to
// the runner's config.yml alone; every other input keeps a fingerprint (gt-svm7ds).
func TestDriftConfigTextKeepsFingerprintsOffTheRunnerConfig(t *testing.T) {
	t.Parallel()
	const renderedText = "runner:\n  labels:\n    - \"ci-base:latest\"\n"
	const liveText = "runner:\n  labels:\n    - \"ci-base:old\"\n"
	cases := []struct {
		name     string
		kind     string
		object   string
		key      string
		rendered any
		live     any
		path     string
	}{
		{name: "another ConfigMap", kind: "ConfigMap", object: "other-config", key: "config.yml", rendered: renderedText, live: liveText, path: "data.config.yml"},
		{name: "another key", kind: "ConfigMap", object: runnerConfigMapName, key: "other.yml", rendered: renderedText, live: liveText, path: "data.other.yml"},
		{name: "another kind", kind: "Secret", object: runnerConfigMapName, key: "config.yml", rendered: renderedText, live: liveText, path: "data.config.yml"},
		{name: "a live value that is not text", kind: "ConfigMap", object: runnerConfigMapName, key: "config.yml", rendered: renderedText, live: map[string]any{"labels": "ci-base:old"}, path: "data.config.yml"},
		{name: "a diff too large to build", kind: "ConfigMap", object: runnerConfigMapName, key: "config.yml", rendered: strings.Repeat("rendered line\n", 1024), live: strings.Repeat("observed line\n", 1024), path: "data.config.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := driftRunnerConfigMap(tc.object, tc.key, tc.rendered)
			live := driftRunnerConfigMap(tc.object, tc.key, tc.live)
			got, err := objectDrift(tc.kind, rendered, live)
			if err != nil {
				t.Fatalf("objectDrift(%s): %v", tc.kind, err)
			}
			want := []string{driftWantLine(t, tc.path, driftHiddenMarker, tc.rendered, tc.live)}
			if !slices.Equal(got, want) {
				t.Errorf("drift = %q, want %q", got, want)
			}
		})
	}
}
