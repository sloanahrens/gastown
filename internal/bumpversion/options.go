// Package bumpversion is the part of bump-version that touches no repository:
// its command line, its version rule and, in the slices after gt-tpg0p.71, the
// version-file text edits. scripts/bump-version.sh is the port's specification.
package bumpversion

import (
	"errors"
	"regexp"
)

// Usage is the help text for a command line that cannot be read. It is the
// script's usage() with the --push lines removed, since --push is not offered.
const Usage = `Usage: bump-version <version> [--commit] [--tag] [--install] [--allow-dirty]

Bump version across all Gas Town components.

Arguments:
  <version>        Semantic version (e.g., 0.2.0, 1.0.0)
  --commit         Automatically create a git commit
  --tag            Create annotated git tag (requires --commit)
  --install        Rebuild and install gt binary to GOPATH/bin
  --allow-dirty    Continue when the tree has uncommitted changes

Note: flake.nix version and vendorHash are updated automatically if nix is available.

Examples:
  bump-version 0.2.0                        # Update versions and show diff
  bump-version 0.2.0 --install              # Update versions and rebuild/install gt
  bump-version 0.2.0 --commit               # Update versions and commit
  bump-version 0.2.0 --commit --tag         # Update, commit, and tag

Recommended release command:
  bump-version X.Y.Z --commit --tag --install
`

// ErrUsage reports a command line with no arguments, the one case where the
// command prints Usage and nothing else.
var ErrUsage = errors.New("no arguments")

// UsageError reports an option the command cannot accept. The command prints
// Msg, then Usage, then exits 1.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Refusal reports a command line the command explains and stops: it prints Msg
// and exits 1 without Usage, because the flags are the subject, not the
// spelling of the command.
type Refusal struct{ Msg string }

func (e *Refusal) Error() string { return e.Msg }

// VersionError reports a version that is not MAJOR.MINOR.PATCH. Its message is
// the two lines the script printed, which is the whole contract an operator
// sees.
type VersionError struct{ Version string }

func (e *VersionError) Error() string {
	return "Invalid version format '" + e.Version + "'\n" +
		"Expected semantic version format: MAJOR.MINOR.PATCH (e.g., 0.2.0)"
}

// versionPattern is the script's ^[0-9]+\.[0-9]+\.[0-9]+$: the anchors are what
// reject a leading v and a fourth component, and Go's $ matches only at the end
// of the text, so a trailing newline is rejected too.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Options is the parsed command line. AllowDirty is the operator's consent to
// run over an unclean tree, which stands in for the script's interactive
// Continue anyway? prompt.
type Options struct {
	Version    string
	Commit     bool
	Tag        bool
	Install    bool
	AllowDirty bool
}

// Parse reads args, the command line without the program name, in the script's
// order: an unknown option first, then the flag dependencies, then the version
// format, with --push refused before any of them. The empty command line
// returns ErrUsage.
func Parse(args []string) (Options, error) {
	if len(args) == 0 {
		return Options{}, ErrUsage
	}
	// The script reads args[0] as the version, which would report a leading
	// --push as a bad version; the refusal names the option wherever it sits.
	for _, arg := range args {
		if arg == "--push" {
			return Options{}, &Refusal{Msg: "--push is not offered: this command never runs git push, because main lands only through the landing worker and the tag is pushed by the release procedure"}
		}
	}
	opts := Options{Version: args[0]}
	for _, arg := range args[1:] {
		switch arg {
		case "--commit":
			opts.Commit = true
		case "--tag":
			opts.Tag = true
		case "--install":
			opts.Install = true
		case "--allow-dirty":
			opts.AllowDirty = true
		default:
			return Options{}, &UsageError{Msg: "Unknown option '" + arg + "'"}
		}
	}
	if opts.Tag && !opts.Commit {
		return Options{}, &Refusal{Msg: "--tag requires --commit"}
	}
	if err := ValidateVersion(opts.Version); err != nil {
		return Options{}, err
	}
	return opts, nil
}

// ValidateVersion reports why version is not three dot-separated numbers, or
// nil when it is.
func ValidateVersion(version string) error {
	if versionPattern.MatchString(version) {
		return nil
	}
	return &VersionError{Version: version}
}
