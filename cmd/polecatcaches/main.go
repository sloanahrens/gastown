// Command polecatcaches applies the seed Job for the shared polecat cache
// claim (gt-s9r1s). The Job itself is built by internal/polecat/cachevol from
// deploy/polecat/seed-pins, so the command's only cluster dependency is
// kubectl and it runs wherever kubectl reaches the sandbox cluster.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/polecat/seed"
	"github.com/steveyegge/gastown/internal/rig"
)

func main() {
	os.Exit(runWith(os.Args[1:], os.Stdout, os.Stderr, prodDeps()))
}

// deps is the command's outside world: the pins file, each rig's Forgejo
// remote, and the kubectl call. Unit tests replace all three, so no test reads
// a file, a town config or the cluster.
type deps struct {
	readFile  func(path string) ([]byte, error)
	remoteURL func(rigName string) (string, error)
	// kubectl runs kubectl with args, feeding stdin to it, and returns its
	// standard output. A failure's error names the command and never its
	// output, which goes straight to the terminal so nothing captured can
	// carry a secret into an error.
	kubectl func(stdin io.Reader, args ...string) ([]byte, error)
}

// prodDeps is the real world. The town root is read from -town, so the
// resolver is built inside runWith.
func prodDeps() deps {
	return deps{
		readFile: os.ReadFile,
		kubectl:  execKubectl,
	}
}

// execKubectl is the thin wrapper around kubectl: applying a manifest is
// platform work, so the only logic here is wiring the process's streams.
func execKubectl(stdin io.Reader, args ...string) ([]byte, error) {
	return runCommand("kubectl", stdin, args...)
}

