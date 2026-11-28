package openshellprov

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// gtBinary is the program every endpoint call runs.
const gtBinary = "gt"

// defaultGateway is the gateway a sandbox profile with no gateway runs on, and
// the one Gt targets when its Gateway is empty.
const defaultGateway = "k8s"

// blanks are the characters the script's awk reads as a space in a profile.
const blanks = " \t\r\v\f"

// ProfileID returns the id a profile yaml declares: the name import keys on and
// the name the gateway reports a clash under. Only a line that starts with "id:"
// at the start of the line counts, so a nested "- id:" inside credentials is not
// mistaken for it. A quoted id returns without its quotes; an unquoted one ends
// at the first space or comment. A quote never closed returns "", not the rest
// of the line read as an id. A profile with no such line returns "".
func ProfileID(yaml []byte) string {
	for _, line := range strings.Split(string(yaml), "\n") {
		rest, ok := strings.CutPrefix(line, "id:")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, blanks)
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			end := strings.IndexByte(rest[1:], rest[0])
			if end < 0 {
				return ""
			}
			return rest[1 : 1+end]
		}
		if end := strings.IndexAny(rest, blanks+"#"); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	return ""
}

// Gt makes the `gt sandbox endpoint` calls a run needs for one gateway. Every
// call goes through Run, so a unit test runs no process. A call passes no --root:
// gt reads the gateway's policy from the root of the gt it runs as, and a call's
// profile arrives on stdin, so gt needs no root to find it.
type Gt struct {
	// Run runs every gt call.
	Run Runner
	// Gateway is the --gateway name; empty is the default gateway, k8s.
	Gateway string
	// Err receives what gt writes to stderr, which is gt's explanation of a refusal.
	Err io.Writer
}

// gateway is the --gateway name every call targets.
func (g Gt) gateway() string {
	if g.Gateway == "" {
		return defaultGateway
	}
	return g.Gateway
}

// endpoint runs one `gt sandbox endpoint` call with extra flags and stdin and
// returns what it wrote to stdout. Its stderr goes to Err, where gt's refusal
// reaches the operator, and never into an error built from this call.
func (g Gt) endpoint(stdin io.Reader, extra ...string) ([]byte, error) {
	args := append([]string{"sandbox", "endpoint", "--gateway", g.gateway()}, extra...)
	stdout, stderr, err := g.Run.Run(gtBinary, args, nil, stdin)
	if g.Err != nil {
		if _, werr := g.Err.Write(stderr); werr != nil {
			return stdout, fmt.Errorf("%s: writing gt's errors: %w", Name, werr)
		}
	}
	return stdout, err
}

// CheckEndpoint asks gt for the endpoint once, before anything is imported, so a
// gateway gt refuses stops the run with nothing touched. The answer is discarded.
func (g Gt) CheckEndpoint() error {
	if _, err := g.endpoint(nil); err != nil {
		return fmt.Errorf("%s: gt sandbox endpoint --gateway %s failed, so the forgejo-polecat profile cannot be rendered: %w", Name, g.gateway(), err)
	}
	return nil
}

// Render writes the forgejo-polecat profile, sent to gt on stdin and bound to the
// gateway's endpoint, to out. A non-empty exportFile stamps it with that export's
// resource_version, which profile update needs. A failed render writes nothing.
func (g Gt) Render(out io.Writer, profile []byte, exportFile string) error {
	extra := []string{"--render", "/dev/stdin"}
	if exportFile != "" {
		extra = append(extra, "--from-export", exportFile)
	}
	stdout, err := g.endpoint(bytes.NewReader(profile), extra...)
	if err != nil {
		return fmt.Errorf("%s: gt sandbox endpoint --render /dev/stdin failed, so the forgejo-polecat profile cannot be rendered: %w", Name, err)
	}
	if _, err := out.Write(stdout); err != nil {
		return fmt.Errorf("%s: writing the rendered profile: %w", Name, err)
	}
	return nil
}

// StaleReason returns why the gateway's profile id, exported and sent to gt on
// stdin, does not bind the gateway's endpoint: the reason after "stale:" in gt's
// answer. It returns "" when the profile binds the endpoint. A failed check, or an
// answer that is neither bound nor stale with a reason, is an error that names the
// profile and gives the answer.
func (g Gt) StaleReason(id string, exported []byte) (string, error) {
	stdout, err := g.endpoint(bytes.NewReader(exported), "--check", "/dev/stdin")
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
	return "", fmt.Errorf("%s: gt sandbox endpoint --check answered neither bound nor stale for %s: %s", Name, id, verdict)
}
