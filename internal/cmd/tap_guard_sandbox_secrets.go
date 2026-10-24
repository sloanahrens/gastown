package cmd

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
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
  - a shell, or a source or ., whose stdin a pipe or a here-string feeds the
    script it runs: bash <<< printenv, source /dev/stdin <<< printenv
  - curl, wget or http sending a credential file (-d @file, -K, --post-file), or
    an argument that expands a variable assigned a secret, or a ${!name} indirection
  - any read of /proc/<pid>/environ
  - reading ~/.claude.json, a .env file, ~/.netrc or ~/.git-credentials
  - git credential fill, and the git credential helper run directly
  - a python/node/ruby/perl/awk one-liner, or a heredoc body that one runs as
    code, that reads the whole environment, a secret-like variable, or a
    credential file, or passes a shell command that prints the environment to
    a shell-out such as os.system or child_process; also when run through uv
    run, uv tool run, uvx, npx or pipx run
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
	// sandboxSecretsVarRefAtRe matches a variable reference at the start of the text it is given.
	sandboxSecretsVarRefAtRe = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
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
	// strings are judged as commands. It is a keyword match: the spellings it misses are
	// listed in TestSandboxSecretsShellOutLimits.
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
// secret-like beyond config.IsSecretKey: CACHE_KEY, GH_CREDENTIALS. Whole words
// only, so GIT_AUTHOR_NAME and MONKEY are not secrets.
var sandboxSecretsNameWords = map[string]bool{
	"KEY": true, "PASSWD": true, "CRED": true, "CREDS": true, "CREDENTIAL": true, "CREDENTIALS": true,
}

// sandboxSecretsIsSecretName reports whether a variable name is secret-like. The markers
// config uses to redact a credential decide first, so the guard never reads a name as
// harmless that config would redact.
func sandboxSecretsIsSecretName(name string) bool {
	if config.IsSecretKey(name) {
		return true
	}
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
		if span.bodyStart < 0 || span.quoted || sandboxSecretsHeredocFedToShell(command, span) {
			continue
		}
		if sandboxSecretsBodyRefsSecret(span.body) {
			return true
		}
	}
	return false
}

// sandboxSecretsOperatorMark stands in for a heredoc's operator while the line
// it sits on is tokenized, so the segment that the body is fed to can be found.
const sandboxSecretsOperatorMark = "sandboxsecretsheredocoperator"

// sandboxSecretsHeredocFedToShell reports whether a shell invoker in the heredoc's
// own pipeline runs its body: "cat <<EOF | bash". A shell invoker after a ; or &&
// on the same line reads no body from this heredoc, so it does not exempt it.
func sandboxSecretsHeredocFedToShell(command string, span heredocSpan) bool {
	for _, argv := range sandboxSecretsHeredocPipeline(command, span) {
		if shellInvokers[strings.ToLower(filepath.Base(argv[0]))] {
			return true
		}
	}
	return false
}

// sandboxSecretsHeredocFedToSource reports whether source or . in the heredoc's own
// pipeline runs its body: "source /dev/stdin <<EOF". A shell reads the body as a script
// there, so the guard judges it as one.
func sandboxSecretsHeredocFedToSource(command string, span heredocSpan) bool {
	for _, argv := range sandboxSecretsHeredocPipeline(command, span) {
		if name := strings.ToLower(filepath.Base(argv[0])); name == "source" || name == "." {
			return true
		}
	}
	return false
}

// sandboxSecretsHeredocInterpreter returns the script interpreter in the heredoc's
// own pipeline that runs its body: "python3 <<EOF", "uv run python3 <<EOF", or
// "cat <<EOF | node". It is "" when no interpreter runs the body, which includes an
// interpreter that runs a script file: the body is then that file's stdin, not code.
func sandboxSecretsHeredocInterpreter(command string, span heredocSpan) string {
	for _, argv := range sandboxSecretsHeredocPipeline(command, span) {
		name := strings.ToLower(filepath.Base(argv[0]))
		if sandboxSecretsIsScriptRunner(name) && !sandboxSecretsRunsScriptFile(name, argv[1:]) {
			return name
		}
	}
	return ""
}

// sandboxSecretsHeredocPipeline returns the argv of each command in the pipeline that
// holds the heredoc's operator, so a caller can see what runs the body and with what.
func sandboxSecretsHeredocPipeline(command string, span heredocSpan) [][]string {
	lineEnd := len(command)
	if i := strings.IndexByte(command[span.opEnd:], '\n'); i >= 0 {
		lineEnd = span.opEnd + i
	}
	line := command[span.readerStart:span.opStart] + " " + sandboxSecretsOperatorMark + " " + command[span.opEnd:lineEnd]
	return sandboxSecretsPipelineArgvs(shellTokenize(strings.ReplaceAll(line, "\\\n", " ")))
}

