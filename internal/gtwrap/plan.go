package gtwrap

import (
	"fmt"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// tools are the names the wrapper is installed under, each running the tool of
// the same name inside the container. The list is the whole of what argv[0] may
// select: the name comes from the caller, and without it a symlink under any
// name would run any binary in the image.
var tools = []string{"gt", "bd", "om", "hm"}

// envPrefixes are the variable families that cross into the container.
var envPrefixes = []string{"GT_", "BEADS_", "BD_", "CLAUDE_"}

// envNames are the exact variables that cross beyond those families: the
// terminal variables, so a tool that lays out a screen knows the size it has.
var envNames = []string{"TERM", "COLUMNS", "LINES"}

// credentialWords mark a CLAUDE_ name as a credential whether or not the config
// lists it. KEY also drops the mTLS client key path (CLAUDE_CODE_CLIENT_KEY),
// which is the safe side of the match.
// The rule is set out in docs/design/gt-in-container.md.
var credentialWords = []string{"TOKEN", "SECRET", "PASSWORD", "PASSPHRASE", "KEY"}

// Plan returns the call that runs the tool the wrapper was invoked as, with args
// unchanged and env filtered to the variables that cross, or an error naming the
// four for any other name.
func Plan(name string, args, env []string, cwd string, isTTY bool) (Call, error) {
	tool, err := toolFor(name)
	if err != nil {
		return Call{}, err
	}
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, tool)
	argv = append(argv, args...)
	return Call{Argv: argv, Env: forwardEnv(env), Dir: cwd, TTY: isTTY}, nil
}

// toolFor returns the executable the invoked name selects.
func toolFor(name string) (string, error) {
	for _, tool := range tools {
		if name == tool {
			return tool, nil
		}
	}
	return "", fmt.Errorf("gtwrap: invoked as %q; the wrapper answers to %s", name, toolList())
}

// toolList renders tools for the error: the reader needs all four names, so a
// list kept in one place cannot drift from the check.
func toolList() string {
	return strings.Join(tools[:len(tools)-1], ", ") + " and " + tools[len(tools)-1]
}

// forwardEnv keeps the entries of env that cross and drops the rest. An entry
// with no "=" names no variable, so it cannot match and is dropped rather than
// handed to exec, which would refuse the whole environment.
func forwardEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && forwards(name) {
			out = append(out, entry)
		}
	}
	return out
}

// forwards reports whether the variable named name crosses into the container.
// A credential is refused before the prefix match: CLAUDE_CODE_OAUTH_TOKEN is in
// the CLAUDE_ family, and a forwarded credential would reach the container's argv.
// An unlisted CLAUDE_ name is refused too when its upper-case form contains a
// credentialWords entry. The GT_, BEADS_ and BD_ families are not screened by
// substring: GT_DOLT_PASSWORD must cross.
func forwards(name string) bool {
	if slices.Contains(config.UnforwardedCredentialEnvVars(), name) {
		return false
	}
	if strings.HasPrefix(name, "CLAUDE_") && containsCredentialWord(strings.ToUpper(name)) {
		return false
	}
	for _, prefix := range envPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, exact := range envNames {
		if name == exact {
			return true
		}
	}
	return false
}

// containsCredentialWord reports whether upper, an upper-case variable name,
// contains one of the credentialWords.
func containsCredentialWord(upper string) bool {
	for _, word := range credentialWords {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return false
}
