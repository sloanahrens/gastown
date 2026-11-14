package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// TestRenderDaemonBdGatewayURLRefused holds --daemon-bd-gateway-url to a refusal per
// bad value. Each refusal writes nothing, and none echoes the URL, since one with
// credentials would put them in the error. An empty value is refused by the flag;
// a malformed URL is refused by the render's Build, which names the field.
func TestRenderDaemonBdGatewayURLRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		set       bool
		rehearsal bool
		names     string
	}{
		{name: "an empty value", set: true, names: "--daemon-bd-gateway-url"},
		{name: "a whitespace-only value", url: "   ", set: true, names: "--daemon-bd-gateway-url"},
		{name: "a non-http scheme", url: "ftp://host:7440", set: true, names: "Values.DaemonBdGatewayURL"},
		{name: "no host", url: "http://:7440", set: true, names: "Values.DaemonBdGatewayURL"},
		{name: "credentials", url: "http://gt:hunter2@host:7440", set: true, names: "Values.DaemonBdGatewayURL"},
		{name: "a query", url: "http://host:7440/?x=1", set: true, names: "Values.DaemonBdGatewayURL"},
		{name: "a rehearsal", url: "http://host:7440", set: true, rehearsal: true, names: "Values.DaemonBdGatewayURL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.daemonBdGatewayURL = tc.url
			f.daemonBdGatewayURLSet = tc.set
			if tc.rehearsal {
				// A rehearsal names its own namespace and takes no forge host, so the
				// fixture's live values are replaced.
				f.rehearsal = true
				f.namespace = "gt-rehearsal"
				f.forgeHost = ""
			}
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			requireErrorNames(t, err, tc.names)
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal %q echoes the credentials", err)
			}
			if got := outState(f.out); got != "absent" {
				t.Errorf("the refused render left %s: %s", f.out, got)
			}
		})
	}
}

// TestRenderDaemonBdGatewayNoTokenValue holds that a flagged render reads no token:
// the operator's config directory holds a file under the daemon Secret's name with a
// sentinel value, and the sentinel reaches neither the printed output nor any
// manifest. No Secret document names the gateway Secret either.
func TestRenderDaemonBdGatewayNoTokenValue(t *testing.T) {
	t.Parallel()
	const sentinel = "bd-gateway-token-sentinel-7f3a"
	f := clusterRenderFixture(t)
	f.configDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(f.configDir, k8sdaemon.BdGatewayDaemonSecretName), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("writing the token stand-in: %v", err)
	}
	f.daemonBdGatewayURL = "http://host.docker.internal:7440"
	f.daemonBdGatewayURLSet = true
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.String(), sentinel) {
		t.Errorf("the printed output carries the token value:\n%s", out.String())
	}
	for _, name := range dirNames(t, f.out) {
		path := filepath.Join(f.out, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if strings.Contains(string(data), sentinel) {
			t.Errorf("%s carries the token value", name)
		}
		if renderedKind(t, path) == "Secret" && strings.Contains(string(data), k8sdaemon.BdGatewayDaemonSecretName) {
			t.Errorf("%s writes the daemon's gateway Secret, which the operator creates", name)
		}
	}
}

// TestDaemonBdGatewayFlag holds the flag to its effects on the daemon: the environment
// names the gateway and the token file, the token Secret is mounted read-only at the
// token directory, and no argument changes. The Secret needs no create line, and the
// note names it. With the flag off, none of it appears.
func TestDaemonBdGatewayFlag(t *testing.T) {
	t.Parallel()
	const gateway = "http://host.docker.internal:7440"
	town := clusterRenderTestTown(t)
	deps := func(out io.Writer) clusterRenderDeps {
		d := clusterRenderTestDeps(t, out)
		d.findTown = func() (string, error) { return town, nil }
		return d
	}

	off := clusterRenderFixture(t)
	var offOut bytes.Buffer
	if err := runClusterRender(deps(&offOut), off); err != nil {
		t.Fatalf("render without the flag: %v", err)
	}
	offArgs, offMounts, offEnv := daemonContainer(t, off.out)
	if _, ok := offEnv[k8sdaemon.BdGatewayURLEnv]; ok {
		t.Errorf("the flag-free daemon names %s", k8sdaemon.BdGatewayURLEnv)
	}
	if _, ok := offMounts[k8sdaemon.BdGatewayTokenDir]; ok {
		t.Errorf("the flag-free daemon mounts the gateway token directory")
	}

	// The same fixture with the flag on writes to a second directory, so the create
	// lines compare under the same config paths. The URL carries surrounding spaces,
	// which the render trims before it reaches the pod.
	on := off
	on.out = filepath.Join(t.TempDir(), "out-with-daemon-bd-gateway")
	on.daemonBdGatewayURL = " " + gateway + " "
	on.daemonBdGatewayURLSet = true
	var onOut bytes.Buffer
	if err := runClusterRender(deps(&onOut), on); err != nil {
		t.Fatalf("render with the flag: %v", err)
	}
	args, mounts, env := daemonContainer(t, on.out)
	if !slices.Equal(args, offArgs) {
		t.Errorf("the flag changed the daemon's arguments: %v, want %v", args, offArgs)
	}
	if got := env[k8sdaemon.BdGatewayURLEnv]; got != gateway {
		t.Errorf("%s = %q, want the trimmed %q", k8sdaemon.BdGatewayURLEnv, got, gateway)
	}
	if got := env[k8sdaemon.BdGatewayTokenFileEnv]; got != k8sdaemon.BdGatewayTokenPath {
		t.Errorf("%s = %q, want %q", k8sdaemon.BdGatewayTokenFileEnv, got, k8sdaemon.BdGatewayTokenPath)
	}
	if _, ok := mounts[k8sdaemon.BdGatewayTokenDir]; !ok {
		t.Errorf("the daemon mounts nothing at %s (mounts %v)", k8sdaemon.BdGatewayTokenDir, mounts)
	}
	if got, want := createLines(onOut.String()), createLines(offOut.String()); !slices.Equal(got, want) {
		t.Errorf("the create lines with the flag = %q, want %q: the gateway Secret has none", got, want)
	}
	if !strings.Contains(onOut.String(), k8sdaemon.BdGatewayDaemonSecretName) || !strings.Contains(onOut.String(), "does not create it") {
		t.Errorf("the output names no note for the gateway Secret:\n%s", onOut.String())
	}
	if strings.Contains(offOut.String(), k8sdaemon.BdGatewayDaemonSecretName) {
		t.Errorf("the flag-free output names the gateway Secret:\n%s", offOut.String())
	}
}
