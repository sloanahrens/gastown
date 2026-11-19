package townreboot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The image fixtures are written out here so the check's JSON tags are read
// against the fields kubectl prints, and its header read against the lines curl
// writes (gt-plvm2h).
const (
	polecatDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	polecatRef     = "registry.example.test/gastown/gastown-polecat@" + polecatDigest
	imageRegBase   = "http://registry.internal:5001"
	polecatDevURL  = imageRegBase + "/v2/gastown-polecat/manifests/dev"
	polecatAccept  = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	imageFix       = "gt town cluster render --polecat-image on the digest the registry answers"
	bdGatewayFixTx = "gt bd workspace push through the push Service once it exists " +
		"(bead: bd gateway push Service), else the exec-pod route"
)

// imageAnswer is what the Deployment read and the registry read return.
type imageAnswer struct {
	deployment string
	headers    string
	kubectlErr error
	curlErr    error
}

// imageRunner answers the Deployment read and the registry read, recording each
// call as the command name then its arguments.
func imageRunner(a imageAnswer) (Runner, func() [][]string) {
	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		switch name {
		case "kubectl":
			return []byte(a.deployment), a.kubectlErr
		case "curl":
			return []byte(a.headers), a.curlErr
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}
	return run, func() [][]string { return calls }
}

// daemonJSON is the daemon Deployment as kubectl prints it, carrying annotation
// as its pod template's polecat image annotation; an empty annotation leaves the
// key out, as a Deployment rendered without it does.
func daemonJSON(annotation string) string {
	if annotation == "" {
		return `{"metadata":{"name":"daemon"},"spec":{"template":{"metadata":{"labels":{}}}}}`
	}
	return `{"metadata":{"name":"daemon"},"spec":{"template":{"metadata":{"annotations":{"gastown.dev/polecat-image":"` + annotation + `"}}}}}`
}

// curlHeaders is a HEAD answer whose Docker-Content-Digest header carries
// digest; an empty digest leaves the header out, as a registry that cannot
// answer the manifest does.
func curlHeaders(digest string) string {
	headers := "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.oci.image.manifest.v1+json\r\n"
	if digest != "" {
		headers += "Docker-Content-Digest: " + digest + "\r\n"
	}
	return headers + "\r\n"
}

// imageRow runs the polecat image check over run and returns its row.
func imageRow(run Runner, registryBase string) Row {
	return RunAll(context.Background(), []Check{polecatImageCheck(run, registryBase)})[0]
}

// gatewayRow runs the bd gateway check over run and returns its row.
func gatewayRow(run Runner) Row {
	return RunAll(context.Background(), []Check{bdGatewayCheck(run)})[0]
}

