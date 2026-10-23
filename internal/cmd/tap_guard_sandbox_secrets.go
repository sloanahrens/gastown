package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

var tapGuardSandboxSecretsCmd = &cobra.Command{
	Use:   "sandbox-secrets",
	Short: "In a sandbox, block commands that print the environment or secret values",
	Long: `Block commands that would print the environment or a secret, in a sandbox
session only (GT_SESSION_BACKEND=openshell). On a host it does nothing.

Blocked, in any chain, launcher, shell -c payload, eval or command substitution:
  - env, printenv, set, export, declare/typeset and compgen, when they would list
    the environment, or printenv of a secret-like NAME
  - echo/printf of a secret-like variable
  - any read of /proc/<pid>/environ
  - reading ~/.claude.json, a .env file, ~/.netrc or ~/.git-credentials
  - git credential fill, and the git credential helper run directly
  - a python/node/ruby/perl/awk one-liner that reads the whole environment, a
    secret-like variable, or a credential file

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardSandboxSecrets,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardSandboxSecretsCmd)
}

func runTapGuardSandboxSecrets(cmd *cobra.Command, args []string) error {
	return tapGuardSandboxSecrets(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardSandboxSecrets reads the hook payload from stdin. A guard that cannot
// read the command cannot judge it, as with the other tap guards. A payload with
// no command in it has nothing to judge.
func tapGuardSandboxSecrets(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	if proc.getenv("GT_SESSION_BACKEND") != "openshell" {
		return nil
	}
	input, err := io.ReadAll(stdin)
	if err != nil {
		return nil
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}
	if reason := sandboxSecretsReason(command); reason != "" {
		printSandboxSecretsBlock(stderr, reason)
		return NewSilentExit(2)
	}
	return nil
}

var (
	// sandboxSecretsVarRefRe finds the variable names a word expands: $NAME, ${NAME}.
	sandboxSecretsVarRefRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	// sandboxSecretsProcEnvironRe matches a read of any process's environment block.
	sandboxSecretsProcEnvironRe = regexp.MustCompile(`/proc/(?:[^\s;&|'"]|\$\([^)]*\))*/environ`)
	// sandboxSecretsNamesEnvRe finds a command too deeply nested to judge that still
	// names an environment dump.
	sandboxSecretsNamesEnvRe = regexp.MustCompile(`\b(env|printenv|environ)\b`)
	// sandboxSecretsEnvScriptRe matches a one-liner payload's access to the environment.
	sandboxSecretsEnvScriptRe = regexp.MustCompile(`os\.environ|os\.getenv|process\.env|ENV\[|\$ENV\{|\bENVIRON\b`)
	// sandboxSecretsEnvScriptNamedRe matches an access that names one literal variable, such as
	// process.env.NODE_ENV or os.environ['HOME'] or awk's ENVIRON["HOME"]. The name
	// is in whichever of its three groups the form uses; a dynamic key
	// (os.environ[k]) names none.
	sandboxSecretsEnvScriptNamedRe = regexp.MustCompile(`process\.env\.([A-Za-z_]\w*)|(?:os\.environ(?:\.get)?[(\[]|os\.getenv\(|process\.env\[|\bENV\[|\bENVIRON\[)\s*['"]([A-Za-z_]\w*)['"]|\$ENV\{\s*['"]?([A-Za-z_]\w*)`)
	// sandboxSecretsCredentialPathRe matches a one-liner payload naming a credential file.
	sandboxSecretsCredentialPathRe = regexp.MustCompile(`\.claude\.json|\.netrc|git-credentials`)
)

// sandboxSecretsNameWords are the words, between underscores, that make a variable name
// secret-like: FORGEJO_TOKEN, DEEPSEEK_API_KEY, DB_PASSWORD. Whole words only, so
// GIT_AUTHOR_NAME and MONKEY are not secrets.
var sandboxSecretsNameWords = map[string]bool{
	"TOKEN": true, "KEY": true, "SECRET": true, "PASSWORD": true, "PASSWD": true,
	"CRED": true, "CREDS": true, "CREDENTIAL": true, "CREDENTIALS": true, "APIKEY": true,
}

func sandboxSecretsIsSecretName(name string) bool {
	for _, w := range strings.Split(strings.ToUpper(name), "_") {
		if sandboxSecretsNameWords[w] {
			return true
		}
	}
	return false
}

// sandboxSecretsRefsSecret reports whether the word expands a secret-like variable.
func sandboxSecretsRefsSecret(word string) bool {
	for _, m := range sandboxSecretsVarRefRe.FindAllStringSubmatch(word, -1) {
		if sandboxSecretsIsSecretName(m[1]) {
			return true
		}
	}
	return false
}

// sandboxSecretsEnvReaders are the commands that print a file's contents.
var sandboxSecretsEnvReaders = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true, "bat": true,
	"jq": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "awk": true,
	"sed": true, "strings": true, "xxd": true, "od": true, "cut": true, "tr": true, "nl": true,
	"base64": true, "sort": true, "uniq": true, "fold": true, "rev": true, "column": true,
	"zcat": true,
}

var sandboxSecretsScriptRunners = map[string]bool{
	"python": true, "python3": true, "node": true, "ruby": true, "perl": true,
	"awk": true, "gawk": true, "mawk": true,
}

// sandboxSecretsReason names why the command would print the environment or a
// secret, or "" when it would not.
func sandboxSecretsReason(command string) string {
	if sandboxSecretsProcEnvironRe.MatchString(command) {
		return "it reads a process's environment block"
	}
	reason, tooDeep := sandboxSecretsWalk(command, 0)
	if reason == "" && tooDeep && sandboxSecretsNamesEnvRe.MatchString(command) {
		return "it nests commands too deeply to check and names the environment"
	}
	return reason
}

// sandboxSecretsWalk judges every command a line runs: each segment, and the
// payloads a shell runs (a shell -c string, an eval, a heredoc fed to a shell, a
// command substitution). It walks the way prWorkflowInvocations does but judges
// each segment itself, because that function reads past a launcher, and a bare
// `env` is a launcher with no command behind it: it prints the environment.
func sandboxSecretsWalk(command string, depth int) (reason string, tooDeep bool) {
	tokens := shellTokenize(prWorkflowTokenText(command))
	var payloads []string
	for _, segment := range splitShellSegments(tokens) {
		r, launched, argv := sandboxSecretsSegment(segment)
		if r != "" {
			return r, false
		}
		payloads = append(payloads, launched...)
		if len(argv) == 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(argv[0]))
		switch {
		case shellInvokers[name]:
			if idx := shellCPayloadIndex(toLowerTokens(argv[1:])); idx >= 0 {
				payloads = append(payloads, argv[1+idx])
			}
		case name == "eval" && len(argv) > 1:
			payloads = append(payloads, strings.Join(argv[1:], " "))
		}
	}
	payloads = append(payloads, shellFedHeredocBodies(command)...)
	payloads = append(payloads, prWorkflowSubstitutions(command)...)
	for _, payload := range payloads {
		if depth >= maxPRWorkflowNestDepth {
			tooDeep = true
			continue
		}
		r, deep := sandboxSecretsWalk(payload, depth+1)
		if r != "" {
			return r, false
		}
		tooDeep = tooDeep || deep
	}
	return "", tooDeep
}

