package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// bdGatewayConfigFile is a complete bdgateway target in the config file.
const bdGatewayConfigFile = `{"target": "bdgateway", "bdgateway": {"url": "http://gw:7440", "token_file": "/run/secrets/daemon.token", "caller": "daemon.test", "timeout_ms": 60000}}`

func TestRunBuildsTheBdGatewayTargetFromItsConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdGatewayConfigFile)
	if exit := h.run("bd", "show", "gt-x"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.bdBuilt || h.kubeBuilt || h.targetSet {
		t.Fatalf("bdgateway built = %v, kubernetes = %v, docker = %v; want the bdgateway target only", h.bdBuilt, h.kubeBuilt, h.targetSet)
	}
	if h.builtTown != h.town {
		t.Fatalf("built the target for town %q, want %q", h.builtTown, h.town)
	}
	want := bdGatewayConfig{URL: "http://gw:7440", TokenFile: "/run/secrets/daemon.token", Caller: "daemon.test", TimeoutMS: 60000}
	if h.bdCfg != want {
		t.Fatalf("target config = %+v, want %+v", h.bdCfg, want)
	}
	if got, want := h.fake.call.Argv, []string{"bd", "show", "gt-x"}; !slices.Equal(got, want) {
		t.Fatalf("launched %q, want %q", got, want)
	}
}

// TestRunBdGatewayFallsBackToTheEnvironment: the daemon pod configures the URL
// and token file through BD_GATEWAY_URL and BD_GATEWAY_TOKEN_FILE, so a config
// file that names only the target still builds a wired target.
func TestRunBdGatewayFallsBackToTheEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getenv = func(name string) string {
		switch name {
		case "HOME":
			return h.home
		case "GT_TOWN_ROOT":
			return h.town
		case "BD_GATEWAY_URL":
			return "http://host.docker.internal:7440"
		case "BD_GATEWAY_TOKEN_FILE":
			return "/run/gt/secrets/daemon.token"
		}
		return ""
	}
	h.writeConfig(t, `{"target": "bdgateway"}`)
	if exit := h.run("bd", "show", "gt-x"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	want := bdGatewayConfig{URL: "http://host.docker.internal:7440", TokenFile: "/run/gt/secrets/daemon.token"}
	if h.bdCfg != want {
		t.Fatalf("target config = %+v, want %+v", h.bdCfg, want)
	}
}

// TestRunRefusesAnIncompleteBdGatewayConfig: a target the wrapper cannot reach
// refuses by naming the missing key, and builds nothing.
func TestRunRefusesAnIncompleteBdGatewayConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{
			name:   "no url",
			config: `{"target": "bdgateway", "bdgateway": {"token_file": "/t"}}`,
			want:   []string{"no \"url\"", "BD_GATEWAY_URL"},
		},
		{
			name:   "no token file",
			config: `{"target": "bdgateway", "bdgateway": {"url": "http://gw:7440"}}`,
			want:   []string{"no \"token_file\"", "BD_GATEWAY_TOKEN_FILE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.writeConfig(t, tt.config)
			if exit := h.run("bd", "show", "gt-x"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			for _, want := range tt.want {
				if !strings.Contains(h.stderr.String(), want) {
					t.Fatalf("stderr %q does not name %q", h.stderr, want)
				}
			}
			if h.bdBuilt || h.fake.launched {
				t.Fatal("an incomplete bdgateway config built and launched a target")
			}
		})
	}
}

// TestRunRefusesABdGatewayCallNotNamedBd: the bdgateway target serves bd only, so
// a call under another tool name is refused before any request.
func TestRunRefusesABdGatewayCallNotNamedBd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdGatewayConfigFile)
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "bdgateway target runs bd") {
		t.Fatalf("stderr %q does not say the target runs bd", h.stderr)
	}
	if h.fake.launched || h.bdBuilt {
		t.Fatal("a non-bd call through the bdgateway target was launched")
	}
}

// TestRunRefusesAnUnwiredBdGatewayTarget: a wrapper built without a bdgateway
// constructor refuses rather than panicking, and launches nothing.
func TestRunRefusesAnUnwiredBdGatewayTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdGatewayConfigFile)
	h.surface.newBdGateway = nil
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no bdgateway target is wired") {
		t.Fatalf("stderr %q does not say the target is unwired", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("an unwired bdgateway target launched a call")
	}
}

// TestDoctorPrintsTheBdGatewayLines: a bdgateway diagnosis names the URL and the
// token file rather than a container and image.
func TestDoctorPrintsTheBdGatewayLines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdGatewayConfigFile)
	h.fake.diag = gtwrap.Diagnosis{
		Rung:      gtwrap.RungExec,
		URL:       "http://gw:7440",
		TokenFile: "/run/secrets/daemon.token",
		Reason:    "the bd gateway answers /readyz",
	}
	if exit := h.run("bd", doctorFlag); exit != 0 {
		t.Fatalf("run(%s) = %d, want 0; stderr: %s", doctorFlag, exit, h.stderr)
	}
	want := "rung: exec\nurl: http://gw:7440\ntoken_file: /run/secrets/daemon.token\nreason: the bd gateway answers /readyz\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
}

// TestRunFileArgRewrittenForBdGateway: the gateway's workspace does not mirror
// the caller's tree, so a file a bd flag names travels as stdin with the flag's
// value rewritten, exactly as through the kubernetes target.
func TestRunFileArgRewrittenForBdGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, bdGatewayConfigFile)
	path := writeMacFile(t, h.town, "body.md", "the bead text\n")
	if exit := h.run("bd", "create", "--title", "t", "--body-file", path); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	want := []string{"bd", "create", "--title", "t", "--body-file", "-"}
	if !slices.Equal(h.fake.call.Argv, want) {
		t.Errorf("target argv = %q, want %q", h.fake.call.Argv, want)
	}
	if h.fake.call.Stdin == nil {
		t.Fatal("the target got no stdin; the file was not fed")
	}
	if got, _ := io.ReadAll(h.fake.call.Stdin); string(got) != "the bead text\n" {
		t.Errorf("stdin = %q, want the file's bytes", got)
	}
}