// sandboxSecretsPipelineArgvs returns the argv of each command in the pipeline that
// holds the operator mark, the run of segments joined by | with it. Each argv starts at
// the command its segment runs, past the assignments, launchers and package runners
// ahead of it, so "uv run python3 <<EOF" yields python3. It is nil when the tokens
// hold no mark.
func sandboxSecretsPipelineArgvs(tokens []string) [][]string {
	var (
		segments [][]string
		piped    []bool // piped[i]: segments[i] reads the output of the segment before it
		current  []string
		joined   bool
		mark     = -1
	)
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			segments = append(segments, current)
			piped = append(piped, joined)
			current = nil
			joined = tok == "|"
			continue
		}
		if tok == sandboxSecretsOperatorMark {
			mark = len(segments)
			continue
		}
		current = append(current, tok)
	}
	segments = append(segments, current)
	piped = append(piped, joined)
	if mark < 0 {
		return nil
	}
	first, last := mark, mark
	for first > 0 && piped[first] {
		first--
	}
	for last+1 < len(segments) && piped[last+1] {
		last++
	}
	var argvs [][]string
	for _, segment := range segments[first : last+1] {
		if _, _, argv := sandboxSecretsSegment(segment, map[string]bool{}, map[string]bool{}); len(argv) > 0 {
			argvs = append(argvs, argv)
		}
	}
	return argvs
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

// sandboxSecretsScriptExts are the source extensions of the runners that take a script
// file, keyed by the runner's name without its version.
var sandboxSecretsScriptExts = map[string][]string{
	"python": {".py"}, "node": {".js", ".mjs", ".cjs"}, "perl": {".pl"}, "ruby": {".rb"},
}

// sandboxSecretsRunsScriptFile reports whether an interpreter's arguments name a script
// file: a word with the runner's source extension. The file runs in place of the
// heredoc, which is then its stdin. A flag or a - is not a file, so python3 -u <<EOF
// and python3 - <<EOF still run the body as code.
func sandboxSecretsRunsScriptFile(name string, args []string) bool {
	exts := sandboxSecretsScriptExts[strings.TrimRight(name, "0123456789.")]
	for _, arg := range args {
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToLower(arg), ext) {
				return true
			}
		}
	}
	return false
}

// sandboxSecretsReason names why the command would print the environment or a
// secret, or "" when it would not.
func sandboxSecretsReason(command string) string {
	if sandboxSecretsProcEnvironRe.MatchString(sandboxSecretsReadableText(command)) {
		return "it reads a process's environment block"
	}
	reason, tooDeep := sandboxSecretsWalk(command, 0, nil)
	if reason == "" && tooDeep && sandboxSecretsNamesEnvRe.MatchString(command) {
		return "it nests commands too deeply to check and names the environment"
	}
	return reason
}

