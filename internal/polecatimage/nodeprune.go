package polecatimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/polecat/imageprune"
)

// execProgram runs one program and returns its standard output.
func execProgram(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// nodePruneRepos is the default --repos: the repositories the polecat image runs pull by digest.
const nodePruneRepos = "gastown-polecat,gastown-daemon,gastown-bd"

// stringList is a repeatable string flag: each occurrence appends one value.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// nodePruneWith is the node-prune helper. It plans the images to remove from the node whose CRI socket is
// --endpoint and prints one line per planned image. Without --apply the plan is a dry run: nothing is
// removed. With --apply each planned image is removed, one at a time, and reported. run executes each crictl
// call's program and returns its standard output; the dispatcher gives execProgram, and a test gives a fake.
//
// Exit 0 means the node was read and the plan printed, or every apply removal succeeded, and -h prints the
// usage and exits 0. Exit 3 means the node could not be read, with the reason on stderr and no would-remove
// line, so a missing plan is never read as an empty one; it also means an apply removal failed, or a flag
// the helper does not parse (an unknown one, or a value it cannot read). Exit 2 is a usage error the helper
// checks itself.
func nodePruneWith(args []string, stdout, stderr io.Writer, run func(name string, args ...string) ([]byte, error)) int {
	flags := flag.NewFlagSet("node-prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keep := flags.Int("keep", 3, "the newest images of each repository to keep")
	repos := flags.String("repos", nodePruneRepos, "the comma-separated repositories to prune")
	var spares stringList
	flags.Var(&spares, "spare", "an image id or digest the plan must not remove; repeatable")
	crictlBin := flags.String("crictl-bin", "crictl", "the program that runs crictl")
	var crictlArgs stringList
	flags.Var(&crictlArgs, "crictl-arg", "an argument placed before each crictl call, such as crictl when the program is k3s; repeatable")
	endpoint := flags.String("endpoint", "", "the CRI socket of the node, passed as --runtime-endpoint")
	apply := flags.Bool("apply", false, "remove the planned images; without it nothing is removed")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 3
	}
	if *keep < 0 {
		fmt.Fprintf(stderr, "node-prune: -keep must be 0 or more, got %d\n", *keep)
		return 2
	}
	if *endpoint == "" {
		fmt.Fprintln(stderr, "node-prune: --endpoint names the node's CRI socket; give it")
		return 2
	}
	for _, s := range spares {
		if !plainRef(s) {
			fmt.Fprintf(stderr, "node-prune: --spare %q is not an image id or digest: give printable ASCII without quotes or backslashes\n", s)
			return 2
		}
	}
	names := strings.Split(*repos, ",")
	for _, r := range names {
		if r == "" {
			fmt.Fprintf(stderr, "node-prune: --repos %q has an empty repository\n", *repos)
			return 2
		}
	}

	crictl := nodePruneCrictl(run, *crictlBin, crictlArgs, *endpoint, spares)
	res := imageprune.PruneNodeRepos(*endpoint, crictl, *keep, names, !*apply)
	if res.Err != nil {
		fmt.Fprintf(stderr, "node-prune: cannot read the node %s: %v\n", *endpoint, res.Err)
		return 3
	}
	if !*apply {
		for _, im := range res.Planned {
			fmt.Fprintf(stdout, "node-prune: would remove %s %s created %s\n",
				im.ID, repositoryOf(im, names), im.Created.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(stdout, "node-prune: dry run planned %d images\n", len(res.Planned))
		return 0
	}
	// A failed removal is named on stderr and the run goes on; the exit status carries the failure.
	for _, im := range res.Planned {
		if err, failed := res.Failed[im.ID]; failed {
			fmt.Fprintf(stderr, "node-prune: cannot remove %s: %v\n", im.ID, err)
			continue
		}
		fmt.Fprintf(stdout, "node-prune: removed %s\n", im.ID)
	}
	fmt.Fprintf(stdout, "node-prune: applied removed %d failed %d\n", len(res.Removed), len(res.Failed))
	if len(res.Failed) > 0 {
		return 3
	}
	return 0
}

// nodePruneCrictl runs crictl on the node: the program, the --crictl-arg lead, the CRI socket, then the
// call's own arguments. A ps reply goes through withSpares, so each spare counts as in use.
func nodePruneCrictl(run func(name string, args ...string) ([]byte, error), bin string, lead []string, endpoint string, spares []string) imageprune.Crictl {
	return func(args ...string) ([]byte, error) {
		argv := append(append(append([]string{}, lead...), "--runtime-endpoint", endpoint), args...)
		out, err := run(bin, argv...)
		if err != nil {
			return nil, fmt.Errorf("crictl %s: %w", crictlVerb(args), err)
		}
		if crictlVerb(args) == "ps" {
			return withSpares(out, spares)
		}
		return out, nil
	}
}

// crictlVerb is the crictl subcommand in args. imageprune's Timed puts --timeout and its value before the
// subcommand, so they are skipped.
func crictlVerb(args []string) string {
	if len(args) >= 2 && args[0] == "--timeout" {
		return crictlVerb(args[2:])
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// withSpares answers a `crictl ps` reply with one container per spare, each using that spare, so the
// plan reads the spare as in use. The reply is read in the forms the plan understands: JSON null (no
// containers, which is how crictl prints a node with none), or an object with no key other than containers,
// whose value is a list or null. Any other reply is refused: one read as no containers would let the plan
// remove images a container still uses.
func withSpares(ps []byte, spares []string) ([]byte, error) {
	var reply struct {
		Containers []json.RawMessage `json:"containers"`
	}
	dec := json.NewDecoder(bytes.NewReader(ps))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reply); err != nil {
		return nil, fmt.Errorf("crictl ps: the reply is not a containers object: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("crictl ps: the reply holds more than one JSON value")
	}
	for _, s := range spares {
		reply.Containers = append(reply.Containers, fmt.Appendf(nil, `{"image":{"image":%q},"imageRef":%q}`, s, s))
	}
	// Each container is a JSON value the decoder read or the line above wrote (plainRef keeps %q equal to a
	// JSON string), so joining them with commas is a valid object.
	parts := make([]string, len(reply.Containers))
	for i, c := range reply.Containers {
		parts[i] = string(c)
	}
	return []byte(`{"containers":[` + strings.Join(parts, ",") + `]}`), nil
}

// plainRef reports whether s is a non-empty run of printable ASCII with no quote or backslash, the
// characters a JSON string can hold as they are.
func plainRef(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// repositoryOf is the repository in repos that im's references name. The plan only selects an image whose
// every reference names one repository, so the first reference that matches is enough.
func repositoryOf(im imageprune.Image, repos []string) string {
	for _, refs := range [][]string{im.Tags, im.Digests} {
		for _, ref := range refs {
			name := refRepository(ref)
			for _, repo := range repos {
				if name == repo {
					return repo
				}
			}
		}
	}
	return ""
}

// refRepository is the repository a tag or digest reference names: the path after any registry host,
// without its tag or digest.
func refRepository(ref string) string {
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	return name
}
