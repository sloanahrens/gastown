package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// captureLiveAnswer answers every kubectl read with out, exit and err.
func captureLiveAnswer(out string, exit int, err error) update.RunFunc {
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		return out, exit, err
	}
}

// TestClusterCaptureLive holds captureFromCluster to the file it builds: the
// resource read, the capture header, the redaction, the indent and the trailing
// newline.
func TestClusterCaptureLive(t *testing.T) {
	t.Parallel()
	// A zone that is not UTC, so the header's UTC form is asserted rather than
	// echoed back.
	at := time.Date(2026, 11, 25, 21, 30, 0, 0, time.FixedZone("CET", 3600))
	for _, tc := range []struct {
		name       string
		kind       string
		objectName string
		namespace  string
		uid        string
		live       string
		wantArgs   string
		wantToken  string
		wantRaw    string
	}{
		{
			name:       "a namespaced Secret is read with its namespace, redacted and stamped",
			kind:       "secret",
			objectName: "runner",
			namespace:  "gt-sbx",
			uid:        "s-1",
			live:       `{"kind":"Secret","metadata":{"name":"runner","uid":"s-1","labels":{"note":"a<b&c"}},"data":{"token":"c2VjcmV0"}}`,
			wantArgs:   "get secret/runner -n gt-sbx --ignore-not-found -o json",
			wantToken:  "<redacted>",
			wantRaw:    "a<b&c",
		},
		{
			name:       "a cluster-scoped object is read without a namespace flag",
			kind:       "namespace",
			objectName: "gt-sbx",
			namespace:  "",
			uid:        "n-1",
			live:       `{"kind":"Namespace","metadata":{"name":"gt-sbx","uid":"n-1"}}`,
			wantArgs:   "get namespace/gt-sbx --ignore-not-found -o json",
		},
	} {
		var args []string
		body, err := captureFromCluster(context.Background(), clusterCaptureDeps{
			kubectl: func(_ context.Context, _ string, _ []string, _ string, got ...string) (string, int, error) {
				args = got
				return tc.live, 0, nil
			},
			now:     func() time.Time { return at },
			cluster: "gt-sbx",
		}, tc.kind, tc.objectName, tc.namespace)
		if err != nil {
			t.Fatalf("%s: captureFromCluster() error = %v, want none", tc.name, err)
		}
		if got := strings.Join(args, " "); got != tc.wantArgs {
			t.Errorf("%s: read with %q, want %q", tc.name, got, tc.wantArgs)
		}
		if !bytes.HasSuffix(body, []byte("}\n")) || bytes.HasSuffix(body, []byte("\n\n")) {
			t.Errorf("%s: capture does not end in one newline: %q", tc.name, body)
		}
		if !bytes.Contains(body, []byte("\n  \"_capture\": {\n    \"at\": ")) {
			t.Errorf("%s: capture is not indented two spaces a level: %q", tc.name, body)
		}
		if tc.wantRaw != "" && !bytes.Contains(body, []byte(tc.wantRaw)) {
			t.Errorf("%s: capture does not hold %q as written, so a value's < and & are escaped: %q", tc.name, tc.wantRaw, body)
		}
		var captured map[string]any
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("%s: the capture is not a JSON object: %v", tc.name, err)
		}
		header, ok := captured["_capture"].(map[string]any)
		if !ok {
			t.Fatalf("%s: the capture holds no _capture header: %q", tc.name, body)
		}
		if got := header["cluster"]; got != "gt-sbx" {
			t.Errorf("%s: header cluster = %v, want gt-sbx", tc.name, got)
		}
		if got := header["at"]; got != "2026-11-25T20:30:00Z" {
			t.Errorf("%s: header at = %v, want 2026-11-25T20:30:00Z, the same instant in UTC", tc.name, got)
		}
		if got := header["uid"]; got != tc.uid {
			t.Errorf("%s: header uid = %v, want %s", tc.name, got, tc.uid)
		}
		if tc.wantToken == "" {
			continue
		}
		object, ok := captured["object"].(map[string]any)
		if !ok {
			t.Fatalf("%s: the capture holds no object: %q", tc.name, body)
		}
		data, ok := object["data"].(map[string]any)
		if !ok {
			t.Fatalf("%s: the captured object holds no data: %q", tc.name, body)
		}
		if got := data["token"]; got != tc.wantToken {
			t.Errorf("%s: data token = %v, want %s", tc.name, got, tc.wantToken)
		}
	}
}

// TestClusterCaptureLiveRefuse drives each refusal: the error is returned, no
// bytes come with it, and the message names what failed.
func TestClusterCaptureLiveRefuse(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return time.Date(2026, 11, 25, 20, 30, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name string
		deps clusterCaptureDeps
		want string
	}{
		{"no kubectl runner", clusterCaptureDeps{now: at}, "kubectl"},
		{"no clock", clusterCaptureDeps{kubectl: driftNoRead(t)}, "clock"},
		{"a read that failed", clusterCaptureDeps{kubectl: captureLiveAnswer("", 0, errors.New("kubectl is not installed")), now: at}, "kubectl is not installed"},
		{"a nonzero exit", clusterCaptureDeps{kubectl: captureLiveAnswer("", 1, nil), now: at}, "exited 1"},
		{"an answer that is not JSON", clusterCaptureDeps{kubectl: captureLiveAnswer("not json", 0, nil), now: at}, "not a JSON object"},
		{"an object the cluster does not hold", clusterCaptureDeps{kubectl: captureLiveAnswer("", 0, nil), now: at}, "does not hold"},
		{"an object with no metadata", clusterCaptureDeps{kubectl: captureLiveAnswer(`{}`, 0, nil), now: at}, "no metadata"},
		{"an object with no uid", clusterCaptureDeps{kubectl: captureLiveAnswer(`{"metadata":{"name":"daemon"}}`, 0, nil), now: at}, "no uid"},
		{"data that is not an object", clusterCaptureDeps{kubectl: captureLiveAnswer(`{"metadata":{"uid":"s-1"},"data":"secret"}`, 0, nil), now: at}, "data is not an object"},
	} {
		body, err := captureFromCluster(context.Background(), tc.deps, "secret", "runner", "gt-sbx")
		if err == nil {
			t.Errorf("%s: captureFromCluster() = %q, want a refusal", tc.name, body)
			continue
		}
		if len(body) != 0 {
			t.Errorf("%s: returned %d bytes beside error %v, want none", tc.name, len(body), err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
	}
}

// TestCaptureEncodeRefuses drives the encode refusal with a value JSON cannot
// hold; captureObject's own output never holds one, so the branch has no other
// path.
func TestCaptureEncodeRefuses(t *testing.T) {
	t.Parallel()
	body, err := captureEncode(map[string]any{"count": math.Inf(1)})
	if err == nil {
		t.Fatalf("captureEncode() = %q, want a refusal for a value JSON cannot hold", body)
	}
	if len(body) != 0 {
		t.Errorf("returned %d bytes beside error %v, want none", len(body), err)
	}
}
