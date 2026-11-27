package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// captureLive decodes a fixture as kubectl -o json answers it.
func captureLive(t *testing.T, body string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		t.Fatalf("the fixture is not a JSON object: %v", err)
	}
	return object
}

// captureJSON returns value's canonical JSON: one comparison covers the whole tree.
func captureJSON(t *testing.T, value any) string {
	t.Helper()
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatalf("encoding %v: %v", value, err)
	}
	return strings.TrimSuffix(body.String(), "\n")
}

// captureStamp is the instant a capture is stamped with, in a zone that is not UTC.
var captureStamp = time.Date(2026, 11, 25, 19, 0, 0, 0, time.FixedZone("CET", 3600))

// TestCaptureObject holds captureObject to the file it builds: the header, the
// fields it strips, the values it redacts and the one value it keeps.
func TestCaptureObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		live string
		want string
	}{
		{name: "the header names the cluster, the instant in UTC and the uid", live: `{"metadata":{"uid":"u-1"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"u-1"},"object":{"metadata":{"uid":"u-1"}}}`},
		{name: "the fingerprint fields are stripped and the rest kept", live: `{"kind":"Deployment","metadata":{"name":"daemon","uid":"d-1","resourceVersion":"42","managedFields":[{"manager":"kubectl"}],"labels":{"app":"daemon"}},"status":{"replicas":1},"spec":{"replicas":3}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-1"},"object":{"kind":"Deployment","metadata":{"labels":{"app":"daemon"},"name":"daemon","uid":"d-1"},"spec":{"replicas":3}}}`},
		{name: "the last-applied annotation is stripped and the rest kept", live: `{"kind":"Secret","metadata":{"uid":"s-1","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"token\":\"c2VjcmV0\"}}","app":"daemon"}},"data":{"token":"c2VjcmV0"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"s-1"},"object":{"data":{"token":"<redacted>"},"kind":"Secret","metadata":{"annotations":{"app":"daemon"},"uid":"s-1"}}}`},
		{name: "a Pod's container and init container env values are redacted", live: `{"kind":"Pod","metadata":{"uid":"p-1"},"spec":{"initContainers":[{"name":"init","env":[{"name":"TOKEN","value":"s3cret"}]}],"containers":[{"name":"daemon","env":[{"name":"GT_LOG","value":"debug"},{"name":"POD","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}}]}]}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"p-1"},"object":{"kind":"Pod","metadata":{"uid":"p-1"},"spec":{"containers":[{"env":[{"name":"GT_LOG","value":"<redacted>"},{"name":"POD","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}}],"name":"daemon"}],"initContainers":[{"env":[{"name":"TOKEN","value":"<redacted>"}],"name":"init"}]}}}`},
		{name: "a workload's pod template container and init container env values are redacted", live: `{"kind":"Deployment","metadata":{"uid":"d-1"},"spec":{"template":{"spec":{"initContainers":[{"name":"init","env":[{"name":"TOKEN","value":"s3cret"}]}],"containers":[{"name":"daemon","env":[{"name":"GT_LOG","value":"debug"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-1"},"object":{"kind":"Deployment","metadata":{"uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"GT_LOG","value":"<redacted>"}],"name":"daemon"}],"initContainers":[{"env":[{"name":"TOKEN","value":"<redacted>"}],"name":"init"}]}}}}}`},
		{name: "a CronJob's pod template env value is redacted under jobTemplate", live: `{"kind":"CronJob","metadata":{"uid":"c-1"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"GT_DSN","value":"s3cret"}]}]}}}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"c-1"},"object":{"kind":"CronJob","metadata":{"uid":"c-1"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"GT_DSN","value":"<redacted>"}]}]}}}}}}}`},
		{name: "a container's args and command are kept", live: `{"kind":"Deployment","metadata":{"uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"name":"daemon","command":["gt","daemon"],"args":["--flag"],"env":[{"name":"TOKEN","value":"s3cret"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-1"},"object":{"kind":"Deployment","metadata":{"uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"args":["--flag"],"command":["gt","daemon"],"env":[{"name":"TOKEN","value":"<redacted>"}],"name":"daemon"}]}}}}}`},
		{name: "a data value is redacted", live: `{"kind":"Secret","metadata":{"uid":"s-1"},"data":{"token":"c2VjcmV0"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"s-1"},"object":{"data":{"token":"<redacted>"},"kind":"Secret","metadata":{"uid":"s-1"}}}`},
		{name: "a stringData value is redacted", live: `{"kind":"Secret","metadata":{"uid":"s-1"},"stringData":{"user":"me"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"s-1"},"object":{"kind":"Secret","metadata":{"uid":"s-1"},"stringData":{"user":"<redacted>"}}}`},
		{name: "a binaryData value is redacted", live: `{"kind":"ConfigMap","metadata":{"name":"other","uid":"c-1"},"binaryData":{"ca.crt":"LS0t"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"c-1"},"object":{"binaryData":{"ca.crt":"<redacted>"},"kind":"ConfigMap","metadata":{"name":"other","uid":"c-1"}}}`},
		{name: "the runner ConfigMap keeps its config.yml", live: `{"kind":"ConfigMap","metadata":{"name":"forgejo-runner-config","uid":"c-1"},"data":{"config.yml":"runner: {}","label":"team"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"c-1"},"object":{"data":{"config.yml":"runner: {}","label":"<redacted>"},"kind":"ConfigMap","metadata":{"name":"forgejo-runner-config","uid":"c-1"}}}`},
		{name: "another ConfigMap's config.yml is redacted", live: `{"kind":"ConfigMap","metadata":{"name":"other","uid":"c-2"},"data":{"config.yml":"runner: {}"}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"c-2"},"object":{"data":{"config.yml":"<redacted>"},"kind":"ConfigMap","metadata":{"name":"other","uid":"c-2"}}}`},
		{name: "a same-named object that is not a ConfigMap keeps nothing", live: `{"kind":"Secret","metadata":{"name":"forgejo-runner-config","uid":"s-1"},"data":{"config.yml":"e30="}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"s-1"},"object":{"data":{"config.yml":"<redacted>"},"kind":"Secret","metadata":{"name":"forgejo-runner-config","uid":"s-1"}}}`},
		{name: "the runner Deployment keeps the four Docker values in its container and init container", live: `{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"d-1"},"spec":{"template":{"spec":{"initContainers":[{"env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"},{"name":"DOCKER_TLS_VERIFY","value":"1"},{"name":"DOCKER_CERT_PATH","value":"/certs/client"},{"name":"DOCKER_TLS_CERTDIR","value":"/certs"}],"name":"dind"}],"containers":[{"env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"},{"name":"DOCKER_TLS_VERIFY","value":"1"},{"name":"DOCKER_CERT_PATH","value":"/certs/client"},{"name":"DOCKER_TLS_CERTDIR","value":"/certs"}],"name":"runner"}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-1"},"object":{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"},{"name":"DOCKER_TLS_VERIFY","value":"1"},{"name":"DOCKER_CERT_PATH","value":"/certs/client"},{"name":"DOCKER_TLS_CERTDIR","value":"/certs"}],"name":"runner"}],"initContainers":[{"env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"},{"name":"DOCKER_TLS_VERIFY","value":"1"},{"name":"DOCKER_CERT_PATH","value":"/certs/client"},{"name":"DOCKER_TLS_CERTDIR","value":"/certs"}],"name":"dind"}]}}}}}`},
		{name: "an unlisted name on the runner Deployment is redacted", live: `{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"name":"runner","env":[{"name":"GT_LOG","value":"debug"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-1"},"object":{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"d-1"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"GT_LOG","value":"<redacted>"}],"name":"runner"}]}}}}}`},
		{name: "a listed name on another kind is redacted", live: `{"kind":"StatefulSet","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"s-1"},"spec":{"template":{"spec":{"containers":[{"name":"runner","env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"s-1"},"object":{"kind":"StatefulSet","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"s-1"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DOCKER_HOST","value":"<redacted>"}],"name":"runner"}]}}}}}`},
		{name: "a listed name on another object name is redacted", live: `{"kind":"Deployment","metadata":{"name":"other","namespace":"forgejo","uid":"d-2"},"spec":{"template":{"spec":{"containers":[{"name":"runner","env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-2"},"object":{"kind":"Deployment","metadata":{"name":"other","namespace":"forgejo","uid":"d-2"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DOCKER_HOST","value":"<redacted>"}],"name":"runner"}]}}}}}`},
		{name: "a listed name in another namespace is redacted", live: `{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"other","uid":"d-3"},"spec":{"template":{"spec":{"containers":[{"name":"runner","env":[{"name":"DOCKER_HOST","value":"tcp://127.0.0.1:2376"}]}]}}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"d-3"},"object":{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"other","uid":"d-3"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DOCKER_HOST","value":"<redacted>"}],"name":"runner"}]}}}}}`},
		{name: "a containers key outside the known paths is left alone", live: `{"metadata":{"uid":"u-1"},"containers":{"name":"daemon","env":[{"name":"TOKEN","value":"s3cret"}]}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"u-1"},"object":{"containers":{"env":[{"name":"TOKEN","value":"s3cret"}],"name":"daemon"},"metadata":{"uid":"u-1"}}}`},
		{name: "a containers key under a path the walk does not visit is left alone", live: `{"metadata":{"uid":"u-1"},"spec":{"another":{"containers":"not a list"}}}`, want: `{"_capture":{"at":"2026-11-25T18:00:00Z","cluster":"gt-sbx","uid":"u-1"},"object":{"metadata":{"uid":"u-1"},"spec":{"another":{"containers":"not a list"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := captureObject(captureLive(t, tc.live), "gt-sbx", captureStamp)
			if err != nil {
				t.Fatalf("captureObject(): %v", err)
			}
			if got := captureJSON(t, got); got != tc.want {
				t.Errorf("capture = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestCaptureObjectLeavesInput holds the capture to a copy: the caller's live
// answer stays as the cluster sent it.
func TestCaptureObjectLeavesInput(t *testing.T) {
	t.Parallel()
	live := `{"kind":"Secret","metadata":{"uid":"s-1","resourceVersion":"42"},"data":{"token":"c2VjcmV0"}}`
	object := captureLive(t, live)
	if _, err := captureObject(object, "gt-sbx", captureStamp); err != nil {
		t.Fatalf("captureObject(): %v", err)
	}
	if got, want := captureJSON(t, object), captureJSON(t, captureLive(t, live)); got != want {
		t.Errorf("the capture changed its input: got %s, want %s", got, want)
	}
}

// TestCaptureObjectRefuse holds every object the capture cannot read to a failed
// call: a file written from one would keep a value or name no object.
func TestCaptureObjectRefuse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		live string
		want string
	}{
		{name: "no metadata", live: `{}`, want: "no metadata"},
		{name: "metadata of another shape", live: `{"metadata":"daemon"}`, want: "no metadata"},
		{name: "no uid", live: `{"metadata":{"name":"daemon"}}`, want: "no uid"},
		{name: "an empty uid", live: `{"metadata":{"uid":""}}`, want: "no uid"},
		{name: "annotations of another shape", live: `{"metadata":{"uid":"u-1","annotations":["a"]}}`, want: "annotations are not an object"},
		{name: "data of another shape", live: `{"metadata":{"uid":"u-1"},"data":["a"]}`, want: "data is not an object"},
		{name: "stringData of another shape", live: `{"metadata":{"uid":"u-1"},"stringData":"a"}`, want: "stringData is not an object"},
		{name: "binaryData of another shape", live: `{"metadata":{"uid":"u-1"},"binaryData":"a"}`, want: "binaryData is not an object"},
		{name: "spec of another shape", live: `{"metadata":{"uid":"u-1"},"spec":"daemon"}`, want: "spec is not an object"},
		{name: "a pod template of another shape", live: `{"metadata":{"uid":"u-1"},"spec":{"template":"daemon"}}`, want: "spec.template is not an object"},
		{name: "a CronJob template of another shape", live: `{"metadata":{"uid":"u-1"},"spec":{"jobTemplate":"daemon"}}`, want: "spec.jobTemplate is not an object"},
		{name: "a Pod's containers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"containers":{}}}`, want: "spec.containers is not a list"},
		{name: "a Pod's initContainers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"initContainers":{}}}`, want: "spec.initContainers is not a list"},
		{name: "a workload's containers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"template":{"spec":{"containers":{}}}}}`, want: "spec.template.spec.containers is not a list"},
		{name: "a workload's initContainers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"template":{"spec":{"initContainers":{}}}}}`, want: "spec.template.spec.initContainers is not a list"},
		{name: "a CronJob's containers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":{}}}}}}}`, want: "spec.jobTemplate.spec.template.spec.containers is not a list"},
		{name: "a CronJob's initContainers that are not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"initContainers":{}}}}}}}`, want: "spec.jobTemplate.spec.template.spec.initContainers is not a list"},
		{name: "a container that is not an object", live: `{"metadata":{"uid":"u-1"},"spec":{"containers":["daemon"]}}`, want: "a container under spec.containers is not an object"},
		{name: "an env that is not a list", live: `{"metadata":{"uid":"u-1"},"spec":{"containers":[{"env":{}}]}}`, want: "an env under spec.containers is not a list"},
		{name: "an env entry that is not an object", live: `{"metadata":{"uid":"u-1"},"spec":{"containers":[{"env":["TOKEN"]}]}}`, want: "an env entry under spec.containers is not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := captureObject(captureLive(t, tc.live), "gt-sbx", captureStamp)
			if err == nil {
				t.Fatalf("captureObject() = %s, want an error naming %q", captureJSON(t, got), tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
