package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// pushTrustCall drives one request path through its production method.
type pushTrustCall struct {
	name string
	call func(t *testing.T, g GatewayPusher, root string) error
}

// pushTrustCalls is Push and each read path.
var pushTrustCalls = []pushTrustCall{
	{"push", func(t *testing.T, g GatewayPusher, root string) error {
		_, err := g.Push(context.Background(), root)
		return err
	}},
	{"probe", func(t *testing.T, g GatewayPusher, _ string) error { return g.Probe(context.Background()) }},
	{"reported hash", func(t *testing.T, g GatewayPusher, _ string) error {
		_, err := g.ReportedHash(context.Background())
		return err
	}},
	{"calls read", func(t *testing.T, g GatewayPusher, _ string) error {
		_, err := g.Calls(context.Background(), time.Time{}, 1)
		return err
	}},
}

// pushTrustDoer answers as the gateway does and records each request, so a
// refusal row proves no request was sent.
func pushTrustDoer(t *testing.T, seen *[]*http.Request) HTTPDoer {
	return doerFunc(func(req *http.Request) (*http.Response, error) {
		*seen = append(*seen, req)
		if req.Method == http.MethodPut {
			var sent WorkspaceDocument
			if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
				t.Fatalf("the request body is not a workspace document: %v", err)
			}
			return jsonResponse(http.StatusOK, `{"manifest_sha":"`+sent.ManifestSHA+`"}`), nil
		}
		if req.URL.Path == callsPath {
			return jsonResponse(http.StatusOK, `{"calls":[],"dropped":0}`), nil
		}
		resp := jsonResponse(http.StatusOK, "")
		resp.Header.Set(WorkspaceHashHeader, "manifest-1")
		return resp, nil
	})
}

// TestPushTrustRoutes: Push may use the push Service over plain http; each read
// goes to the gateway's name and never to the push name over plain http.
func TestPushTrustRoutes(t *testing.T) {
	t.Parallel()
	const gatewayURL = "http://bd-gateway.gt.svc:7440"
	const pushURL = "http://bd-gateway-push.gt.svc:7441"
	cases := []struct {
		name, url, push            string
		readsRefused, noURL        bool
		wantPushHost, wantReadHost string
	}{
		{"both fields set", gatewayURL, pushURL, false, false, "bd-gateway-push.gt.svc", "bd-gateway.gt.svc"},
		{"no gateway URL refuses the reads the push name", "", pushURL, true, false, "bd-gateway-push.gt.svc", ""},
		{"https push name is admitted on every path", "https://bd-gateway.gt.svc:7440", "https://bd-gateway-push.gt.svc:7441", false, false, "bd-gateway-push.gt.svc", "bd-gateway.gt.svc"},
		{"no URL at all", "", "", false, true, "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, call := range pushTrustCalls {
				call := call
				t.Run(call.name, func(t *testing.T) {
					t.Parallel()
					var seen []*http.Request
					pusher := GatewayPusher{URL: tc.url, PushURL: tc.push, TokenFile: clientToken(t, "tok"), HTTP: pushTrustDoer(t, &seen)}
					err := call.call(t, pusher, clientTown(t))
					if tc.noURL {
						if !errors.Is(err, ErrNoGatewayURL) || len(seen) != 0 {
							t.Fatalf("%s error = %v with %d requests, want %v and none", call.name, err, len(seen), ErrNoGatewayURL)
						}
						if namesPush := strings.Contains(err.Error(), WorkspacePushURLEnv); namesPush == (call.name != "push") {
							t.Errorf("%s error = %q, want the push env named for Push only", call.name, err)
						}
						return
					}
					if call.name == "push" {
						if err != nil || len(seen) != 1 || seen[0].URL.Hostname() != tc.wantPushHost {
							t.Fatalf("Push error = %v with %d requests, want one to %s", err, len(seen), tc.wantPushHost)
						}
						return
					}
					if tc.readsRefused {
						if err == nil || !strings.Contains(err.Error(), "plain http") || strings.Contains(err.Error(), "push Service") || len(seen) != 0 {
							t.Fatalf("%s error = %v with %d requests, want a plain-http refusal offering no push Service and no request", call.name, err, len(seen))
						}
						return
					}
					if err != nil || len(seen) != 1 || seen[0].URL.Hostname() != tc.wantReadHost {
						t.Fatalf("%s error = %v with %d requests, want one to %s", call.name, err, len(seen), tc.wantReadHost)
					}
				})
			}
		})
	}
}