// TestRebootCheckImageDigest pins the polecat image check's findings: OK when
// the annotation digest equals the registry's header, a not-OK row naming both
// digests and the render fix when they differ, and a not-OK row for every read
// that cannot be made or parsed, which is never read as a match (gt-plvm2h).
func TestRebootCheckImageDigest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		answer         imageAnswer
		noRunner       bool
		wantOK         bool
		wantDetail     string
		detailIsPrefix bool
		wantFix        string
	}{
		{
			name:       "the annotation and the registry agree",
			answer:     imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders(polecatDigest)},
			wantOK:     true,
			wantDetail: "the annotation and the registry both name " + polecatDigest,
		},
		{
			name:       "the annotation and the registry differ",
			answer:     imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders(otherDigest)},
			wantDetail: "the annotation names " + polecatDigest + ", the registry answers " + otherDigest,
			wantFix:    imageFix,
		},
		{
			name:       "the registry answers no digest header",
			answer:     imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders("")},
			wantDetail: "the registry answered no Docker-Content-Digest header",
		},
		{
			name:       "the registry answers a malformed digest",
			answer:     imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders("latest")},
			wantDetail: `the registry answered digest "latest", which is not sha256:<64 hex>`,
		},
		{
			name: "the Deployment read fails",
			answer: imageAnswer{
				kubectlErr: errors.New("kubectl could not reach colima-gt-control"),
			},
			wantDetail: "read deployment daemon: kubectl could not reach colima-gt-control",
		},
		{
			name: "the registry read fails",
			answer: imageAnswer{
				deployment: daemonJSON(polecatRef),
				curlErr:    errors.New("curl: (7) failed to connect to registry.internal port 5001"),
			},
			wantDetail: "read the registry digest: curl: (7) failed to connect to registry.internal port 5001",
		},
		{
			name:       "the Deployment carries no annotation",
			answer:     imageAnswer{deployment: daemonJSON(""), headers: curlHeaders(polecatDigest)},
			wantDetail: "deployment daemon has no gastown.dev/polecat-image annotation",
			wantFix:    imageFix,
		},
		{
			name:       "the annotation is not a digest reference",
			answer:     imageAnswer{deployment: daemonJSON("gastown-polecat:dev"), headers: curlHeaders(polecatDigest)},
			wantDetail: `deployment daemon names polecat image "gastown-polecat:dev", which is not repo@sha256:<64 hex>`,
			wantFix:    imageFix,
		},
		{
			name:           "an unparseable Deployment",
			answer:         imageAnswer{deployment: "not json"},
			wantDetail:     "parse deployment daemon: ",
			detailIsPrefix: true,
		},
		{
			name:       "no runner",
			noRunner:   true,
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, _ := imageRunner(tt.answer)
			if tt.noRunner {
				run = nil
			}
			row := imageRow(run, imageRegBase)
			if row.Name != "7 polecat image" {
				t.Errorf("row name = %q, want %q", row.Name, "7 polecat image")
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if tt.detailIsPrefix {
				if !strings.HasPrefix(row.Detail, tt.wantDetail) {
					t.Errorf("Detail = %q, want prefix %q", row.Detail, tt.wantDetail)
				}
			} else if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// TestRebootCheckImageDigestCommands pins that the check only reads: the Runner
// sees the one kubectl get and one curl -sI, whether the digests agree or not,
// and the curl's URL is the registryBase argument with the polecat image's
// manifest path under it (gt-plvm2h).
func TestRebootCheckImageDigestCommands(t *testing.T) {
	t.Parallel()
	getCall := []string{"kubectl", "--context", "colima-gt-control", "-n", "gt", "get", "deployment", "daemon", "-o", "json"}
	curlCall := []string{"curl", "-sI", "-H", "Accept: " + polecatAccept, polecatDevURL}
	tests := []struct {
		name   string
		answer imageAnswer
	}{
		{
			name:   "the digests agree",
			answer: imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders(polecatDigest)},
		},
		{
			name:   "the digests differ",
			answer: imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders(otherDigest)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, calls := imageRunner(tt.answer)
			imageRow(run, imageRegBase)
			if want := [][]string{getCall, curlCall}; !reflect.DeepEqual(calls(), want) {
				t.Fatalf("runner calls = %q, want only %q", calls(), want)
			}
			if got := calls()[1][len(curlCall)-1]; !strings.HasPrefix(got, imageRegBase) {
				t.Errorf("curl URL = %q, want it to start with the registry base %q", got, imageRegBase)
			}
		})
	}
}