// sandboxSecretsSegment looks through the assignments and launchers ahead of a
// segment's command, as prWorkflowCommand does, but stops at an `env` that has
// no command behind it. It returns the verdict on the command it finds, the
// payloads a launcher carries, and that command's argv.
func sandboxSecretsSegment(segment []string) (reason string, payloads, argv []string) {
	seg := trimShellKeywords(segment)
	for len(seg) > 0 {
		word := seg[0]
		if isEnvAssignment(word) || word == "!" || word == prWorkflowSubstitutionWord {
			seg = seg[1:]
			continue
		}
		name := strings.ToLower(filepath.Base(word))
		launcher, ok := prWorkflowLaunchers[name]
		if !ok {
			return sandboxSecretsInvocation(seg), payloads, seg
		}
		if name == "env" && sandboxSecretsEnvWouldPrint(seg[1:]) {
			return "env prints the environment", nil, nil
		}
		var payload string
		seg, payload = skipLauncherArgs(name, launcher, seg[1:])
		if payload != "" {
			payloads = append(payloads, payload)
		}
	}
	return "", payloads, nil
}

func sandboxSecretsInvocation(argv []string) string {
	name := strings.ToLower(filepath.Base(argv[0]))
	args := argv[1:]
	switch name {
	case "env":
		if sandboxSecretsEnvWouldPrint(args) {
			return "env prints the environment"
		}
	case "printenv":
		// printenv -0 with no name prints the whole environment, so only names count.
		names := sandboxSecretsNonFlagArgs(args)
		if len(names) == 0 {
			return "printenv prints the environment"
		}
		for _, a := range names {
			if sandboxSecretsIsSecretName(a) {
				return "printenv " + a + " prints a secret"
			}
		}
	case "set":
		if len(args) == 0 {
			return "set prints every shell variable"
		}
	case "export":
		if len(args) == 0 || sandboxSecretsHasAnyFlag(args, "-p") {
			return "export lists the environment"
		}
	case "declare", "typeset":
		if len(args) == 0 || (sandboxSecretsHasAnyFlag(args, "-p", "-x") && !sandboxSecretsHasAssignment(args)) {
			return name + " lists variables"
		}
	case "compgen":
		if sandboxSecretsHasAnyFlag(args, "-e", "-v") {
			return "compgen lists variable names"
		}
	case "echo", "printf", "print":
		for _, a := range args {
			if sandboxSecretsRefsSecret(a) {
				return name + " of a secret variable"
			}
		}
	case "git":
		if sandboxSecretsIsCredentialFill(args) {
			return "git credential fill prints a password"
		}
	case ".githelper":
		return "the git credential helper prints a password"
	}
	if sandboxSecretsEnvReaders[name] {
		for _, a := range args {
			if sandboxSecretsIsClaudeJSONPath(a) || sandboxSecretsIsCredentialFile(a) {
				return "it reads " + a + ", which holds credentials or account data"
			}
		}
	}
	if sandboxSecretsScriptRunners[strings.TrimRight(name, "0123456789.")] || sandboxSecretsScriptRunners[name] {
		for _, a := range args {
			if sandboxSecretsReadsEnvironment(a) {
				return "a " + name + " one-liner reads the environment"
			}
			if sandboxSecretsCredentialPathRe.MatchString(a) {
				return "a " + name + " one-liner reads a credential file"
			}
		}
	}
	return ""
}

