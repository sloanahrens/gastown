// Command render-services renders an environment's control-plane Services from
// its hosts file and writes them to stdout:
//
//	render-services deploy/k8s/values/kind/hosts.yaml > deploy/k8s/values/kind/control-plane-services.yaml
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

	"github.com/steveyegge/gastown/internal/k8spolicy"
)

func main() {
	os.Exit(run(os.Args[1:], os.ReadFile, os.Stdout, os.Stderr))
}

func run(args []string, read func(string) ([]byte, error), out, errw io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errw, "usage: render-services <values/ENV/hosts.yaml>")
		return 2
	}
	raw, err := read(args[0])
	if err != nil {
		fmt.Fprintf(errw, "render-services: cannot read %s: %v\n", args[0], err)
		return 1
	}
	env := filepath.Base(filepath.Dir(filepath.Clean(args[0])))
	text, err := k8spolicy.RenderServices(env, raw)
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