// TestRebootCheckImageDigestRegistryBase pins that a registry base the check
// cannot build a manifest URL from is a not-OK row that runs no command, and
// that the row never repeats a base that carries credentials (gt-plvm2h).
func TestRebootCheckImageDigestRegistryBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		base       string
		wantDetail string
	}{
		{name: "no base", base: "", wantDetail: "no registry base configured"},
		{
			name:       "a base with no scheme",
			base:       "registry.internal:5001",
			wantDetail: "the registry base is not an http(s) root: want a URL with a host and no path, query, credentials or fragment",
		},
		{
			name:       "a base with a path",
			base:       "http://registry.internal:5001/gastown",
			wantDetail: "the registry base is not an http(s) root: want a URL with a host and no path, query, credentials or fragment",
		},
		{
			name:       "a base with credentials",
			base:       "http://viewer:secret-token@registry:5001",
			wantDetail: "the registry base is not an http(s) root: want a URL with a host and no path, query, credentials or fragment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, calls := imageRunner(imageAnswer{deployment: daemonJSON(polecatRef), headers: curlHeaders(polecatDigest)})
			row := imageRow(run, tt.base)
			if row.OK {
				t.Errorf("OK = true for base %q, want a refusal", tt.base)
			}
			if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if len(calls()) != 0 {
				t.Errorf("runner calls = %q, want none: a base refused builds no command", calls())
			}
			if strings.Contains(row.Detail, "secret-token") {
				t.Errorf("Detail = %q repeats the credential in the base", row.Detail)
			}
		})
	}
}

// TestRebootCheckImageGateway pins the bd gateway check's findings: OK at one
// ready replica, a not-OK row at none that names the missing Service endpoint
// and the push fix, not deployed for a town with no Deployment, and a not-OK
// row for every read that cannot be made or parsed (gt-plvm2h).
func TestRebootCheckImageGateway(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		out            string
		err            error
		noRunner       bool
		wantOK         bool
		wantDetail     string
		detailIsPrefix bool
		wantFix        string
	}{
		{
			name:       "one ready replica",
			out:        `{"metadata":{"name":"bd-gateway"},"status":{"readyReplicas":1}}`,
			wantOK:     true,
			wantDetail: "readyReplicas 1",
		},
		{
			name:       "no ready replica",
			out:        `{"metadata":{"name":"bd-gateway"},"status":{}}`,
			wantDetail: "no ready replica: a pod that fails readiness has no Service endpoint",
			wantFix:    bdGatewayFixTx,
		},
		{
			name:       "not deployed",
			out:        "",
			wantOK:     true,
			wantDetail: "not deployed",
		},
		{
			name:       "the Deployment read fails",
			err:        errors.New("kubectl could not reach colima-gt-control"),
			wantDetail: "read deployment bd-gateway: kubectl could not reach colima-gt-control",
		},
		{
			name:           "an unparseable Deployment",
			out:            "not json",
			wantDetail:     "parse deployment bd-gateway: ",
			detailIsPrefix: true,
		},
		{
			name:       "no runner",
			noRunner:   true,
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, _ := imageRunner(imageAnswer{deployment: tt.out, kubectlErr: tt.err})
			if tt.noRunner {
				run = nil
			}
			row := gatewayRow(run)
			if row.Name != "8 bd gateway" {
				t.Errorf("row name = %q, want %q", row.Name, "8 bd gateway")
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if tt.detailIsPrefix {
				if !strings.HasPrefix(row.Detail, tt.wantDetail) {
					t.Errorf("Detail = %q, want prefix %q", row.Detail, tt.wantDetail)
				}
			} else if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// TestRebootCheckImageGatewayCommands pins that the gateway check only reads:
// the Runner sees the one kubectl get, with no verb that could start or patch
// the Deployment, whether a replica is ready or not (gt-plvm2h).
func TestRebootCheckImageGatewayCommands(t *testing.T) {
	t.Parallel()
	getCall := []string{"kubectl", "--context", "colima-gt-control", "-n", "gt",
		"get", "deployment", "bd-gateway", "-o", "json", "--ignore-not-found"}
	tests := []struct {
		name string
		out  string
	}{
		{name: "one ready replica", out: `{"status":{"readyReplicas":1}}`},
		{name: "no ready replica", out: `{"status":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, calls := imageRunner(imageAnswer{deployment: tt.out})
			gatewayRow(run)
			if want := [][]string{getCall}; !reflect.DeepEqual(calls(), want) {
				t.Errorf("runner calls = %q, want only %q", calls(), want)
			}
		})
	}
}
