// Command render-services renders an environment's control-plane Services from
// its hosts file and writes them to stdout:
//
//	render-services deploy/k8s/values/kind/hosts.yaml > deploy/k8s/values/kind/control-plane-services.yaml
//
// With --registry-forward it renders the node-local registry forwarder instead
// (gt-gg9hv.25):
//
//	render-services --registry-forward deploy/k8s/values/kind/hosts.yaml > deploy/k8s/values/kind/registry-forward.yaml
//
// With --forgejo-server it renders the Forgejo server manifest for the
// environment's control-plane host (gt-gifs81). Render it to a file outside the
// repository and apply that file, so no manifest spelling the host is checked in:
//
//	render-services --forgejo-server deploy/k8s/values/kind/hosts.yaml > /tmp/forgejo-server.yaml
//	kubectl apply -f /tmp/forgejo-server.yaml
//
// The environment name is the hosts file's directory name. It exits 0 after
// writing, 2 for a wrong command line, and 1 when the file cannot be read or
// rendered; the reason is on stderr and stdout is empty.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/deploy/k8s/forgejo-server"
	"github.com/steveyegge/gastown/internal/k8spolicy"
)

// usage is the one command line shape both flag forms accept.
const usage = "usage: render-services [--registry-forward | --forgejo-server] <values/ENV/hosts.yaml>"

// registryForwardFlag selects the registry forwarder over the control-plane
// Services. It must come first, so a command line without it reads as before.
const registryForwardFlag = "--registry-forward"

// forgejoServerFlag selects the Forgejo server manifest over the control-plane
// Services. It must come first, so a command line without it reads as before.
const forgejoServerFlag = "--forgejo-server"

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

func run(args []string, read func(string) ([]byte, error), out, errw io.Writer) int {
	if len(args) > 0 && args[0] == forgejoServerFlag {
		return runForgejoServer(args[1:], read, out, errw)
	}
	render := k8spolicy.RenderServices
	if len(args) > 0 && args[0] == registryForwardFlag {
		render = k8spolicy.RenderRegistryForward
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	raw, err := read(args[0])
	if err != nil {
		fmt.Fprintf(errw, "render-services: cannot read %s: %v\n", args[0], err)
		return 1
	}
	env := filepath.Base(filepath.Dir(filepath.Clean(args[0])))
	text, err := render(env, raw)
	if err != nil {
		fmt.Fprintf(errw, "render-services: %s: %v\n", args[0], err)
		return 1
	}
	if _, err := io.WriteString(out, text); err != nil {
		fmt.Fprintf(errw, "render-services: writing: %v\n", err)
		return 1
	}
	return 0
}

// runForgejoServer renders the Forgejo server manifest for the one hosts file
// named on the command line. Its database host is the file's control_plane_host,
// so a hosts file without a usable host refuses instead of rendering a
// placeholder.
func runForgejoServer(args []string, read func(string) ([]byte, error), out, errw io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	raw, err := read(args[0])
	if err != nil {
		fmt.Fprintf(errw, "render-services: cannot read %s: %v\n", args[0], err)
		return 1
	}
	host, err := k8spolicy.ControlPlaneHost(raw)
	if err != nil {
		fmt.Fprintf(errw, "render-services: %s: %v\n", args[0], err)
		return 1
	}
	text, err := forgejoserver.Manifest(host)
	if err != nil {
		fmt.Fprintf(errw, "render-services: %s: %v\n", args[0], err)
		return 1
	}
	if _, err := out.Write(text); err != nil {
		fmt.Fprintf(errw, "render-services: writing: %v\n", err)
		return 1
	}
	return 0
}
