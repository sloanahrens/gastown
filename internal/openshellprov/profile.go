package openshellprov

import (
	"fmt"
	"io"
	"strings"
)

// gtBinary is the program every endpoint call runs.
const gtBinary = "gt"

// defaultGateway is the gateway a sandbox profile with no gateway runs on, and
// the one Gt targets when its Gateway is empty.
const defaultGateway = "openshell"

// blanks are the characters the script's awk reads as a space in a profile.
const blanks = " \t\r\v\f"

// ProfileID returns the id a profile yaml declares: the name import keys on and
// the name the gateway reports a clash under. Only a line that starts with "id:"
// at the start of the line counts, so a nested "- id:" inside credentials is not
// mistaken for it. A quoted id returns without its quotes; an unquoted one ends
// at the first space or comment. A profile with no such line returns "".
func ProfileID(yaml []byte) string {
	for _, line := range strings.Split(string(yaml), "\n") {
		rest, ok := strings.CutPrefix(line, "id:")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, blanks)
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
				return rest[1 : 1+end]
			}
		}
		if end := strings.IndexAny(rest, blanks+"#"); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	return ""
}

// Gt makes the `gt sandbox endpoint` calls a run needs for one gateway. Every
// call goes through Run, so a unit test runs no process.
type Gt struct {
	// Run runs every gt call.
	Run Runner
	// Gateway is the --gateway name; empty is the default gateway, openshell.
	Gateway string
	// Root is the gt source checkout the policy paths are relative to, passed as --root.
	Root string
}

// gateway is the --gateway name every call targets.
func (g Gt) gateway() string {
	if g.Gateway == "" {
		return defaultGateway
	}
	return g.Gateway
}

// endpoint runs one `gt sandbox endpoint` call with extra flags and returns what
// it wrote to stdout. Its stderr is dropped, so no captured output can reach an
// error built from this call.
func (g Gt) endpoint(extra ...string) ([]byte, error) {
	args := append([]string{"sandbox", "endpoint", "--gateway", g.gateway(), "--root", g.Root}, extra...)
	stdout, _, err := g.Run.Run(gtBinary, args, nil, nil)
	return stdout, err
}

// CheckEndpoint asks gt for the endpoint once, before anything is imported, so a
// gateway gt refuses stops the run with nothing touched. The answer is discarded.
func (g Gt) CheckEndpoint() error {
	if _, err := g.endpoint(); err != nil {
		return fmt.Errorf("%s: gt sandbox endpoint --gateway %s failed, so the forgejo-polecat profile cannot be rendered: %w", Name, g.gateway(), err)
	}
	return nil
}

// Render writes the forgejo-polecat profile file, bound to the gateway's endpoint,
// to out. A non-empty exportFile stamps the profile with the resource_version of
// that export, which profile update needs. A failed render writes nothing.
func (g Gt) Render(file string, out io.Writer, exportFile string) error {
	extra := []string{"--render", file}
	if exportFile != "" {
		extra = append(extra, "--from-export", exportFile)
	}
	stdout, err := g.endpoint(extra...)
	if err != nil {
		return fmt.Errorf("%s: gt sandbox endpoint --render %s failed, so the forgejo-polecat profile cannot be rendered: %w", Name, file, err)
	}
	if _, err := out.Write(stdout); err != nil {
		return fmt.Errorf("%s: writing the rendered profile %s: %w", Name, file, err)
	}
	return nil
}

// StaleReason returns why the gateway's profile id, exported to exportFile, does
// not bind the gateway's endpoint: the reason after "stale:" in gt's answer. It
// returns "" when the profile binds the endpoint. A failed check, or an answer
// that is neither bound nor stale with a reason, is an error that names the
// profile. The answer itself is not echoed into the error.
func (g Gt) StaleReason(id, exportFile string) (string, error) {
	stdout, err := g.endpoint("--check", exportFile)
	if err != nil {
		return "", fmt.Errorf("%s: gt sandbox endpoint --check failed on the exported profile %s: %w", Name, id, err)
	}
	verdict := strings.TrimRight(string(stdout), "\n")
	if verdict == "bound" {
		return "", nil
	}
	if reason, ok := strings.CutPrefix(verdict, "stale:"); ok {
		if reason = strings.TrimSpace(reason); reason != "" {
			return reason, nil
		}
	}
	return "", fmt.Errorf("%s: gt sandbox endpoint --check answered neither bound nor stale for %s", Name, id)
}