// sandboxSecretsWalk judges every command a line runs: each segment, and the
// payloads a shell runs (a shell -c string, an eval, a heredoc or here-string
// fed to a shell, a command substitution). It walks the way prWorkflowInvocations
// does but judges each segment itself, because that function reads past a
// launcher, and a bare `env` is a launcher with no command behind it: it prints
// the environment.
//
// inherited names the variables a shell that runs this command already holds a
// secret in. The walk starts from them and taints more as it passes each segment,
// so an argument is judged by the assignments before it.
func sandboxSecretsWalk(command string, depth int, inherited map[string]bool) (reason string, tooDeep bool) {
	tokens := shellTokenize(prWorkflowTokenText(command))
	tainted := maps.Clone(inherited)
	if tainted == nil {
		tainted = map[string]bool{}
	}
	expanded := sandboxSecretsExpandedNames(command)
	var payloads []string
	segments, piped := sandboxSecretsPipeSegments(tokens)
	var stdin string // the text a pipe hands the segment being judged
	shells := false  // whether the line runs a shell or eval, which reads a substitution's output as a script
	for i, segment := range segments {
		r, launched, argv := sandboxSecretsSegment(segment, tainted, expanded)
		if r != "" {
			return r, false
		}
		sandboxSecretsTaint(segment, tainted)
		payloads = append(payloads, launched...)
		fed := stdin
		stdin = ""
		if i+1 < len(segments) && piped[i+1] {
			stdin = sandboxSecretsPipeOutput(argv, fed)
		}
		if len(argv) == 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(argv[0]))
		switch {
		case shellInvokers[name]:
			shells = true
			if idx := shellCPayloadIndex(toLowerTokens(argv[1:])); idx >= 0 {
				payloads = append(payloads, argv[1+idx])
			}
			// A here-string feeds the shell's stdin, which it runs as a script
			// the way it runs a pipe's text: bash <<< printenv (gt-ma5e6).
			if _, texts := splitHereStrings(argv[1:]); len(texts) > 0 {
				payloads = append(payloads, texts...)
			}
			if fed != "" {
				payloads = append(payloads, fed)
			}
		case sandboxSecretsIsScriptRunner(name):
			// A script that shells out runs the strings it passes, so each one is judged as a command.
			shells = true
			for _, a := range argv[1:] {
				payloads = append(payloads, sandboxSecretsShelledStrings(a)...)
			}
		case name == "eval" && len(argv) > 1:
			// eval runs its argument as a script, and reads a substitution's output the way a shell does.
			shells = true
			payloads = append(payloads, strings.Join(argv[1:], " "))
		case name == "source" || name == ".":
			// source and . run the file they name as a script, so a substitution's output is one, as sh <(...) is.
			shells = true
			if fed != "" {
				// A pipe into source reads its script from /dev/stdin, as a pipe into a shell does.
				payloads = append(payloads, fed)
			}
			// A here-string reaches the same stdin, so its text is judged as a
			// script too: source /dev/stdin <<< printenv (gt-ma5e6).
			if _, texts := splitHereStrings(argv[1:]); len(texts) > 0 {
				payloads = append(payloads, texts...)
			}
		}
	}
	if sandboxSecretsHeredocRefsSecret(command) {
		return "an unquoted heredoc expands a secret variable", false
	}
	// A body an interpreter runs is code whatever its tag: the quotes only keep
	// the shell from expanding it. So it gets the one-liner rules, as a -c string would.
	for _, span := range scanHeredocs(command) {
		if span.bodyStart >= 0 && sandboxSecretsHeredocFedToSource(command, span) {
			// source /dev/stdin <<EOF runs the body as a script, so it is judged as shell text.
			payloads = append(payloads, span.body)
		}
		interpreter := sandboxSecretsHeredocInterpreter(command, span)
		if span.bodyStart < 0 || interpreter == "" {
			continue
		}
		if reason := sandboxSecretsScriptReason(span.body); reason != "" {
			return interpreter + " heredoc " + reason, false
		}
		payloads = append(payloads, sandboxSecretsShelledStrings(span.body)...)
	}
	payloads = append(payloads, shellFedHeredocBodies(command)...)
	substitutions := prWorkflowSubstitutions(command)
	payloads = append(payloads, substitutions...)
	if shells {
		// A shell reads a substitution's output as a script too: sh <(echo printenv).
		for _, body := range substitutions {
			if text := sandboxSecretsStdoutText(body); text != "" {
				payloads = append(payloads, text)
			}
		}
	}
	// A substitution's heredoc output runs as a command whether or not a shell reads it
	// here: "$(cat <<'B' ... B)" runs the body it prints.
	payloads = append(payloads, sandboxSecretsSubstitutionHeredocs(command)...)
	for _, payload := range payloads {
		if depth >= maxPRWorkflowNestDepth {
			tooDeep = true
			continue
		}
		r, deep := sandboxSecretsWalk(payload, depth+1, tainted)
		if r != "" {
			return r, false
		}
		tooDeep = tooDeep || deep
	}
	return "", tooDeep
}

// sandboxSecretsBodyPassers are the readers that write a heredoc body back to their
// output, so a substitution's output carries the body it read.
var sandboxSecretsBodyPassers = map[string]bool{"cat": true, "tee": true}

// sandboxSecretsSubstitutionHeredocs returns the bodies of heredocs that a command
// substitution reads through a body passer such as cat or tee, which writes them back
// out. The substitution's output is what a shell or a command word runs:
// printf '%s' "$(cat <<'BEAD' ...)" | sh. The heredoc is found in the substitution's
// own body, because scanHeredocs does not see an operator written inside double
// quotes (gt-ynkew).
func sandboxSecretsSubstitutionHeredocs(command string) []string {
	var bodies []string
	for _, sub := range substitutionSpansIn(command, true) {
		text := substitutionBody(command, sub)
		for _, span := range scanHeredocs(text) {
			if span.bodyStart < 0 || span.unterminated {
				continue
			}
			for _, argv := range sandboxSecretsHeredocPipeline(text, span) {
				if sandboxSecretsBodyPassers[strings.ToLower(filepath.Base(argv[0]))] {
					bodies = append(bodies, span.body)
					break
				}
			}
		}
	}
	return bodies
}

