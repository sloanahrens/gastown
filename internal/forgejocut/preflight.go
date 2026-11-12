package forgejocut

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/land"
)

// Runner is the exec seam the run's child processes go through. argv[0] is the
// program. A test injects a fake; the command injects the real exec. No child
// receives the admin token: the probe runs in this process, through ProbeFunc.
type Runner interface {
	// Output runs argv and returns its stdout: the answer a read needs.
	Output(argv []string) ([]byte, error)
	// Run runs argv with the run's own stdout and stderr, so a child's log reaches
	// the operator. The token variables are removed from its environment.
	Run(argv []string) error
}

// countPattern is what the bd count prints: digits and nothing else.
var countPattern = regexp.MustCompile(`^[0-9]+$`)

// LandingCount returns how many open beads in the rig at rigRoot wear the
// ready-to-land label: the queue the landing worker drains. A failing read, or an
// answer that is not a plain count, is an error that names the command. It is
// never zero, so "I could not tell" cannot read as "nothing is in flight".
func LandingCount(run Runner, rigRoot string) (int, error) {
	argv := []string{"bd", "-C", rigRoot, "count", "--status", "open", "--label", land.LabelReadyToLand}
	out, err := run.Output(argv)
	if err != nil {
		return 0, fmt.Errorf("%s failed: %w", commandText(argv), err)
	}
	// Command substitution strips trailing newlines and nothing else, so a count
	// with a space or a sign around it is refused.
	text := strings.TrimRight(string(out), "\n")
	if !countPattern.MatchString(text) {
		return 0, fmt.Errorf("%s printed no plain count", commandText(argv))
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s printed a count that does not fit: %w", commandText(argv), err)
	}
	return n, nil
}

// RefuseIfLandingInFlight is step 1's first half. It refuses the cutover while
// the rig has an open ready-to-land bead, since that bead is a landing queued or
// in flight; an empty queue is logged and passes.
func RefuseIfLandingInFlight(run Runner, o *Options, log func(string)) error {
	n, err := LandingCount(run, o.RigRoot)
	if err != nil {
		return fmt.Errorf("could not read the landing queue of %s: %w", o.Rig, err)
	}
	if n != 0 {
		return fmt.Errorf("%s has %d open %s bead(s): land them (or wait) before cutting the rig over",
			o.Rig, n, land.LabelReadyToLand)
	}
	log(o.Rig + "'s landing queue is empty")
	return nil
}

// ProbeFunc is one probe run: forgejoprobe.Run, and a test's stand-in for it,
// which records the arguments and the environment it is handed and forces the
// exit code.
type ProbeFunc func(ctx context.Context, args []string, getenv func(string) string, s forgejoprobe.Streams) int

// runProbe is the production ProbeFunc: the probe in this process, on the
// operating system's seams.
func runProbe(ctx context.Context, args []string, getenv func(string) string, s forgejoprobe.Streams) int {
	return forgejoprobe.Run(ctx, args, getenv, s, forgejoprobe.SystemDeps())
}

// adminTokenEnv is the stand-in variable the admin token may arrive in. Its name
// only, not a credential.
const adminTokenEnv = "FORGEJO_ADMIN_TOKEN" //nolint:gosec // G101: the variable's name, not a credential

// ProbeGreen is step 1's second half. The probe pushes the rig's main through its
// real runner and waits for the gate, so a green run is the evidence the landing
// path works. Under a dry run the probe is printed and not run.
//
// The probe runs in this process, through probe, with the flags the shell probe
// took, so no child process starts for it. It carries --town-root so it pushes from
// a worktree of the rig's bare repository and the rig's hooks run. Its push rides
// the admin git URL, because it happens before provisioning, when the bots have no
// access yet.
//
// The probe reads its token through getenv, which is base with the token variables
// replaced: with no token file named, the resolved token is answered under the
// broker's name; with one named, both token variables are empty and the probe reads
// the file itself. The token is in no argv word, no log line and no child
// environment.
func ProbeGreen(ctx context.Context, probe ProbeFunc, o *Options, token forgejoprov.Secret, base func(string) string, s forgejoprobe.Streams, log func(string)) error {
	args := []string{o.Rig,
		"--repo", o.Repo,
		"--main-branch", o.MainBranch,
		"--town-root", o.TownRoot,
		"--api-url", o.APIURL,
		"--web-url", o.WebURL,
	}
	if o.AdminTokenFile != "" {
		args = append(args, "--admin-token-file", o.AdminTokenFile)
	}
	args = append(args,
		"--github-url", o.GitHubURL,
		"--git-url", o.AdminGitURL,
	)
	if o.DryRun {
		log("+ " + commandText(append([]string{"forgejo-probe"}, args...)))
		log("(dry run: the probe was not run)")
		return nil
	}
	log("probing " + o.Rig + " through its real runner before the cutover")
	if rc := probe(ctx, args, probeGetenv(base, o.AdminTokenFile, token), s); rc != forgejoprobe.ExitOK {
		// The probe's own output already reached the operator, and rc is only its exit
		// code, so the message does not repeat either.
		return fmt.Errorf("the probe of %s is not green; fix the runner or the gate workflow and re-run", o.Rig)
	}
	return nil
}

// probeGetenv is the environment the probe reads: base, except the two token
// variables. With no token file the resolved token is answered under the broker's
// name, which the probe resolves first; with a token file both are empty, so the
// probe reads the file and no other token source.
func probeGetenv(base func(string) string, tokenFile string, token forgejoprov.Secret) func(string) string {
	return func(name string) string {
		switch name {
		case forgejoprov.BrokerTokenEnv:
			if tokenFile == "" {
				return token.Reveal()
			}
			return ""
		case adminTokenEnv:
			return ""
		}
		return base(name)
	}
}

// commandText is argv as one line, for an error or a log line.
func commandText(argv []string) string {
	return strings.Join(argv, " ")
}