// TestPushTrustHostShapes: pushGatewayHost admits the two cluster DNS shapes
// only, and localGatewayHost still refuses the push name.
func TestPushTrustHostShapes(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"bd-gateway-push.gt.svc", "BD-Gateway-Push.GT.SVC", "bd-gateway-push.gt.svc.cluster.local"} {
		if !pushGatewayHost(host) {
			t.Errorf("pushGatewayHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{
		"bd-gateway-push.openshell.svc", "bd-gateway-push.gt-rehearsal.svc", "bd-gateway-push.openshell.svc.cluster.local",
		"bd-gateway-push.svc", "svc", "bd-gateway.gt.svc", "dolt.gt.svc", "push.gt.svc", "bd-gateway-push.gt.svc.evil.com",
		"bd-gateway-push.gt.svc.cluster", "bd-gateway-push.gt.svc.cluster.local.evil.com", "bd-gateway-push", "",
	} {
		if pushGatewayHost(host) {
			t.Errorf("pushGatewayHost(%q) = true, want false", host)
		}
	}
	for _, host := range []string{"bd-gateway-push.gt.svc", "bd-gateway-push.gt.svc.cluster.local"} {
		if localGatewayHost(host) {
			t.Errorf("localGatewayHost(%q) = true, want the push Service refused", host)
		}
	}
}

// policyNamesPushService reports whether a parsed validation expression names
// the push Service; the parser drops comments, so a comment cannot satisfy it.
func policyNamesPushService(t *testing.T, data []byte) bool {
	var doc struct {
		Spec struct {
			Validations []struct {
				Expression string `yaml:"expression"`
			} `yaml:"validations"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing the policy: %v", err)
	}
	for _, v := range doc.Spec.Validations {
		if strings.Contains(v.Expression, pushGatewayServiceName) {
			return true
		}
	}
	return false
}

// TestPushTrustPolicyParses: an evaluated expression bounds the push Service.
func TestPushTrustPolicyParses(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../deploy/k8s/bd-gateway-service-admission.yaml")
	if err != nil {
		t.Fatalf("reading the admission policy: %v", err)
	}
	if !policyNamesPushService(t, data) {
		t.Error("no policy expression names the push Service")
	}
}

// TestPushTrustPolicyCommentOnly: a comment-only mention of the push Service is refused.
func TestPushTrustPolicyCommentOnly(t *testing.T) {
	t.Parallel()
	fixture := []byte("apiVersion: admissionregistration.k8s.io/v1\n" +
		"kind: ValidatingAdmissionPolicy\n" +
		"spec:\n  validations:\n" +
		"    # bd-gateway-push is mentioned here and nowhere the policy evaluates.\n" +
		"    - expression: object.metadata.name != 'other'\n      message: fixture\n")
	if policyNamesPushService(t, fixture) {
		t.Error("a policy that names the push Service only in a comment was accepted")
	}
}

// TestPushTrustSourceCallSite: pushGatewayHost is reached only from endpoint,
// so no other function can admit the push Service.
func TestPushTrustSourceCallSite(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "workspace_client.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing workspace_client.go: %v", err)
	}
	calls := func(fn *ast.FuncDecl, name string) bool {
		got := false
		ast.Inspect(fn, func(node ast.Node) bool {
			if c, ok := node.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
					got = true
				}
			}
			return true
		})
		return got
	}
	var callers []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !calls(fn, "pushGatewayHost") {
			continue
		}
		callers = append(callers, fn.Name.Name)
		if fn.Name.Name == "localGatewayHost" || fn.Name.Name == "gatewayServiceHost" {
			t.Errorf("%s calls pushGatewayHost, want no host predicate to admit the push name", fn.Name.Name)
		}
	}
	if len(callers) != 1 || callers[0] != "endpoint" {
		t.Fatalf("pushGatewayHost is called from %v, want only endpoint", callers)
	}
}