// runCommand runs name, feeds it stdin and passes its stderr straight to the
// terminal, and reports its stdout. A failure names the command and never
// carries its output, which can hold a secret.
func runCommand(name string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: kubectl in production; the verb and flag words are fixed and the Job name is a validated DNS-1123 label, so no shell and no user text
	cmd.Stdin = stdin
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func runWith(args []string, out, errw io.Writer, d deps) int {
	fs := flag.NewFlagSet("polecatcaches", flag.ContinueOnError)
	fs.SetOutput(errw)
	pinsPath := fs.String("pins", "deploy/polecat/seed-pins", "the file naming each pinned rig's commit")
	town := fs.String("town", "", "the town root whose rigs hold the Forgejo remotes the Job clones")
	image := fs.String("image", "", "the polecat image tag the Job's containers run")
	claim := fs.String("claim", "gt-cache", "the PersistentVolumeClaim the Job fills")
	namespace := fs.String("namespace", "", "the Job's namespace; empty uses the kubectl context's namespace")
	name := fs.String("name", "", "the Job's name; empty uses the builder's default")
	forgejoHost := fs.String("forgejo-host", "", "the host[:port] the Job clones from; empty keeps each rig's configured host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *town == "" {
		fmt.Fprintln(errw, "polecatcaches: -town is required: it holds each rig's Forgejo remote")
		return 2
	}
	if *image == "" {
		fmt.Fprintln(errw, "polecatcaches: -image is required: the seed Job's containers run the polecat image")
		return 2
	}
	if d.remoteURL == nil {
		d.remoteURL = func(rigName string) (string, error) { return forgejoRemote(*town, rigName) }
	}

	pins, code := resolvePins(fs.Args(), *pinsPath, *forgejoHost, errw, d)
	if code != 0 {
		return code
	}

	job, err := cachevol.BuildSeedJob(cachevol.SeedJobInput{
		Name:      *name,
		Namespace: *namespace,
		Image:     *image,
		ClaimName: *claim,
		Pins:      pins,
	})
	if err != nil {
		fmt.Fprintf(errw, "polecatcaches: %v\n", err)
		return 1
	}

	// A Job's pod template is immutable, so re-applying one whose pins changed
	// fails; deleting first is what lets a changed pin reseed. The pass is
	// cheap when the pins did not change, because the Job's marker exits before
	// running a step (internal/polecat/cachevol).
	del := []string{"delete", "job", job.Metadata.Name, "--ignore-not-found"}
	if *namespace != "" {
		del = append(del, "--namespace", *namespace)
	}
	if code := kubectl(d, out, errw, nil, del); code != 0 {
		return code
	}
	if code := kubectl(d, out, errw, bytes.NewReader(job.JSON()), []string{"apply", "-f", "-"}); code != 0 {
		return code
	}

	rigs := make([]string, len(pins))
	for i, p := range pins {
		rigs[i] = p.Rig
	}
	fmt.Fprintf(out, "polecatcaches: applied Job %s filling %s from %d pin(s): %s\n",
		job.Metadata.Name, *claim, len(pins), strings.Join(rigs, " "))
	return 0
}

// kubectl runs one kubectl call and passes its output through, returning the
// command's exit code when the call fails.
func kubectl(d deps, out, errw io.Writer, stdin io.Reader, args []string) int {
	text, err := d.kubectl(stdin, args...)
	if err != nil {
		fmt.Fprintf(errw, "polecatcaches: %v\n", err)
		return 1
	}
	if s := strings.TrimSpace(string(text)); s != "" {
		fmt.Fprintln(out, s)
	}
	return 0
}

// resolvePins reads the pins file, keeps the rigs named on the command line
// when any are, and gives each pin the clone URL the Job's init container
// fetches. A named rig with no pin line is refused: a seed that silently left
// it out would leave its caches empty.
func resolvePins(only []string, pinsPath, forgejoHost string, errw io.Writer, d deps) ([]cachevol.Pin, int) {
	data, err := d.readFile(pinsPath)
	if err != nil {
		fmt.Fprintf(errw, "polecatcaches: cannot read %s: %v\n", pinsPath, err)
		return nil, 1
	}
	sources, err := seed.Pins(data)
	if err != nil {
		fmt.Fprintf(errw, "polecatcaches: %s: %v\n", pinsPath, err)
		return nil, 1
	}

	wanted := make(map[string]bool, len(only))
	for _, r := range only {
		wanted[r] = true
	}
	found := make(map[string]bool, len(sources))
	pins := make([]cachevol.Pin, 0, len(sources))
	for _, s := range sources {
		if len(wanted) > 0 && !wanted[s.Rig] {
			continue
		}
		found[s.Rig] = true
		remote, err := d.remoteURL(s.Rig)
		if err != nil {
			fmt.Fprintf(errw, "polecatcaches: %v\n", err)
			return nil, 1
		}
		clone, err := cloneURL(s.Rig, remote, forgejoHost)
		if err != nil {
			fmt.Fprintf(errw, "polecatcaches: %v\n", err)
			return nil, 1
		}
		pins = append(pins, cachevol.Pin{Rig: s.Rig, Commit: s.Commit, URL: clone})
	}
	for _, r := range only {
		if !found[r] {
			fmt.Fprintf(errw, "polecatcaches: %s names no pin in %s\n", r, pinsPath)
			return nil, 1
		}
	}
	return pins, 0
}

// forgejoRemote is a rig's configured Forgejo URL. A rig with no block cannot
// be cloned by the Job, and the sandbox path refuses it the same way.
func forgejoRemote(town, rigName string) (string, error) {
	fc := rig.ResolveForgejoConfig(town, rigName)
	if fc == nil || strings.TrimSpace(fc.RemoteURL) == "" {
		return "", fmt.Errorf("rig %s has no merge_queue.forgejo.remote_url, so the seed Job has no Forgejo URL to clone it from", rigName)
	}
	return fc.RemoteURL, nil
}

// cloneURL is the URL the seed Job clones rigName from: the rig's Forgejo
// remote, with host swapped in when -forgejo-host names one. Userinfo is
// dropped because the Job clones without credentials; an error never quotes
// the URL, which can hold one.
func cloneURL(rigName, remote, host string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(remote))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("rig %s: the Forgejo remote is not an http(s) URL with a host", rigName)
	}
	u.User = nil
	if host == "" {
		return u.String(), nil
	}
	authority, err := cloneAuthority(host, u.Port())
	if err != nil {
		return "", fmt.Errorf("rig %s: -forgejo-host %q: %w", rigName, host, err)
	}
	u.Host = authority
	return u.String(), nil
}

// cloneAuthority is host[:port]. A host that names no port takes remotePort,
// as polecat's sandbox clone URL takes the port from the rig's remote.
func cloneAuthority(host, remotePort string) (string, error) {
	h, port := host, ""
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		h, port = host[:i], host[i+1:]
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("want host[:port]")
		}
	}
	if !config.ValidSandboxForgejoHost(h) {
		return "", errors.New("want a bare host name: letters, digits, dots and hyphens")
	}
	if port == "" {
		port = remotePort
	}
	if port != "" {
		return net.JoinHostPort(h, port), nil
	}
	return h, nil
}
