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
  - echo/printf of a secret-like variable, or a secret-like variable fed to a
    here-string, an unquoted heredoc body, or an argument of curl, wget or http
  - any read of /proc/<pid>/environ
  - reading ~/.claude.json, a .env file, ~/.netrc or ~/.git-credentials
  - git credential fill, and the git credential helper run directly
  - a python/node/ruby/perl/awk one-liner that reads the whole environment, a
    secret-like variable, or a credential file, or passes a shell command that
    prints the environment to a shell-out such as os.system or child_process;
    also when run through uv run, uv tool run, uvx, npx or pipx run
  - jq whose filter is env or $ENV

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
	// sandboxSecretsHereStringRe matches a here-string operator, with an optional file
	// descriptor, and captures the operand glued to it: <<<"$X" tokenizes as <<<$X.
	sandboxSecretsHereStringRe = regexp.MustCompile(`^[0-9]*<<<(.*)$`)
	// sandboxSecretsProcEnvironRe matches a read of any process's environment block.
	sandboxSecretsProcEnvironRe = regexp.MustCompile(`/proc/(?:[^\s;&|'"]|\$\([^)]*\))*/environ`)
	// sandboxSecretsNamesEnvRe finds a command too deeply nested to judge that still
	// names an environment dump.
	sandboxSecretsNamesEnvRe = regexp.MustCompile(`\b(env|printenv|environ)\b`)
	// sandboxSecretsEnvScriptRe matches a one-liner payload's access to the environment. ENV
	// covers ruby's ENV and perl's %ENV and $ENV{}; the from-import binds environ by name.
	sandboxSecretsEnvScriptRe = regexp.MustCompile(`os\.environ|os\.getenv|process\.env|\bENV\b|\bENVIRON\b|\bfrom\s+os\s+import\b[^;\n]*\benviron\b`)
	// sandboxSecretsShellOutRe matches a script that runs a shell command, so its quoted
	// strings are judged as commands.
	sandboxSecretsShellOutRe = regexp.MustCompile("\\b(?:system|popen|exec\\w*|spawn\\w*|subprocess|check_output|getoutput|Open3)\\b|`")
	// sandboxSecretsQuotedRe matches one quoted string in a one-liner.
	sandboxSecretsQuotedRe = regexp.MustCompile("'([^']*)'|\"([^\"]*)\"|`([^`]*)`")
	// sandboxSecretsJQEnvRe matches a jq filter that yields the environment: a bare env
	// word or $ENV, not a key such as .env or {env: 1}.
	sandboxSecretsJQEnvRe = regexp.MustCompile(`\$ENV\b|(?:^|[^\w.$"'])env(?:$|[^\w:"'])`)
	// sandboxSecretsEnvScriptNamedRe matches an access that names one literal variable, such as
	// process.env.NODE_ENV or os.environ['HOME'] or awk's ENVIRON["HOME"]. The name
	// is in whichever of its three groups the form uses; a dynamic key
	// (os.environ[k]) names none.
	sandboxSecretsEnvScriptNamedRe = regexp.MustCompile(`process\.env\.([A-Za-z_]\w*)|(?:os\.environ(?:\.get)?[(\[]|os\.getenv\(|process\.env\[|\bENV(?:\.fetch\(|\[)|\bENVIRON\[)\s*['"]([A-Za-z_]\w*)['"]|\$ENV\{\s*['"]?([A-Za-z_]\w*)`)
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

// sandboxSecretsBodyRefsSecret reports whether the text of an unquoted heredoc
// body expands a secret-like variable. A backslash before the $ makes it literal.
func sandboxSecretsBodyRefsSecret(body string) bool {
	for _, m := range sandboxSecretsVarRefRe.FindAllStringSubmatchIndex(body, -1) {
		if backslashesBefore(body, m[0])%2 == 1 {
			continue
		}
		if sandboxSecretsIsSecretName(body[m[2]:m[3]]) {
			return true
		}
	}
	return false
}

