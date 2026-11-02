package templates_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// clientSpec is the live town with the client configuration set: a derived
// kubeconfig first, the original second, the host's openshell config directory,
// and two daemon.env entries.
func clientSpec() templates.ComposeSpec {
	s := liveSpec()
	s.Env = map[string]string{"BEADS_DOLT_POOL_READ_TIMEOUT": "30s", "A_FIRST": "x$y"}
	s.KubeconfigFiles = []string{"/towns/live/.runtime/container/kubeconfig.yaml", "/towns/live/.runtime/sbx-k8s/kubeconfig"}
	s.OpenShellConfigDir = "/host/home/.config/openshell"
	return s
}

// The openshell config directory (the gateway's mTLS client certificates) is
// bound read-only at the fixed path under XDG_CONFIG_HOME, and the file carries
// the host path only, never a file's contents (gt-yj0vh).
func TestComposeBindsTheGatewayMTLSDirReadOnly(t *testing.T) {
	t.Parallel()
	out, f := render(t, clientSpec())

	want := composeVolume{Type: "bind", Source: "/host/home/.config/openshell", Target: templates.LiveOpenShellConfigPath, ReadOnly: true}
	if !slices.Contains(f.Services["daemon"].Volumes, want) {
		t.Errorf("daemon volumes = %v, want the read-only mount %+v", f.Services["daemon"].Volumes, want)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-live-client.golden.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose-live-client.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

func TestRenderDaemonComposeRefusesWhatTheClientConfigCannotCarry(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*templates.ComposeSpec)
		want   string
	}{
		"an owned name":     {func(s *templates.ComposeSpec) { s.Env = map[string]string{"KUBECONFIG": "x"} }, "set by the render itself"},
		"a bad name":        {func(s *templates.ComposeSpec) { s.Env = map[string]string{"A B": "x"} }, "not an environment variable name"},
		"a control char":    {func(s *templates.ComposeSpec) { s.Env = map[string]string{"A": "x\ny"} }, "control character"},
		"a relative config": {func(s *templates.ComposeSpec) { s.OpenShellConfigDir = "rel" }, "not an absolute path"},
		"a colon in a path": {func(s *templates.ComposeSpec) { s.KubeconfigFiles = []string{"/a:b"} }, "colon"},
		"on a scratch spec": {func(s *templates.ComposeSpec) { *s = scratchClientBase(); s.Env = map[string]string{"A": "x"} }, "scratch spec"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := liveSpec()
			tc.mutate(&s)
			if _, err := templates.RenderDaemonCompose(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func scratchClientBase() templates.ComposeSpec {
	s := liveSpec()
	s.Scratch = true
	s.ScratchDoltImage = templates.DoltServerImage
	s.LiveTownRoot = "/towns/other-live"
	s.TownRoot = "/towns/scratch"
	s.DoltHost = ""
	return s
}
