// Package k8spolicy renders the polecat sandbox policy for a Kubernetes
// gateway from the Docker policy (deploy/polecat/policy.yaml), so the two
// cannot drift apart (gt-tpg0p.81). The package holds the command line and the
// input validators (this file and validate.go) and the endpoint rewriter
// (render.go); cmd/render-policy writes the result. It is the port of a shell
// script that gt-tpg0p.84 deleted.
package k8spolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Name is the command's name: the binary, and the prefix of every message it
// prints.
const Name = "render-policy"

// The values a command line that names none carries, the script's own. Both
// names are ExternalName Services the kind cluster defines; the address is
// host.lima.internal as the kind node sees it, and OpenShell refuses a private
// endpoint that does not pin it.
const (
	DefaultDoltHost    = "control-plane.openshell.svc.cluster.local"
	DefaultForgejoHost = "control-plane.openshell.svc.cluster.local"
	DefaultAllowedIP   = "192.168.5.2"
)

// DefaultBasePath is BASE when the command line names none: the Docker policy,
// relative to the repository root.
const DefaultBasePath = "deploy/polecat/policy.yaml"

// Options is a command line the command accepts.
type Options struct {
	// DoltHost names the endpoint on port 3307.
	DoltHost string
	// ForgejoHost names the endpoint on port 3000.
	ForgejoHost string
	// TrackerHost names the endpoint on the tracker's port (DefaultTrackerPort).
	TrackerHost string
	// AllowedIP is the bare address pinned on each rewritten endpoint; the
	// render adds the /32.
	AllowedIP string
	// Base is the policy file a render is produced from.
	Base string
}

// UsageError is a command line the command refuses. The caller prints Msg and
// exits 2: the code the script used both for an unknown option and for a value
// it would not render.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Host is what a run reads from the process: its working directory and its
// filesystem. Production uses ProcessHost; a test passes a scripted host
// instead of chdir-ing or building a tree on disk.
type Host struct {
	// Getwd reports the working directory the repository root is searched from.
	Getwd func() (string, error)
	// Stat reports a path's file information, so the repository-root walk runs
	// against a tree a test scripts rather than one it builds on disk.
	Stat func(string) (fs.FileInfo, error)
	// ReadFile reads BASE.
	ReadFile func(string) ([]byte, error)
}

// ProcessHost reads the running process.
var ProcessHost = Host{Getwd: os.Getwd, Stat: os.Stat, ReadFile: os.ReadFile}

// Parse reads args, the command line after the program name, and resolves the
// default BASE from the repository root when the command line names none. A
// refused command line is a *UsageError, which the caller exits 2 on; a
// repository root the walk cannot find or read is a plain error, exited 1.
func Parse(args []string, h Host) (Options, error) {
	opts := Options{
		DoltHost:    DefaultDoltHost,
		ForgejoHost: DefaultForgejoHost,
		TrackerHost: DefaultTrackerHost,
		AllowedIP:   DefaultAllowedIP,
	}
	namedBase := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dolt-host" || arg == "--forgejo-host" || arg == "--tracker-host" || arg == "--allowed-ip":
			// Whatever follows the flag is its value, another flag included: the
			// script's case took $2 the same way. Only a flag with nothing after
			// it is refused, where the script died on an unbound variable
			// (gt-tpg0p.81).
			if i+1 == len(args) {
				return Options{}, &UsageError{Msg: fmt.Sprintf("%s: %s needs a value", Name, arg)}
			}
			i++
			switch arg {
			case "--dolt-host":
				opts.DoltHost = args[i]
			case "--forgejo-host":
				opts.ForgejoHost = args[i]
			case "--tracker-host":
				opts.TrackerHost = args[i]
			case "--allowed-ip":
				opts.AllowedIP = args[i]
			}
		case strings.HasPrefix(arg, "-"):
			return Options{}, &UsageError{Msg: fmt.Sprintf("%s: unknown option %s", Name, arg)}
		default:
			opts.Base = arg
			namedBase = true
		}
	}
	if !namedBase {
		root, err := repositoryRoot(h)
		if err != nil {
			return Options{}, err
		}
		opts.Base = filepath.Join(root, filepath.FromSlash(DefaultBasePath))
	}
	return opts, nil
}

// ReadBase reads the policy file a render is produced from. The script tested
// readability with -r and reported the path alone, so an operator reads the
// same line whether the file is missing or unreadable.
func ReadBase(h Host, base string) ([]byte, error) {
	text, err := h.ReadFile(base)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read %s", Name, base)
	}
	return text, nil
}

// repositoryRoot is the nearest directory at or above the working directory
// that holds a go.mod: the anchor for the default BASE. The script resolved its
// default as ../polecat/policy.yaml beside itself, so the anchor is the
// checkout rather than the directory a run was started in.
func repositoryRoot(h Host) (string, error) {
	dir, err := h.Getwd()
	if err != nil {
		return "", fmt.Errorf("%s: working directory: %w", Name, err)
	}
	start := dir
	for {
		goMod := filepath.Join(dir, "go.mod")
		_, err := h.Stat(goMod)
		switch {
		case err == nil:
			return dir, nil
		case !errors.Is(err, fs.ErrNotExist):
			// A directory the walk cannot examine is not "no module here": a
			// permission error must not read as a checkout that has no root.
			return "", fmt.Errorf("%s: reading %s: %w", Name, goMod, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s: no go.mod in %s or any parent, so the default BASE cannot be found", Name, start)
		}
		dir = parent
	}
}