// sandboxSecretsSegment looks through the assignments and launchers ahead of a
// segment's command, as prWorkflowCommand does, but stops at an `env` that has
// no command behind it. It returns the verdict on the command it finds, the
// payloads a launcher carries, and that command's argv. tainted names the
// variables the whole command assigns a secret to.
func sandboxSecretsSegment(segment []string, tainted, expanded map[string]bool) (reason string, payloads, argv []string) {
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
			return sandboxSecretsInvocation(seg, tainted, expanded), payloads, seg
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

func sandboxSecretsInvocation(argv []string, tainted, expanded map[string]bool) string {
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
			if sandboxSecretsExpandsSecret(a, expanded) {
				return name + " of a secret variable"
			}
		}
	case "curl", "wget", "http", "https":
		// A verbose flag or an echoing server puts a header or body in the output,
		// so any argument that expands a secret counts, not only -H and -d. So does
		// one that expands a variable assigned a secret, or names it indirectly.
		for _, a := range args {
			if sandboxSecretsRefsSecret(a) || sandboxSecretsRefsTainted(a, tainted) {
				return name + " sends a secret variable"
			}
			if strings.Contains(a, "${!") {
				return name + " expands a variable named by another, which the guard cannot read"
			}
		}
		if file := sandboxSecretsSentFile(args); file != "" {
			return name + " sends " + file + ", which holds credentials or account data"
		}
	case "jq":
		// `jq -n env` prints the environment, so the filter is judged, whatever options precede it.
		filter := sandboxSecretsJQFilterIndex(args)
		if filter >= 0 && sandboxSecretsJQEnvRe.MatchString(args[filter]) {
			return "jq prints the environment"
		}
		// The filter is a program, not a file: `jq .env config.json` reads config.json only.
		for i, a := range args {
			if i != filter && sandboxSecretsIsReadableSecretFile(a) {
				return "it reads " + a + ", which holds credentials or account data"
			}
		}
		return ""
	case "git":
		if sandboxSecretsIsCredentialFill(args) {
			return "git credential fill prints a password"
		}
	case ".githelper":
		return "the git credential helper prints a password"
	}
	if sandboxSecretsEnvReaders[name] {
		files := args
		if sandboxSecretsPatternReaders[name] {
			files = sandboxSecretsSearchFiles(name, args)
		}
		for _, a := range files {
			if sandboxSecretsIsReadableSecretFile(a) {
				return "it reads " + a + ", which holds credentials or account data"
			}
		}
	}
	if sandboxSecretsIsScriptRunner(name) {
		for _, a := range args {
			if reason := sandboxSecretsScriptReason(a); reason != "" {
				return "a " + name + " one-liner " + reason
			}
		}
	}
	return ""
}

