package openshellprov

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// openshellBinary is the program every gateway call runs.
const openshellBinary = "openshell"

// Runner runs one external command and returns what it wrote to each stream.
// Every openshell call goes through it, so a unit test runs no process.
type Runner interface {
	Run(name string, args []string, env []string, stdin io.Reader) (stdout, stderr []byte, err error)
}

// ExecRunner is the production Runner. A nil env inherits the process environment.
type ExecRunner struct {
	// Command builds the command to run; nil is exec.Command. A test swaps in a
	// command that cannot start, so its failure path runs with no process.
	Command func(name string, args ...string) *exec.Cmd
}

// Run runs name with args through os/exec and returns what it wrote.
func (r ExecRunner) Run(name string, args []string, env []string, stdin io.Reader) ([]byte, []byte, error) {
	command := r.Command
	if command == nil {
		command = exec.Command
	}
	cmd := command(name, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Client makes the openshell calls the port needs, through one Runner.
type Client struct {
	// Run runs every openshell call.
	Run Runner
	// Gateway is the -g name every call targets; empty is the CLI's default gateway.
	Gateway string
}

// args returns the openshell argv for subcommand. The -g flag is global to
// openshell, so it has to come before the subcommand.
func (c Client) args(subcommand ...string) []string {
	if c.Gateway == "" {
		return subcommand
	}
	return append([]string{"-g", c.Gateway}, subcommand...)
}

// ProviderExists reports whether the gateway lists a provider named name. A
// list that fails is an error: read as absent, a provider the gateway already
// holds would be created again and fail with a clash that hides the cause.
func (c Client) ProviderExists(name string) (bool, error) {
	stdout, _, err := c.Run.Run(openshellBinary, c.args("provider", "list"), nil, nil)
	if err != nil {
		return false, fmt.Errorf("%s: openshell provider list failed, so whether provider %s exists is unknown", Name, name)
	}
	return firstColumnIs(stdout, name), nil
}

// ProfileExists reports whether the gateway lists a global profile with id. A
// list that fails is an error: read as absent, a profile the gateway already
// holds would be imported again and fail with a clash that hides the cause.
func (c Client) ProfileExists(id string) (bool, error) {
	stdout, stderr, err := c.Run.Run(openshellBinary, c.args("profile", "list", "--global"), nil, nil)
	if err != nil {
		return false, fmt.Errorf("%s: openshell profile list --global failed, so whether profile %s exists is unknown:\n%s", Name, id, stderr)
	}
	return firstColumnIs(stdout, id), nil
}

// ExportProfile writes the global profile id the gateway holds to out. A failed
// export writes nothing and is an error: guessing whether the profile binds the
// gateway's endpoint would either keep a stale profile or update a current one.
// The -- ends option parsing, so an id that starts with a dash is read as an id.
func (c Client) ExportProfile(id string, out io.Writer) error {
	stdout, stderr, err := c.Run.Run(openshellBinary, c.args("profile", "export", "--global", "--", id), nil, nil)
	if err != nil {
		return fmt.Errorf("%s: openshell profile export --global %s failed, so whether it binds the gateway's endpoint is unknown:\n%s", Name, id, stderr)
	}
	if _, err := out.Write(stdout); err != nil {
		return fmt.Errorf("%s: writing profile %s: %w", Name, id, err)
	}
	return nil
}

// firstColumnIs reports whether a line of a table listing starts with name. Only
// the first column counts, so a provider named deepseek-legacy is not deepseek.
func firstColumnIs(listing []byte, name string) bool {
	for _, line := range strings.Split(string(listing), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}
