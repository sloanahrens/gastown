package cmd

import (
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// driftDigest is an image reference the fixture's rollouts move.
func driftDigest(name string) string {
	return "registry.example.test/" + name + "@sha256:" + strings.Repeat("0", 64)
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
							map[string]any{"name": "GT_TOWN_ROOT", "value": "/Users/sloan/gt"},
							map[string]any{"name": "TZ", "value": "America/Chicago"},
						},
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "2", "memory": "1024Mi"},
							"limits":   map[string]any{"cpu": "2", "memory": "1024Mi"},
						},
						"ports": []any{map[string]any{"containerPort": 8080}},
						"volumeMounts": []any{
							map[string]any{"name": "town-state", "mountPath": "/Users/sloan/gt"},
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
			want: []string{`spec.template.spec.containers[name=daemon].env[name=TZ].value: rendered "America/Chicago", live "UTC"`},
		},
		{
			name: "an argument",
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["args"].([]any)[1] = "/run/secrets/other-token"
			},
			want: []string{`spec.template.spec.containers[name=daemon].args[1]: rendered "/run/secrets/om-token", live "/run/secrets/other-token"`},
		},
		{
			name: "a command",
			live: func(t *testing.T, live map[string]any) {
				command := driftInitContainer(t, live, "registry-proxy")["command"].([]any)
				command[2] = "exit 1"
			},
			want: []string{`spec.template.spec.initContainers[name=registry-proxy].command[2]: rendered "sleep infinity", live "exit 1"`},
		},
		{
			name: "a probe",
			live: func(t *testing.T, live map[string]any) {
				driftContainer(t, live, "daemon")["readinessProbe"].(map[string]any)["periodSeconds"] = 15
			},
			want: []string{`spec.template.spec.containers[name=daemon].readinessProbe.periodSeconds: rendered 10, live 15`},
		},
		{
			name: "a mount",
			live: func(t *testing.T, live map[string]any) {
				mounts := driftContainer(t, live, "daemon")["volumeMounts"].([]any)
				mounts[1].(map[string]any)["subPath"] = "promote-om.key.old"
			},
			want: []string{`spec.template.spec.containers[name=daemon].volumeMounts[mountPath=/run/gt/xdg/gt/promote-om.key].subPath: rendered "promote-om.key", live "promote-om.key.old"`},
		},
		{
			name: "a resource limit",
			live: func(t *testing.T, live map[string]any) {
				resources := driftContainer(t, live, "daemon")["resources"].(map[string]any)
				resources["limits"].(map[string]any)["memory"] = "2Gi"
			},
			want: []string{`spec.template.spec.containers[name=daemon].resources.limits.memory: rendered "1024Mi", live "2Gi"`},
		},
		{
			name: "a volume",
			live: func(t *testing.T, live map[string]any) {
				volumes := driftPodSpec(live)["volumes"].([]any)
				volumes[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "other"
			},
			want: []string{`spec.template.spec.volumes[name=town-state].persistentVolumeClaim.claimName: rendered "town-state", live "other"`},
		},
		{
			name: "a host alias",
			live: func(t *testing.T, live map[string]any) {
				alias := driftPodSpec(live)["hostAliases"].([]any)[0].(map[string]any)
				alias["hostnames"].([]any)[0] = "host.docker.internal.old"
			},
			want: []string{`spec.template.spec.hostAliases[ip=192.168.5.2].hostnames[0]: rendered "host.docker.internal", live "host.docker.internal.old"`},
		},
		{
			name: "the strategy",
			live: func(t *testing.T, live map[string]any) {
				live["spec"].(map[string]any)["strategy"].(map[string]any)["type"] = "RollingUpdate"
			},
			want: []string{`spec.strategy.type: rendered "Recreate", live "RollingUpdate"`},
		},
		{
			name: "the replicas",
			live: func(t *testing.T, live map[string]any) {
				live["spec"].(map[string]any)["replicas"] = 2
			},
			want: []string{`spec.replicas: rendered 1, live 2`},
		},
		{
			name: "a label",
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/name"] = "other"
			},
			want: []string{`metadata.labels.app.kubernetes.io/name: rendered "daemon", live "other"`},
		},
		{
			name: "a pod-template annotation",
			live: func(t *testing.T, live map[string]any) {
				driftTemplateMeta(live)["annotations"].(map[string]any)["gastown.dev/pod-template"] = "v2"
			},
			want: []string{`spec.template.metadata.annotations.gastown.dev/pod-template: rendered "v1", live "v2"`},
		},
		{
			name: "an annotation that is named image",
			rendered: func(t *testing.T, rendered map[string]any) {
				rendered["metadata"].(map[string]any)["annotations"].(map[string]any)["image"] = "daemon"
			},
			live: func(t *testing.T, live map[string]any) {
				live["metadata"].(map[string]any)["annotations"].(map[string]any)["image"] = "other"
			},
			want: []string{`metadata.annotations.image: rendered "daemon", live "other"`},
		},
		{
			name: "a port the live object does not carry",
			live: func(t *testing.T, live map[string]any) {
				ports := driftContainer(t, live, "daemon")["ports"].([]any)
				ports[0].(map[string]any)["containerPort"] = 9090
			},
			want: []string{`spec.template.spec.containers[name=daemon].ports[containerPort=8080]: rendered {"containerPort":8080}, live has no such entry`},
		},
		{
			name: "an env var the live object lacks",
			live: func(t *testing.T, live map[string]any) {
				container := driftContainer(t, live, "daemon")
				container["env"] = []any{driftEnvOf(t, container, "GT_TOWN_ROOT")}
			},
			want: []string{`spec.template.spec.containers[name=daemon].env[name=TZ]: rendered {"name":"TZ","value":"America/Chicago"}, live has no such entry`},
		},
		{
			name: "a field the live object lacks",
			live: func(t *testing.T, live map[string]any) {
				delete(driftPodSpec(live), "serviceAccountName")
			},
			want: []string{`spec.template.spec.serviceAccountName: rendered "daemon", live has no such field`},
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
			got, err := deploymentDrift(rendered, live)
			if err != nil {
				t.Fatalf("deploymentDrift: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("drift =\n%q\nwant\n%q", got, tc.want)
			}
		})
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
			got, err := deploymentDrift(rendered, live)
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
			got, err := deploymentDrift(tc.rendered, tc.live)
			if err == nil {
				t.Fatalf("drift = %q with no error, want the unreadable side refused", got)
			}
			if got != nil {
				t.Errorf("got %q beside the error, want no differences", got)
			}
		})
	}
}