// sandboxSecretsScriptReason names what a script an interpreter runs would read, the
// whole environment, a secret-like variable or a credential file, or "" when it reads
// none. A -c one-liner and a heredoc body are both scripts, so both are judged here.
func sandboxSecretsScriptReason(script string) string {
	if sandboxSecretsReadsEnvironment(script) {
		return "reads the environment"
	}
	if sandboxSecretsCredentialPathRe.MatchString(script) {
		return "reads a credential file"
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

// sandboxSecretsJQValueOptions are the jq options that take the words after them
// as values, mapped to how many: -L a library directory, --indent a width, and
// the --arg family a name and a value (or a file). The filter is the first word
// past them.
var sandboxSecretsJQValueOptions = map[string]int{
	"-L": 1, "--indent": 1,
	"--arg": 2, "--argjson": 2, "--slurpfile": 2, "--rawfile": 2,
}

// sandboxSecretsJQFilterIndex returns the index of the filter jq runs: the first
// word that is neither an option nor one of an option's values, or -1 when none is.
func sandboxSecretsJQFilterIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if n, takesValues := sandboxSecretsJQValueOptions[a]; takesValues {
			i += n
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
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

// sandboxSecretsPatternReaders are the search commands whose first operand is a
// pattern, not a file, unless -e, --regexp, -f or --file names the patterns.
var sandboxSecretsPatternReaders = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true}

// sandboxSecretsSearchValueKind is what the value of a search command's option looks
// like, which decides whether the word after it can be skipped.
type sandboxSecretsSearchValueKind int

const (
	sandboxSecretsValueNone   sandboxSecretsSearchValueKind = iota
	sandboxSecretsValueCount                                // a number: -A, -B, -C, -m, --max-depth
	sandboxSecretsValueAction                               // grep's -d and -D: read, skip or recurse
	sandboxSecretsValueWord                                 // any word: a glob, a type or a label
)

// sandboxSecretsSearchValueFlags are the options whose value is never a file, per
// command, and what that value looks like. A flag is listed only for the command
// that defines it: -g and -t are ripgrep's, --exclude and --include are grep's.
// Skipping a word for a flag the command does not define skips the word that is
// there the pattern or a file it opens (grep -e PATTERN -C .env).
var sandboxSecretsSearchValueFlags = map[string]map[string]sandboxSecretsSearchValueKind{
	"grep": {
		"-A":               sandboxSecretsValueCount,
		"-B":               sandboxSecretsValueCount,
		"-C":               sandboxSecretsValueCount,
		"-m":               sandboxSecretsValueCount,
		"--after-context":  sandboxSecretsValueCount,
		"--before-context": sandboxSecretsValueCount,
		"--max-count":      sandboxSecretsValueCount,
		"-d":               sandboxSecretsValueAction,
		"-D":               sandboxSecretsValueAction,
		"--exclude":        sandboxSecretsValueWord,
		"--exclude-dir":    sandboxSecretsValueWord,
		"--include":        sandboxSecretsValueWord,
		"--label":          sandboxSecretsValueWord,
	},
	"rg": {
		"-A":               sandboxSecretsValueCount,
		"-B":               sandboxSecretsValueCount,
		"-C":               sandboxSecretsValueCount,
		"-d":               sandboxSecretsValueCount,
		"-m":               sandboxSecretsValueCount,
		"--max-depth":      sandboxSecretsValueCount,
		"--after-context":  sandboxSecretsValueCount,
		"--before-context": sandboxSecretsValueCount,
		"--max-count":      sandboxSecretsValueCount,
		"-g":               sandboxSecretsValueWord,
		"--glob":           sandboxSecretsValueWord,
		"--iglob":          sandboxSecretsValueWord,
		"-t":               sandboxSecretsValueWord,
		"-T":               sandboxSecretsValueWord,
		"--type":           sandboxSecretsValueWord,
		"--type-not":       sandboxSecretsValueWord,
	},
}

// sandboxSecretsSearchFlagFamily names the flag table each search command reads:
// egrep and fgrep are grep.
var sandboxSecretsSearchFlagFamily = map[string]string{
	"grep": "grep", "egrep": "grep", "fgrep": "grep", "rg": "rg",
}

// sandboxSecretsSearchActions are the words grep's -d and -D take as a value.
var sandboxSecretsSearchActions = map[string]bool{"read": true, "skip": true, "recurse": true}

// sandboxSecretsSearchTakesValue reports whether the word after args[i] is that
// option's value, so the walk skips it, and false when it is the pattern or a file.
// The word must look like the value the option takes: a number for a count option,
// an action word for grep's -d and -D. A word that does not is judged.
func sandboxSecretsSearchTakesValue(tool string, args []string, i int) bool {
	kind := sandboxSecretsSearchValueFlags[sandboxSecretsSearchFlagFamily[tool]][args[i]]
	if kind == sandboxSecretsValueNone || i+1 >= len(args) {
		return false
	}
	switch kind {
	case sandboxSecretsValueCount:
		return sandboxSecretsIsCount(args[i+1])
	case sandboxSecretsValueAction:
		return sandboxSecretsSearchActions[strings.ToLower(args[i+1])]
	default:
		return true
	}
}

// sandboxSecretsIsCount reports whether the word is a count a search option takes.
func sandboxSecretsIsCount(word string) bool {
	if word == "" {
		return false
	}
	for _, r := range word {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sandboxSecretsSearchFiles returns the words a grep or rg run may open as files. The
// pattern is not one: the value of -e or --regexp, and the first operand when -e,
// --regexp, -f or --file names no pattern. An option's value is skipped only where
// the command gives it one (sandboxSecretsSearchTakesValue). Flags stay in the list,
// so --file=.env and -f.env still count. A word after -- is an operand whatever it
// looks like.
func sandboxSecretsSearchFiles(tool string, args []string) []string {
	patterned := false
	for _, a := range args {
		if a == "-e" || a == "-f" || a == "--regexp" || a == "--file" ||
			strings.HasPrefix(a, "--regexp=") || strings.HasPrefix(a, "--file=") ||
			sandboxSecretsIsShortCluster(a) && strings.ContainsAny(a, "ef") {
			patterned = true
		}
	}
	var files []string
	dashes := false
	operands := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !dashes && a == "--":
			dashes = true
		case !dashes && (a == "-e" || a == "--regexp"):
			i++ // the pattern
		case !dashes && strings.HasPrefix(a, "--regexp="):
			// the pattern, glued to its option
		case !dashes && sandboxSecretsSearchTakesValue(tool, args, i):
			i++ // a value that is no file
		case !dashes && strings.HasPrefix(a, "-"):
			files = append(files, a)
		default:
			operands++
			if operands == 1 && !patterned {
				continue // the pattern
			}
			files = append(files, a)
		}
	}
	return files
}

// sandboxSecretsIsCredentialFile reports whether the word names a file that holds secrets: a
// dotenv file (deepseek.env, .env.local, .env.example), ~/.netrc or ~/.git-credentials.
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
		case a == "-S" || a == "--split-string":
			// The value is the command line env runs, judged as a payload of its own.
			return i+1 >= len(args) || strings.TrimSpace(args[i+1]) == ""
		case strings.HasPrefix(a, "--split-string="):
			return strings.TrimSpace(strings.TrimPrefix(a, "--split-string=")) == ""
		case strings.HasPrefix(a, "-S"):
			return strings.TrimSpace(a[2:]) == ""
		case a == "-u" || a == "-C" || a == "--unset" || a == "--chdir":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "="):
		default:
			return false // a command follows
		}
	}
	return true
}

