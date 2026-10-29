// Package rigbootstrap is the pure half of bootstrap-local-rig: the command
// line and the rig settings merge. Running gt and git stays with the command.
package rigbootstrap

import (
	"errors"
)

// Usage is the help text for bootstrap-local-rig. It prints for -h and after a
// usage error.
const Usage = `Usage:
  bootstrap-local-rig --town-root PATH --rig NAME --local-repo PATH [options]

Create a clean Gas Town rig from a local source repo by using gt rig add with
--local-repo instead of adopting a manually assembled rig directory.

Required:
  --town-root PATH       Gas Town town root
  --rig NAME             New rig name
  --local-repo PATH      Existing local repo to use as the object reference

Optional:
  --remote URL           Git URL to register for the rig
                         (default: file://<local-repo>)
  --gt-bin PATH          gt binary to use (default: gt on PATH)
  --prefix PREFIX        Beads prefix override
  --branch NAME          Default branch override
  --polecat-agent NAME   Write rig settings role_agents.polecat

Example:
  bootstrap-local-rig \
    --town-root /gt \
    --rig nightrider_local \
    --local-repo /gt/nightRider \
    --prefix nr \
    --polecat-agent claude
`

// ErrHelp reports that -h or --help was given: the command prints Usage to
// stdout and exits 0.
var ErrHelp = errors.New("help requested")

// UsageError is a mistake on the command line: the command prints its message
// and Usage to stderr and exits 2.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

// Options is the parsed command line. Remote stays empty when the flag is not
// given, so the run step can default it from LocalRepo.
type Options struct {
	TownRoot     string
	Rig          string
	LocalRepo    string
	Remote       string
	GTBin        string
	Prefix       string
	Branch       string
	PolecatAgent string
}

// ParseOptions parses args, the command line without the program name, and
// returns the first mistake it finds. A value flag with nothing after it reads
// as the empty string, as the script's ${2:-} did, so a required flag given that
// way reports as missing.
func ParseOptions(args []string) (Options, error) {
	opts := Options{GTBin: "gt"}
	valued := map[string]*string{
		"--town-root":     &opts.TownRoot,
		"--rig":           &opts.Rig,
		"--remote":        &opts.Remote,
		"--local-repo":    &opts.LocalRepo,
		"--gt-bin":        &opts.GTBin,
		"--prefix":        &opts.Prefix,
		"--branch":        &opts.Branch,
		"--polecat-agent": &opts.PolecatAgent,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			return Options{}, ErrHelp
		}
		// The witness and refinery roles are retired, so a value for either
		// would configure nothing. Refuse it rather than accept it silently.
		if arg == "--witness-agent" || arg == "--refinery-agent" {
			return Options{}, &UsageError{Msg: arg + " is not supported: the witness and refinery roles are retired"}
		}
		field, ok := valued[arg]
		if !ok {
			return Options{}, &UsageError{Msg: "Unknown argument: " + arg}
		}
		*field = ""
		if i+1 < len(args) {
			*field = args[i+1]
		}
		i++
	}

	// The order is the script's: a command missing two values names the first.
	required := []struct{ name, value string }{
		{"TOWN_ROOT", opts.TownRoot},
		{"RIG_NAME", opts.Rig},
		{"LOCAL_REPO", opts.LocalRepo},
	}
	for _, r := range required {
		if r.value == "" {
			return Options{}, &UsageError{Msg: "Missing required argument: " + r.name}
		}
	}
	return opts, nil
}