// sandboxSecretsHereStringRefsSecret reports whether a segment feeds a secret-like
// variable to a command's stdin as a here-string: the operand follows the <<<
// token or is glued to it.
func sandboxSecretsHereStringRefsSecret(segment []string) bool {
	for i, tok := range segment {
		m := sandboxSecretsHereStringRe.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		operand := m[1]
		if operand == "" && i+1 < len(segment) {
			operand = segment[i+1]
		}
		if sandboxSecretsRefsSecret(operand) {
			return true
		}
	}
	return false
}

// sandboxSecretsHeredocRefsSecret reports whether a heredoc the shell expands
// carries a secret-like variable into a command's stdin. A quoted tag keeps its
// body literal, and a body fed to a shell is judged as code by the walk.
func sandboxSecretsHeredocRefsSecret(command string) bool {
	for _, span := range scanHeredocs(command) {
		if span.bodyStart < 0 || span.quoted || heredocReaderIsShellInvoker(span.reader) {
			continue
		}
		if sandboxSecretsBodyRefsSecret(span.body) {
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
		case sandboxSecretsIsScriptRunner(name):
			// A script that shells out runs the strings it passes, so each one is judged as a command.
			for _, a := range argv[1:] {
				payloads = append(payloads, sandboxSecretsShelledStrings(a)...)
			}
		case name == "eval" && len(argv) > 1:
			payloads = append(payloads, strings.Join(argv[1:], " "))
		}
	}
	if sandboxSecretsHeredocRefsSecret(command) {
		return "an unquoted heredoc expands a secret variable", false
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
	if sandboxSecretsHereStringRefsSecret(segment) {
		return "a here-string expands a secret variable", nil, nil
	}
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
			if inner, payload, runs := sandboxSecretsPackageRunner(name, seg[1:]); runs {
				if payload != "" {
					payloads = append(payloads, payload)
				}
				seg = inner
				continue
			}
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

// sandboxSecretsRunnerPayloadFlags are the flags whose value is a command line
// the shell runs: npx -c 'env'. Only npx reads them so
// (sandboxSecretsSkipRunnerPayload drops them for npx alone), because uv's -c
// is --constraints, whose value is a file.
var sandboxSecretsRunnerPayloadFlags = map[string]bool{"-c": true, "--call": true}

// sandboxSecretsPackageRunner looks through the package runners that start the
// command behind them, uv run, uv tool run, uvx, npx and pipx run, as
// skipLauncherArgs does for a launcher. runs is false when the word is not a
// runner or its subcommand runs nothing (uv pip install). inner is the command
// the runner starts, from its name on; payload is the command line npx -c
// hands to a shell.
func sandboxSecretsPackageRunner(name string, args []string) (inner []string, payload string, runs bool) {
	switch name {
	case "uv", "pipx":
		// run is the subcommand that starts a command, and uv tool run spells
		// it with tool in front. It is read wherever it stands, not as the
		// first word: the value of a flag ahead of it (uv --directory sub run)
		// looks the same as it does, and no flag table can know every flag's
		// arity, so a first-word reading would hide the command behind any
		// flag the table lists wrongly.
		i := sandboxSecretsWordIndex(args, "run")
		if i < 0 {
			return nil, "", false
		}
		args = args[i+1:]
	case "uvx":
	case "npx":
		args, payload = sandboxSecretsSkipRunnerPayload(args)
	default:
		return nil, "", false
	}
	return sandboxSecretsRunnerCommand(args), payload, true
}

// sandboxSecretsRunnerCommand returns the command a package runner starts, from
// its name on. The runner's own flags and their values stand ahead of it, and
// the arity of a flag not in any table is not knowable: its value looks exactly
// like the command. So the command is the first word that names one this guard
// judges (sandboxSecretsNamesCommand), wherever it stands, rather than the first
// word after the flags. When no word names one, the command is the first word
// that is not a flag, and judging that blocks nothing.
//
// The cost of reading past a value is that a command named only as somebody's
// argument is judged as that command. The guard errs toward judging more: a
// value that hides an interpreter is the failure this function exists to
// prevent, while a word such as pytest's -k pattern costs the agent a block
// message that names the rule and says to escalate.
func sandboxSecretsRunnerCommand(args []string) []string {
	var first []string
	for i, a := range args {
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		if first == nil {
			first = args[i:]
		}
		if sandboxSecretsNamesCommand(a) {
			return append([]string{sandboxSecretsCommandWord(a)}, args[i+1:]...)
		}
	}
	return first
}

// sandboxSecretsCommandWord strips the version a package runner names after the
// command it starts: npx node@20, pipx run black==24.1. An @ inside a path is
// part of the path, not a version.
func sandboxSecretsCommandWord(word string) string {
	if i := strings.Index(word, "=="); i > 0 {
		word = word[:i]
	}
	if i := strings.LastIndex(word, "@"); i > 0 && !strings.Contains(word[i+1:], "/") {
		word = word[:i]
	}
	return word
}

// sandboxSecretsNamesCommand reports whether the word names a command this guard
// judges, or one that runs the command behind it.
func sandboxSecretsNamesCommand(word string) bool {
	name := strings.ToLower(filepath.Base(sandboxSecretsCommandWord(word)))
	switch name {
	case "env", "printenv", "set", "export", "declare", "typeset", "compgen",
		"echo", "printf", "print", "curl", "wget", "http", "https", "jq", "git",
		".githelper", "eval", "uv", "uvx", "npx", "pipx":
		return true
	}
	if sandboxSecretsEnvReaders[name] || sandboxSecretsIsScriptRunner(name) || shellInvokers[name] {
		return true
	}
	_, launcher := prWorkflowLaunchers[name]
	return launcher
}

// sandboxSecretsWordIndex returns the index of the first word in args that is
// exactly word, or -1.
func sandboxSecretsWordIndex(args []string, word string) int {
	for i, a := range args {
		if a == word {
			return i
		}
	}
	return -1
}

// sandboxSecretsSkipRunnerPayload drops npx's -c/--call flag and the command
// line it carries, given as its own word (npx -c 'env') or attached to the flag
// (-cenv, -c=env, --call=env), and returns the words that remain with that
// command line.
func sandboxSecretsSkipRunnerPayload(args []string) (rest []string, payload string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case sandboxSecretsRunnerPayloadFlags[a]:
			if i+1 < len(args) {
				payload = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--call="):
			payload = strings.TrimPrefix(a, "--call=")
		case len(a) > 2 && strings.HasPrefix(a, "-c") && a[1] != '-':
			payload = strings.TrimPrefix(a[2:], "=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, payload
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
	case "curl", "wget", "http", "https":
		// A verbose flag or an echoing server puts a header or body in the output,
		// so any argument that expands a secret counts, not only -H and -d.
		for _, a := range args {
			if sandboxSecretsRefsSecret(a) {
				return name + " sends a secret variable"
			}
		}
	case "jq":
		// The first word that is not a flag is the filter; `jq -n env` prints the environment.
		if f := sandboxSecretsNonFlagArgs(args); len(f) > 0 && sandboxSecretsJQEnvRe.MatchString(f[0]) {
			return "jq prints the environment"
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
	if sandboxSecretsIsScriptRunner(name) {
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

// sandboxSecretsIsScriptRunner reports whether the command runs a script, such as python3.12.
func sandboxSecretsIsScriptRunner(name string) bool {
	return sandboxSecretsScriptRunners[strings.TrimRight(name, "0123456789.")] || sandboxSecretsScriptRunners[name]
}

// sandboxSecretsShelledStrings returns the quoted strings of a script that runs a shell
// command, so the walk judges each as one. A script with no shell-out has none.
func sandboxSecretsShelledStrings(script string) []string {
	if !sandboxSecretsShellOutRe.MatchString(script) {
		return nil
	}
	var strs []string
	for _, m := range sandboxSecretsQuotedRe.FindAllStringSubmatch(script, -1) {
		if s := m[1] + m[2] + m[3]; s != "" { // one of the three groups matched
			strs = append(strs, s)
		}
	}
	return strs
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