// sandboxSecretsExpandedNames returns the variables the command expands: $NAME and
// ${NAME} outside single quotes, since bare and double-quoted text both expand. A
// single-quoted word is literal, so echo '$TOKEN' reads no variable. A backslash
// escapes the next character outside single quotes. It returns nil when the quoting
// does not balance, and the caller then judges every name as expanded.
func sandboxSecretsExpandedNames(command string) map[string]bool {
	names := map[string]bool{}
	inSingle, inDouble := false, false
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case inSingle:
			inSingle = c != '\''
		case c == '\\':
			i++
		case c == '\'' && !inDouble:
			inSingle = true
		case c == '"':
			inDouble = !inDouble
		case c == '$':
			if m := sandboxSecretsVarRefAtRe.FindStringSubmatch(command[i:]); m != nil {
				names[m[1]] = true
			}
		}
	}
	if inSingle || inDouble {
		return nil
	}
	return names
}

// sandboxSecretsExpandsSecret reports whether the word expands a secret-like variable that
// the command expands outside single quotes (sandboxSecretsExpandedNames). A nil set means
// the quoting did not balance, so the word is judged by its names alone.
func sandboxSecretsExpandsSecret(word string, expanded map[string]bool) bool {
	for _, m := range sandboxSecretsVarRefRe.FindAllStringSubmatch(word, -1) {
		if sandboxSecretsIsSecretName(m[1]) && (expanded == nil || expanded[m[1]]) {
			return true
		}
	}
	return false
}

// sandboxSecretsReadableText returns the command one segment at a time, as the environ-path
// check reads it: quotes are gone and each segment's words are joined, so a path split by
// quotes or by a substitution is still one path. Left out are a git commit's -m or --message
// values, which git writes into the commit and opens no file for. A value that runs a
// substitution stays in, because the shell runs the substitution.
func sandboxSecretsReadableText(command string) string {
	var b strings.Builder
	for _, segment := range splitShellSegments(shellTokenize(command)) {
		skip := sandboxSecretsCommitMessages(segment)
		for i, word := range segment {
			if !skip[i] {
				b.WriteString(word)
				b.WriteByte(' ')
			}
		}
		b.WriteString("; ")
	}
	return b.String()
}