// sandboxSecretsReadsEnvironment reports whether a one-liner reads the whole
// environment, or a secret-like variable out of it. Naming one harmless variable
// (process.env.NODE_ENV) is allowed, as printenv HOME is.
func sandboxSecretsReadsEnvironment(script string) bool {
	accesses := sandboxSecretsEnvScriptRe.FindAllString(script, -1)
	if len(accesses) == 0 {
		return false
	}
	named := sandboxSecretsEnvScriptNamedRe.FindAllStringSubmatch(script, -1)
	if len(named) < len(accesses) {
		return true // at least one access names no variable: the whole environment
	}
	for _, m := range named {
		if sandboxSecretsIsSecretName(m[1] + m[2] + m[3]) { // one of the three is set
			return true
		}
	}
	return false
}

// sandboxSecretsIsCredentialFill reports whether the git command is `credential fill`, which
// runs the credential helper and prints the password it returns. approve and
// reject store or drop a credential and print nothing.
func sandboxSecretsIsCredentialFill(args []string) bool {
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		default:
			words = append(words, a)
		}
	}
	return len(words) >= 2 && words[0] == "credential" && words[1] == "fill"
}

// sandboxSecretsNonFlagArgs returns the words of args that do not start with a dash.
func sandboxSecretsNonFlagArgs(args []string) []string {
	var names []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			names = append(names, a)
		}
	}
	return names
}

// sandboxSecretsIsCredentialFile reports whether the word names a file that holds secrets: a
// dotenv file (deepseek.env, .env.local), ~/.netrc or ~/.git-credentials.
func sandboxSecretsIsCredentialFile(word string) bool {
	base := strings.ToLower(filepath.Base(strings.Trim(word, `"'`)))
	return base == ".env" || base == ".netrc" || base == ".git-credentials" ||
		strings.HasSuffix(base, ".env") || strings.HasPrefix(base, ".env.")
}

// sandboxSecretsEnvWouldPrint reports whether `env <args>` ends without a command to run: only
// options and NAME=value words, so it prints the environment it built.
func sandboxSecretsEnvWouldPrint(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "-C" || a == "-S" || a == "--unset" || a == "--chdir":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "="):
		default:
			return false // a command follows
		}
	}
	return true
}

// sandboxSecretsHasAnyFlag reports whether args carry one of the flags. A short
// flag also counts inside a cluster of letters (-ix carries -x); a value or a
// path that merely contains the letter does not.
func sandboxSecretsHasAnyFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || (sandboxSecretsIsShortCluster(a) && strings.Contains(a[1:], f[1:])) {
				return true
			}
		}
	}
	return false
}

// sandboxSecretsIsShortCluster reports whether the word is a dash followed by
// two or more letters, such as -ix. A long flag, a word with a value, or a path
// is not a cluster.
func sandboxSecretsIsShortCluster(word string) bool {
	if len(word) < 3 || word[0] != '-' {
		return false
	}
	for _, r := range word[1:] {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func sandboxSecretsHasAssignment(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && strings.Contains(a, "=") {
			return true
		}
	}
	return false
}

func sandboxSecretsIsClaudeJSONPath(word string) bool {
	return filepath.Base(strings.Trim(word, `"'`)) == ".claude.json"
}

func printSandboxSecretsBlock(w io.Writer, reason string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "BLOCKED: this command would print the environment or a secret ("+reason+").")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "In a sandbox your credentials are placeholders. If one reaches this conversation,")
	fmt.Fprintln(w, "the proxy refuses every later request and the session cannot continue.")
	fmt.Fprintln(w, "If git or bd fails to authenticate, say so in your notes and escalate with")
	fmt.Fprintln(w, "'gt escalate'; do not look for the credential. To check your setup use")
	fmt.Fprintln(w, "'gt prime', 'bd where', 'git config --list' and 'git remote -v': none prints a secret.")
	fmt.Fprintln(w, "")
}