// sandboxSecretsCommitMessages marks the words of a git commit segment that are its -m or
// --message value: -mTEXT, --message=TEXT, and the word after -m or --message. Only the
// commit subcommand's words are read this way, past git's global options; another
// subcommand's -m is judged like any other word.
func sandboxSecretsCommitMessages(segment []string) []bool {
	skip := make([]bool, len(segment))
	if len(segment) == 0 || strings.ToLower(filepath.Base(segment[0])) != "git" {
		return skip
	}
	sub := gitSubcommandIndex(segment[1:]) + 1
	if sub < 1 || segment[sub] != "commit" {
		return skip
	}
	for i := sub + 1; i < len(segment); i++ {
		word := segment[i]
		switch {
		case word == "-m" || word == "--message":
			if i+1 < len(segment) {
				skip[i+1] = !sandboxSecretsRunsSubstitution(segment[i+1])
			}
			i++
		case strings.HasPrefix(word, "--message="):
			skip[i] = !sandboxSecretsRunsSubstitution(strings.TrimPrefix(word, "--message="))
		case strings.HasPrefix(word, "-m") && !strings.HasPrefix(word, "--"):
			skip[i] = !sandboxSecretsRunsSubstitution(word[2:])
		}
	}
	return skip
}

// sandboxSecretsRunsSubstitution reports whether the text has a command substitution the
// shell would run.
func sandboxSecretsRunsSubstitution(text string) bool {
	return strings.Contains(text, "$(") || strings.Contains(text, "`")
}

// sandboxSecretsEscapes turns the escapes that make a line break or a tab in an echo or
// printf argument into the characters themselves.
var sandboxSecretsEscapes = strings.NewReplacer(`\n`, "\n", `\t`, "\t")

// sandboxSecretsPrintedText returns the text an echo or printf writes, given its argv. A
// shell that reads that text runs it as a script, so echo printenv | sh is judged as
// printenv. Leading options (echo -e) are dropped.
func sandboxSecretsPrintedText(argv []string) string {
	args := argv[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	return sandboxSecretsEscapes.Replace(strings.Join(args, " "))
}

// sandboxSecretsPipeSegments splits tokens as splitShellSegments does, and reports for
// each segment whether a pipe feeds it. A ;, && or || separator feeds nothing.
func sandboxSecretsPipeSegments(tokens []string) (segments [][]string, piped []bool) {
	var current []string
	pipe := false // whether the separator before current is a pipe
	for _, token := range tokens {
		if shellCommandSeparators[token] {
			if len(current) > 0 {
				segments = append(segments, current)
				piped = append(piped, pipe)
			}
			current = nil
			pipe = token == "|"
			continue
		}
		current = append(current, token)
	}
	if len(current) > 0 {
		segments = append(segments, current)
		piped = append(piped, pipe)
	}
	return segments, piped
}

// sandboxSecretsPipeOutput returns the text a command writes to its output, given the
// text a pipe feeds it: what echo or printf prints, or the input of cat or tee, which
// pass it on. Any other command writes nothing the guard follows, so "". A filter that
// rewrites its text (tr, sed) and a file the text is saved to are not followed.
func sandboxSecretsPipeOutput(argv []string, fed string) string {
	if len(argv) == 0 {
		return ""
	}
	switch strings.ToLower(filepath.Base(argv[0])) {
	case "echo", "printf":
		return sandboxSecretsPrintedText(argv)
	case "cat", "tee":
		return fed
	}
	return ""
}

// sandboxSecretsStdoutText returns the text a command's last stage writes when the
// command runs with no input, or "" when that stage writes none the guard follows.
func sandboxSecretsStdoutText(command string) string {
	expanded := sandboxSecretsExpandedNames(command)
	segments, piped := sandboxSecretsPipeSegments(shellTokenize(prWorkflowTokenText(command)))
	var stdin, out string
	for i, segment := range segments {
		_, _, argv := sandboxSecretsSegment(segment, nil, expanded)
		out = sandboxSecretsPipeOutput(argv, stdin)
		stdin = ""
		if i+1 < len(segments) && piped[i+1] {
			stdin = out
		}
	}
	return out
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

// sandboxSecretsIsSecretFile reports whether the word names a file that holds
// credentials or account data: ~/.claude.json, a dotenv file, ~/.netrc or
// ~/.git-credentials.
func sandboxSecretsIsSecretFile(word string) bool {
	return sandboxSecretsIsClaudeJSONPath(word) || sandboxSecretsIsCredentialFile(word)
}

// sandboxSecretsIsEnvTemplate reports whether the word names the dotenv template the
// repo commits, .env.example, by a literal path inside a working tree. The guard reads
// names, not contents, so this is the one file it takes on trust, and only where the
// trust holds: a path that is absolute, under ~ or built from a variable may be any
// dotenv file.
func sandboxSecretsIsEnvTemplate(word string) bool {
	w := strings.Trim(word, `"'`)
	if strings.HasPrefix(w, "/") || strings.HasPrefix(w, "~") || strings.Contains(w, "$") {
		return false
	}
	return strings.ToLower(filepath.Base(w)) == ".env.example"
}

// sandboxSecretsIsReadableSecretFile reports whether reading the word would print a
// secret. Reading the committed template is ordinary work. Sending it is still judged
// by sandboxSecretsIsSecretFile: a transfer hands over whatever the file holds, and
// the guard judges names, never contents.
func sandboxSecretsIsReadableSecretFile(word string) bool {
	return sandboxSecretsIsSecretFile(word) && !sandboxSecretsIsEnvTemplate(word)
}

func sandboxSecretsIsClaudeJSONPath(word string) bool {
	return filepath.Base(strings.Trim(word, `"'`)) == ".claude.json"
}

// sandboxSecretsFileFlags are the flags whose value is a file a transfer command
// reads and sends: a config file that may hold headers (curl -K), a netrc
// (curl --netrc-file), an upload (curl -T) and a request body (wget --post-file).
var sandboxSecretsFileFlags = map[string]bool{
	"-K": true, "--config": true, "--netrc-file": true,
	"-T": true, "--upload-file": true, "--post-file": true, "--body-file": true,
}

// sandboxSecretsSentFile returns the first credential file a transfer command
// reads and sends, given as the value of a file flag or as the @file of a data,
// form or header argument, or "" when none is. The argument forms are curl's -d
// @f and -F name=@f, and httpie's field=@f and Header:@f.
func sandboxSecretsSentFile(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flag, value, found := strings.Cut(a, "="); found && sandboxSecretsFileFlags[flag] {
			if sandboxSecretsIsSecretFile(value) {
				return value
			}
			continue
		}
		if sandboxSecretsFileFlags[a] && i+1 < len(args) {
			i++
			if sandboxSecretsIsSecretFile(args[i]) {
				return args[i]
			}
			continue
		}
		if file, ok := sandboxSecretsGluedFile(a); ok {
			if sandboxSecretsIsSecretFile(file) {
				return file
			}
			continue
		}
		if path, ok := sandboxSecretsAtOperand(a); ok && sandboxSecretsIsSecretFile(path) {
			return path
		}
	}
	return ""
}

// sandboxSecretsAtOperand returns the file a data, form or header argument reads
// with an @: the rest of @file, -d@file, name=@file or Header:@file. A @ inside a
// word, as in a URL's user@host, names no file.
func sandboxSecretsAtOperand(word string) (string, bool) {
	// A short option takes the @ glued to it: -d@file, -H@file.
	if len(word) > 2 && word[0] == '-' && word[1] != '-' && word[2] == '@' {
		return word[3:], true
	}
	i := strings.IndexByte(word, '@')
	if i < 0 || (i > 0 && word[i-1] != '=' && word[i-1] != ':') {
		return "", false
	}
	// A form part may carry a content type after the file: name=@file;type=text/plain.
	path, _, _ := strings.Cut(word[i+1:], ";")
	return path, true
}

// sandboxSecretsGluedFile returns the file a short file flag carries glued to it,
// as curl's -K.netrc and -T.env do, or false when the word is not such a flag.
func sandboxSecretsGluedFile(word string) (string, bool) {
	if len(word) > 2 && sandboxSecretsFileFlags[word[:2]] {
		return word[2:], true
	}
	return "", false
}

// sandboxSecretsTaint records in tainted the variables a segment assigns a value
// that expands a secret-like variable, or one already tainted: T=$FORGEJO_TOKEN,
// export T=${DB_PASSWORD}, U=$T. A name that is not secret-like still holds the
// secret, so an argument that expands it is judged as one that expands the secret
// (curl -H "X: $T"). Taint is only ever added, so a later assignment of a plain
// value does not clear it: the walk errs toward blocking.
func sandboxSecretsTaint(segment []string, tainted map[string]bool) {
	for _, tok := range segment {
		if !isEnvAssignment(tok) {
			continue
		}
		name, value, _ := strings.Cut(tok, "=")
		if sandboxSecretsRefsSecret(value) || sandboxSecretsRefsTainted(value, tainted) {
			tainted[name] = true
		}
	}
}

// sandboxSecretsRefsTainted reports whether the word expands a variable that
// holds a secret, as recorded by sandboxSecretsTaint.
func sandboxSecretsRefsTainted(word string, tainted map[string]bool) bool {
	for _, m := range sandboxSecretsVarRefRe.FindAllStringSubmatch(word, -1) {
		if tainted[m[1]] {
			return true
		}
	}
	return false
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
