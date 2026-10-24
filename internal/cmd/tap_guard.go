package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardCmd = &cobra.Command{
	Use:   "guard",
	RunE:  requireSubcommand,
	Short: "Block forbidden operations (PreToolUse hook)",
	Long: `Block forbidden operations via Claude Code PreToolUse hooks.

Guard commands exit with code 2 to BLOCK tool execution when a policy
is violated. They're called before the tool runs, preventing the
forbidden operation entirely.

Available guards:
  pr-workflow        - Block PR creation and feature branches
  bd-init            - Block bd init in wrong directories
  mol-patrol         - Block mol patrol from agent contexts
  dangerous-command  - Block rm -rf, force push, hard reset, git clean
  container-suite    - Block unwrapped go test/make test on testcontainers-backed packages
  polecat-paths      - Block Edit/Write/Bash targets outside the polecat's own worktree
  bd-close-invariant - Block raw bd close of a bead whose branch carries unmerged work
  question-tool      - Deny AskUserQuestion in a session with nobody at the pane
  permission-request - Deny a permission prompt an unattended session cannot answer (PermissionRequest hook, exit 0)

External guards (standalone scripts, not compiled into gt):
  context-budget   - scripts/guards/context-budget-guard.sh

Example hook configuration (matcher is the TOOL NAME only, a shell guard's
being "Bash|Monitor" — a bare "Bash" lets a command run through the
Monitor tool reach the guard's target unguarded, gt-vx2mm; a command
pattern like "Bash(gh pr create*)" in "matcher" never fires, gt-5ihs —
and no "if" either: Claude Code's "if" evaluator matches ANY pattern for a
command it cannot statically resolve, so a deny guard must read
tool_input.command off stdin and self-filter, gt-3mp1):
  {
    "PreToolUse": [{
      "matcher": "Bash|Monitor",
      "hooks": [{"command": "gt tap guard pr-workflow"}]
    }]
  }`,
}

var tapGuardPRWorkflowCmd = &cobra.Command{
	Use:   "pr-workflow",
	Short: "Block PR creation and feature branches",
	Long: `Block PR workflow operations in Gas Town.

Gas Town workers push directly to main. PRs add friction that breaks
the autonomous execution model (GUPP principle).

This guard blocks:
  - gh pr create
  - git checkout -b (feature branches)
  - git switch -c (feature branches)

Exit codes:
  0 - Operation allowed (not in Gas Town agent context, not maintainer origin)
  2 - Operation BLOCKED (in agent context OR maintainer origin)

The guard blocks in two scenarios:
  1. Running as a Gas Town agent (crew, polecat, witness, etc.)
  2. Origin remote is steveyegge/gastown (maintainer should push directly)

One exemption narrows the first scenario:
  - a polecat creating a local session branch inside its own worktree
    (gt-6hg7) — the escape route for a polecat that resumed a branch whose
    push had already gone out, where gt done's recoverDivergedPush refuses
    the force-push and the alternative this message used to offer (push to
    main) is itself blocked for polecats (gt-ibt8).
"gh pr create" stays blocked for every role, the exemption included.

Humans running outside Gas Town with a fork origin can still use PRs.

This command expects the hook's tool_input JSON on stdin. Run manually on
a terminal with no input piped in and it will hang reading stdin until
EOF (Ctrl-D) — same as the dangerous-command guard.`,
	RunE: runTapGuardPRWorkflow,
}

func init() {
	tapCmd.AddCommand(tapGuardCmd)
	tapGuardCmd.AddCommand(tapGuardPRWorkflowCmd)
}

// shellCommandSeparators are shell operators that start a new command
// within a single line. Claude Code evaluates Bash permission rules
// per sub-command, but the hook is registered without an "if" glob and sees
// the whole line, so this guard must inspect each ;/&/&&/||/| segment
// independently rather than only the start of the whole line, or a leading
// unrelated segment lets a real PR-workflow command later on the line slip
// through (gt-wisp-52y4). An unquoted
// newline separates commands the same way and arrives here as ";" — see
// spaceOutShellOperators (gt-3j8u).
//
// A background & belongs here with the others: it runs the following command
// just as ; does, so leaving it out let a text-only command in front of it
// speak for the real command behind — "echo hi & git clean -f" was judged a
// single echo call and the clean ran unchecked (gt-wwwht).
//
// A "}" closing a brace group belongs here too, because the ";" that ends the
// group's last command comes before it: the group's words would otherwise ride
// into the following segment as "}"-prefixed tokens, hiding that segment's
// command from every matcher keyed on its first words, and the walk would read
// the group's cd as continuing an argument list (gt-ajyw8). The "{" that opens
// the group is not a separator here — it must stay in the group's segment — so
// shellCommandStart names it instead.
var shellCommandSeparators = map[string]bool{
	"&&": true,
	"||": true,
	";":  true,
	"|":  true,
	"&":  true,
	"}":  true,
}

// maxPRWorkflowNestDepth bounds the payload recursion in
// prWorkflowInvocations, mirroring maxDangerousNestDepth: a payload can itself
// hold a payload, and without a cap that nesting could recurse as deep as the
// input text allows.
const maxPRWorkflowNestDepth = 3

// matchesPRWorkflowCommand reports whether command runs gh pr create,
// git checkout -b or git switch -c. The hook is registered without an "if"
// glob and self-filters here, so a command that runs none of the three is
// allowed and not every Bash call in agent context is blocked (gt-pjeh).
//
// Each shell segment is judged by the command it runs, not by its first words
// (prWorkflowInvocations), so an env assignment, a launcher, a group opener, a
// path-qualified binary, gh's -R flag or a bash -c or eval payload cannot
// hide it (gt-zf2vy). Words are compared whole, so "git checkout -branch"
// does not match "git checkout -b", and quoted text is never a command word.
//
// Heredoc bodies are data, not shell syntax, so they are stripped before
// tokenizing (gt-3j8u): a body line that spells "gh pr create" would
// otherwise block an ordinary file write. A body fed to a shell invoker is
// the exception, because that shell runs it (gt-9g0y, gt-q91m).
func matchesPRWorkflowCommand(command string) bool {
	invocations, tooDeep := prWorkflowInvocations(command, 0)
	if tooDeep {
		return true
	}
	for _, argv := range invocations {
		if isGhPRCreate(argv) || isBranchCreation(argv) {
			return true
		}
	}
	return false
}

// prWorkflowLauncher describes a command that runs the command behind it.
// valueFlags are its own flags whose value is the next word, and positionals
// counts the operands before the command (timeout's DURATION).
type prWorkflowLauncher struct {
	valueFlags  map[string]bool
	positionals int
}

// prWorkflowLaunchers are the commands prWorkflowCommand looks through:
// segmentCommandWord's launchers plus timeout and the other wrappers that run
// the command behind them (setsid, doas, ionice, caffeinate, xargs).
var prWorkflowLaunchers = map[string]prWorkflowLauncher{
	"env":     {valueFlags: map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true}},
	"time":    {valueFlags: map[string]bool{"-f": true, "--format": true, "-o": true, "--output": true}},
	"command": {},
	"nohup":   {},
	"builtin": {},
	"exec":    {valueFlags: map[string]bool{"-a": true}},
	"nice":    {valueFlags: map[string]bool{"-n": true, "--adjustment": true}},
	"stdbuf":  {valueFlags: map[string]bool{"-i": true, "-o": true, "-e": true}},
	"sudo": {valueFlags: map[string]bool{
		"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-r": true,
		"-t": true, "-T": true, "-U": true, "-D": true,
	}},
	"setsid":     {},
	"doas":       {valueFlags: map[string]bool{"-u": true, "-C": true}},
	"ionice":     {valueFlags: map[string]bool{"-c": true, "--class": true, "-n": true, "--classdata": true}},
	"caffeinate": {valueFlags: map[string]bool{"-t": true, "-w": true}},
	"xargs": {valueFlags: map[string]bool{
		"-I": true, "-n": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true,
	}},
	"timeout": {valueFlags: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, positionals: 1},
	// Homebrew coreutils installs timeout as gtimeout.
	"gtimeout": {valueFlags: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, positionals: 1},
}

// prWorkflowCommand returns the command a shell segment runs, from its command
// word on. It looks through the group openers and control keywords in front
// (trimShellKeywords), leading NAME=value assignments, leading redirections, a
// command substitution standing where the command word goes, and launchers with
// their flags. payloads holds the command line a launcher itself carries, as in
// env -S 'gh pr create'. argv is empty when nothing runs. standIn is set when
// a substitution stood in the command word's place, so argv's first word may be
// an argument of the command the substitution expanded to.
func prWorkflowCommand(segment []string) (argv, payloads []string, standIn bool) {
	seg := trimShellKeywords(segment)
	for len(seg) > 0 {
		// A redirection ahead of the command is setup the shell performs
		// before it runs the command, exactly as an assignment is, so it and
		// its target are stepped over: "2>/dev/null gh pr create" creates the
		// PR the unprefixed line does (gt-cwi1q).
		if skip := redirectionSkip(seg); skip != 0 {
			seg = seg[skip:]
			continue
		}
		word := seg[0]
		if isEnvAssignment(word) || word == "!" {
			seg = seg[1:]
			continue
		}
		// A substitution in the command word's place expands to the command,
		// and the shell runs the words behind it as that command's own when
		// the expansion is empty ("$(echo) gh pr create" runs gh), so those
		// words are judged as a command line (gt-qgya2). When it expands to
		// gh or git instead ("$(echo gh) pr create"), the words behind it are
		// that command's arguments; standIn lets the caller judge them so too
		// (gt-n2gt8). The expansion itself is unreadable, and
		// prWorkflowSubstitutions judges its body. A command named through a
		// variable ("g=gh; $g pr create") is not resolved: the guard reads
		// the text, not the shell's variables.
		if word == prWorkflowSubstitutionWord {
			seg = seg[1:]
			standIn = true
			continue
		}
		name := strings.ToLower(filepath.Base(word))
		launcher, ok := prWorkflowLaunchers[name]
		if !ok {
			return seg, payloads, standIn
		}
		var payload string
		seg, payload = skipLauncherArgs(name, launcher, seg[1:])
		if payload != "" {
			payloads = append(payloads, payload)
		}
	}
	return nil, payloads, standIn
}

// skipLauncherArgs drops the flags and operands a launcher takes ahead of the
// command it runs and returns the words from that command on. "--" ends the
// flags. env -S takes a whole command line as its value, returned as payload.
func skipLauncherArgs(name string, launcher prWorkflowLauncher, rest []string) (seg []string, payload string) {
	seg, payload, _ = skipLauncherArgsChdir(name, launcher, rest)
	return seg, payload
}

// skipLauncherArgsChdir is skipLauncherArgs that also returns the directories
// the launcher's own flags change into (env -C/--chdir, sudo -D/--chdir), in
// shell order.
func skipLauncherArgsChdir(name string, launcher prWorkflowLauncher, rest []string) (seg []string, payload string, chdirs []string) {
	i := 0
	// skipRedirections steps over the redirections at rest[i:]. The shell
	// removes them before the launcher sees its arguments, so one standing
	// among the flags, or ahead of a flag's value or an operand, is not that
	// word: "timeout 2>/dev/null 5 rm" runs rm under a 5 second limit.
	skipRedirections := func() {
		for i < len(rest) {
			n := redirectionSkip(rest[i:])
			if n == 0 {
				return
			}
			i += n
		}
	}
	// next returns the word after any redirections and steps past it.
	next := func() string {
		skipRedirections()
		if i >= len(rest) {
			return ""
		}
		word := rest[i]
		i++
		return word
	}
	for {
		skipRedirections()
		if i >= len(rest) {
			break
		}
		word := rest[i]
		if word == "--" {
			i++
			break
		}
		if len(word) < 2 || word[0] != '-' {
			break
		}
		if name == "env" {
			switch {
			case word == "-S" || word == "--split-string":
				i++
				payload = next()
				continue
			case strings.HasPrefix(word, "--split-string="):
				payload = strings.TrimPrefix(word, "--split-string=")
				i++
				continue
			case strings.HasPrefix(word, "-S"):
				payload = word[2:]
				i++
				continue
			}
		}
		if dir, ok := launcherChdirFlag(name, launcher, word); ok {
			i++
			if dir == "" {
				dir = next()
			}
			if dir != "" {
				chdirs = append(chdirs, dir)
			}
			continue
		}
		width := launcherFlagWidth(launcher, word)
		i++
		if width == 2 {
			next()
		}
	}
	for n := 0; n < launcher.positionals; n++ {
		next()
	}
	// A redirection after the last operand is shell setup, not the command, so
	// it is stepped over here. Callers then never have to skip it themselves.
	skipRedirections()
	return rest[i:], payload, chdirs
}

// launcherChdirFlag returns the directory a launcher flag changes into — the
// empty string when it stands in the next word — and whether word is such a
// flag. Only env -C/--chdir and sudo -D/--chdir do this: they run the command
// behind them in that directory (gt-l18zp). A short flag inside a cluster
// counts only when launcherFlagWidth would hand it the directory, so "env -iC
// dir" changes directory and "env -uC name" does not.
func launcherChdirFlag(name string, launcher prWorkflowLauncher, word string) (dir string, ok bool) {
	short, long := "", ""
	switch name {
	case "env":
		short, long = "-C", "--chdir"
	case "sudo":
		short, long = "-D", "--chdir"
	default:
		return "", false
	}
	switch {
	case word == short || word == long:
		return "", true
	case strings.HasPrefix(word, long+"="):
		return word[len(long)+1:], true
	}
	if len(word) < 2 || word[0] != '-' || word[1] == '-' {
		return "", false
	}
	for j := 1; j < len(word); j++ {
		flag := "-" + string(word[j])
		if !launcher.valueFlags[flag] {
			continue
		}
		if flag != short {
			return "", false
		}
		return word[j+1:], true
	}
	return "", false
}

// launcherFlagWidth reports how many words a launcher flag occupies: 2 when it
// owns the next word as its value, 1 otherwise. A cluster of short flags is
// read one letter at a time the way getopt reads it — a value flag in it owns
// the rest of the word ("-n1") or, with nothing after it, the next word
// ("caffeinate -it 5", "sudo -Eu alice") — since the next word would otherwise
// read as the command being run (gt-l18zp).
func launcherFlagWidth(launcher prWorkflowLauncher, word string) int {
	if launcher.valueFlags[word] {
		return 2
	}
	if len(word) < 3 || word[0] != '-' || word[1] == '-' {
		return 1
	}
	for j := 1; j < len(word); j++ {
		if !launcher.valueFlags["-"+string(word[j])] {
			continue
		}
		if j == len(word)-1 {
			return 2
		}
		return 1
	}
	return 1
}

// prWorkflowInvocations returns the command every segment of command runs, as
// argv from the command word on, plus the commands inside every payload a
// shell runs: a shell invoker's -c string, an eval's joined arguments, an
// env -S string, the body of a heredoc fed to a shell, and the body of every
// $(...) or backtick substitution the shell expands. A payload is read only
// where a shell runs it, so "echo bash -c 'gh pr create'" is an echo, while
// PR_URL=$(gh pr create --fill) runs the gh inside it (gt-z9asr).
//
// A payload past maxPRWorkflowNestDepth is not read; tooDeep is set when it
// still names gh or git, since a script the guard cannot reach is not one that
// runs nothing.
func prWorkflowInvocations(command string, depth int) (invocations [][]string, tooDeep bool) {
	tokens := shellTokenize(prWorkflowTokenText(command))

	var payloads []string
	for _, segment := range splitShellSegments(tokens) {
		argv, launched, standIn := prWorkflowCommand(segment)
		payloads = append(payloads, launched...)
		if len(argv) == 0 {
			continue
		}
		invocations = append(invocations, argv)
		if standIn {
			// The substitution may have expanded to gh or git, so the words
			// behind it are read as the arguments of each (gt-n2gt8).
			invocations = append(invocations, append([]string{"gh"}, argv...), append([]string{"git"}, argv...))
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
			tooDeep = tooDeep || prWorkflowNamesGhOrGit.MatchString(payload)
			continue
		}
		nested, deep := prWorkflowInvocations(payload, depth+1)
		invocations = append(invocations, nested...)
		tooDeep = tooDeep || deep
	}
	return invocations, tooDeep
}

// prWorkflowSubstitutionWord stands in for a command substitution while the
// pr-workflow guard tokenizes, so the words on either side of it stay whole.
const prWorkflowSubstitutionWord = "__gt_substitution__"

// prWorkflowTokenText returns command as the text prWorkflowInvocations
// tokenizes: every command substitution the shell expands is replaced by one
// opaque word.
//
// shellTokenize splits on whitespace and knows nothing of a substitution scope,
// so it cuts "x=$(echo hi) gh pr create" into "x=$(echo", "hi)", "gh" — the
// "$(" left glued to the assignment and the ")" left glued to the last word of
// the body. The segment judge reads the assignment as setup and "hi)" as the
// command word, so the "gh pr create" the line really runs, with x set for it,
// was no word the guard saw and the line was allowed (gt-4t56h). One word in
// the substitution's place keeps the words behind its ")" whole; the body is
// judged as a command line of its own by prWorkflowSubstitutions.
func prWorkflowTokenText(command string) string {
	text := stripHeredocBodies(strings.TrimSpace(command))
	spans := substitutionSpansIn(text, true)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, span := range spans {
		b.WriteString(text[last:span.open])
		b.WriteString(prWorkflowSubstitutionWord)
		last = prWorkflowSpanEnd(text, span)
	}
	b.WriteString(text[last:])
	return b.String()
}

// prWorkflowSpanEnd returns the index just past a substitution span: the ")" or
// backtick that closes it sits at bodyEnd, while an unterminated substitution
// runs to the end of the text.
func prWorkflowSpanEnd(text string, span substitutionSpan) int {
	if prWorkflowSpanUnterminated(text, span) {
		return len(text)
	}
	return span.bodyEnd + 1
}

// prWorkflowSpanUnterminated reports whether the text never closes a
// substitution: substitutionSpansIn then reads its body to the end of the text,
// so bodyEnd is the text's length.
func prWorkflowSpanUnterminated(text string, span substitutionSpan) bool {
	return span.bodyEnd >= len(text)
}

// prWorkflowSubstitutions returns the body of every command substitution the
// shell expands in command: the ones in the command text, and the ones in the
// body of an unquoted heredoc, which the shell expands while reading it. The
// scan is substitutionSpansIn, the one the other guards read too, so the
// quote, comment and escaping rules are the ones its comment states (gt-ejn41).
//
// A substitution nested in a body is not returned here: the recursion in
// prWorkflowInvocations scans the body for its own. A body is handed over as
// the shell would run it: the backslash that makes a backtick literal, and
// that the shell drops when it parses the body, is dropped too
// (substitutionBody), so the "gh pr create" inside "echo \`gh pr create\`" is
// read as the command it is. A body that runs to the end of the text hands its
// last ")" over as a word of its own (prWorkflowSplitBodyParen).
func prWorkflowSubstitutions(command string) []string {
	text := stripHeredocBodies(command)
	var bodies []string
	for _, span := range substitutionSpansIn(text, true) {
		body := substitutionBody(text, span)
		if prWorkflowSpanUnterminated(text, span) {
			body = prWorkflowSplitBodyParen(body)
		}
		if strings.TrimSpace(body) != "" {
			bodies = append(bodies, body)
		}
	}
	return append(bodies, heredocBodySubstitutions(command)...)
}

// prWorkflowSplitBodyParen returns body with a space in front of each ")" it
// ends on, so every word they were glued to is judged whole.
//
// The body of a substitution the text leaves unterminated runs to the end of
// the text, so a ")" there lands inside the body and hides the word it touches:
// "x=$(echo `gh pr create)" hands "gh pr create)" over one level down, whose
// last word is not the "create" the line would have run, and the gh inside it
// went unjudged (gt-4t56h). A run of them hides a word per ")", so the whole
// run is split (gt-qgya2).
func prWorkflowSplitBodyParen(body string) string {
	last := len(body) - 1
	if last < 1 || body[last] != ')' {
		return body
	}
	first := last
	for first > 1 && body[first-1] == ')' {
		first--
	}
	var b strings.Builder
	b.Grow(len(body) + 2*(len(body)-first))
	b.WriteString(body[:first])
	for range last + 1 - first {
		b.WriteString(" )")
	}
	return b.String()
}

// prWorkflowNamesGhOrGit finds gh or git as a word of its own, path-qualified
// or not, so "github" and "gh-pages" are not mistaken for the commands.
var prWorkflowNamesGhOrGit = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])(gh|git)([^A-Za-z0-9_.-]|$)`)

// ghOptionsWithValue are the gh flags that take the next word as their value
// when given separately (-R o/r); the glued forms (-Ro/r, --repo=o/r) are one
// word.
var ghOptionsWithValue = map[string]bool{"-R": true, "--repo": true}

// ghPositionals returns the non-flag words after gh, command-substitution
// stand-ins included. Flags are skipped wherever they sit, as cobra reads them:
// gh -R o/r pr create and gh pr create -R o/r are the same command. A stand-in
// that is the value of a flag in ghOptionsWithValue is that value, not a word.
func ghPositionals(args []string) []string {
	var words []string
	for i := 0; i < len(args); i++ {
		switch word := args[i]; {
		case ghOptionsWithValue[word]:
			i++
		case strings.HasPrefix(word, "-"):
		default:
			words = append(words, word)
		}
	}
	return words
}

// startsWithSubcommands reports whether words open with the subcommands want,
// reading a command-substitution stand-in as nothing or as one word, whichever
// lets the rest match: "gh pr $(echo) create" is gh pr create, and so is "gh pr
// $(echo create)" (gt-n2gt8). The shell splits an expansion into as many words
// as it holds, and a stand-in is read as at most one: the guard reads the text,
// not what a command prints, as it does not resolve a variable either ("g=gh;
// $g pr create" is not judged).
func startsWithSubcommands(words, want []string) bool {
	if len(want) == 0 {
		return true
	}
	if len(words) == 0 {
		return false
	}
	if words[0] == prWorkflowSubstitutionWord {
		return startsWithSubcommands(words[1:], want) || startsWithSubcommands(words[1:], want[1:])
	}
	return strings.EqualFold(words[0], want[0]) && startsWithSubcommands(words[1:], want[1:])
}

// isGhPRCreate reports whether argv, a command from its command word on, is
// gh pr create. gh may be a path.
func isGhPRCreate(argv []string) bool {
	if len(argv) == 0 || !strings.EqualFold(filepath.Base(argv[0]), "gh") {
		return false
	}
	return startsWithSubcommands(ghPositionals(argv[1:]), []string{"pr", "create"})
}

// isBranchCreation reports whether argv, a command from its command word on,
// is git checkout -b/-B or git switch -c/-C/--create/--force-create, with other
// flags before or after the creation flag ("checkout -q -b x", "checkout -bx",
// "switch --create x"). git may be a path and may carry global options (-C dir,
// -c k=v) in front of the subcommand. Words after "--" are paths, not flags.
// The capitals create or reset the branch their lowercase spellings name, so
// the guard reads both (gt-5wtmz); --force-create is the long spelling of -C
// and is read with it (gt-g27m7).
func isBranchCreation(argv []string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return false
	}
	rest := argv[1:]
	for {
		at := gitSubcommandIndex(rest)
		if at < 0 {
			return false
		}
		if rest[at] != prWorkflowSubstitutionWord {
			return createsBranch(rest[at], rest[at+1:])
		}
		// A substitution in the subcommand's place expands to nothing ("git
		// $(echo) checkout -b x") or to the subcommand ("git $(echo checkout)
		// -b x"), so both are judged (gt-n2gt8). One that is the value of a
		// global option never gets here: gitSubcommandIndex skips it.
		for _, subcommand := range []string{"checkout", "switch"} {
			if createsBranch(subcommand, rest[at+1:]) {
				return true
			}
		}
		rest = append(append([]string(nil), rest[:at]...), rest[at+1:]...)
	}
}

// createsBranch reports whether flags, the words behind git's subcommand, hold
// that subcommand's branch-creation flag. Words after "--" are paths.
func createsBranch(subcommand string, flags []string) bool {
	if !branchCreatingSubcommand(subcommand) {
		return false
	}
	for _, flag := range flags {
		if flag == "--" {
			break
		}
		if branchCreationFlag(subcommand, flag) {
			return true
		}
	}
	return false
}

// branchCreatingSubcommand reports whether subcommand is one git reads a
// branch-creation flag for.
func branchCreatingSubcommand(subcommand string) bool {
	return subcommand == "checkout" || subcommand == "switch"
}

// branchCreationFlag reports whether flag is subcommand's branch-creation
// option in any spelling git's option parser accepts: the bare short flag
// (-b/-B, -c/-C), a cluster of short flags ("-qB", "-qb"), a short flag glued
// to its value ("-bx"), and switch's long spellings --create and
// --force-create, bare, carrying their value after an "="
// ("--create=feature/x"), or abbreviated to a prefix of the name
// (switchLongFlagCreatesBranch). checkout has no long spelling for -b, and
// -C's long form is --force-create (gt-g27m7).
//
// Both halves of the guard read through this one predicate — isBranchCreation
// judges with it, isLeadingBranchCreation exempts with it — because an
// exemption narrower than the judge denies a polecat the spellings it may use
// for the same creation.
func branchCreationFlag(subcommand, flag string) bool {
	switch subcommand {
	case "checkout":
		return shortFlagCluster(flag, 'b') || shortFlagCluster(flag, 'B')
	case "switch":
		return shortFlagCluster(flag, 'c') || shortFlagCluster(flag, 'C') ||
			switchLongFlagCreatesBranch(flag)
	}
	return false
}

// switchCreationOptions are the long spellings of git switch's
// branch-creation flags: --create (switch's short -c) and --force-create (-C).
var switchCreationOptions = []string{"--create", "--force-create"}

// switchOptionsOverlappingCreation are git switch's other long options whose
// own spelling a prefix of a creation option also spells. git's parser takes
// an exact match before it abbreviates, so "--force" is switch's force — throw
// away local changes — and not the --force-create it also prefixes.
var switchOptionsOverlappingCreation = []string{"--force"}

// switchLongFlagCreatesBranch reports whether flag, a "--" word from a git
// switch command line, names a branch-creation option: --create or
// --force-create, in full or as any prefix of it, with any "=value" dropped.
//
// Every prefix counts, the ones the installed git refuses as ambiguous
// included ("--c" against --conflict, "--f" and "--fo" against --force):
// which prefixes an option set leaves unambiguous is a property of the git
// that runs the command, and a guard reading command text cannot know it.
// Reading one version's shortest resolvable prefix as the floor lets a
// shorter prefix that another version resolves past the guard, which is the
// bypass it exists to close; reading them all as the creation errs the other
// way, and blocks only commands git refuses, which create no branch
// (gt-osc72, gt-19s3v).
func switchLongFlagCreatesBranch(flag string) bool {
	name, _, _ := strings.Cut(flag, "=")
	if len(name) <= len("--") {
		return false // "--" is the end-of-options marker, not an option
	}
	for _, option := range switchOptionsOverlappingCreation {
		if name == option {
			return false
		}
	}
	for _, option := range switchCreationOptions {
		if strings.HasPrefix(option, name) {
			return true
		}
	}
	return false
}

// shortFlagCluster reports whether word is a short-option cluster ("-q", "-qb",
// "-bx") that spells letter, the way git's option parser reads bundled flags.
func shortFlagCluster(word string, letter byte) bool {
	return len(word) > 1 && word[0] == '-' && word[1] != '-' && strings.IndexByte(word[1:], letter) >= 0
}

func runTapGuardPRWorkflow(cmd *cobra.Command, args []string) error {
	// The hook carries no "if" pattern (gt-3mp1): it fires for every shell
	// command and gets no argument saying whether gh pr create, git checkout
	// -b or git switch -c ran, so the command is read back off stdin (Claude
	// Code hook protocol) and judged in full.
	return tapGuardPRWorkflow(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardPRWorkflow is the pr-workflow guard: it reads the hook payload
// from stdin and the session from proc, and prints a block to stderr.
func tapGuardPRWorkflow(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
	if err != nil {
		// Unreadable stdin is "we don't know what command this is", not
		// "nothing to block" — the same distinction evaluatePRWorkflowGuard
		// draws for an empty/unparsable payload (gt-wisp-52y4). Drop the
		// partial bytes io.ReadAll returns alongside the error (a truncated
		// command is not a command) and fall through to the unconditional
		// context/origin check. Returning early here failed the guard open,
		// letting the very command it exists to block through (gt-hift).
		input = nil
	}

	switch evaluatePRWorkflowGuard(input, proc) {
	case prWorkflowBlockAgentContext:
		printPRWorkflowAgentContextBlock(stderr)
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	case prWorkflowBlockMaintainerOrigin:
		printPRWorkflowMaintainerOriginBlock(stderr)
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}
	return nil
}

// prWorkflowGuardDecision is the pr-workflow guard's verdict on one hook
// payload: allow, or block with one of the two messages below.
type prWorkflowGuardDecision int

const (
	prWorkflowAllow prWorkflowGuardDecision = iota
	prWorkflowBlockAgentContext
	prWorkflowBlockMaintainerOrigin
)

// evaluatePRWorkflowGuard decides whether a hook payload may proceed.
//
// input is the raw hook JSON, or nil when stdin could not be read — the two
// cases collapse deliberately: a payload we could not read and a payload
// holding no command both mean "we don't know what command this is", and
// self-filtering is only safe when the command is actually known
// (gt-wisp-52y4). Unknown input therefore skips the role exemptions and the
// self-filter below and falls through to the unconditional context/origin
// check — the guard's only behavior before the self-filter existed — rather
// than allowing everything.
func evaluatePRWorkflowGuard(input []byte, proc guardProcess) prWorkflowGuardDecision {
	command := extractCommand(input)

	// gt-6hg7: a polecat that resumes a branch whose push already went out
	// (the gt-i0z3 class — mol-polecat-work's branch reuse rebases AND adds
	// fix commits, so the patch-ids differ and gt done's recoverDivergedPush
	// refuses the force-push by design) has no sanctioned escape. The only
	// remaining route is a fresh LOCAL session branch, and every spelling of
	// it was blocked here while the guard's own advice (push to main) is
	// blocked for polecats by gt-ibt8 — leaving the polecat fully stuck
	// pending a human force-push. Creating a local branch is not the PR path
	// this guard exists to block, so it is allowed for a polecat inside its
	// OWN worktree.
	//
	// Four conditions, each load-bearing:
	//
	//   - isPolecatSession() — the role, not merely GT_POLECAT, so a
	//     coordinator carrying a stale GT_POLECAT from spawning polecats
	//     cannot inherit the exemption.
	//   - isLeadingBranchCreation() — the command's FIRST word, so a branch
	//     creation glued after an && cannot carry a later segment past the
	//     guard (the gt-cyz8 escape).
	//   - !isPRCreateCommand() — "git checkout -b x && gh pr create" stays
	//     blocked for a polecat: a polecat has no reason to pair the two, and
	//     the PR path is exactly what this guard is for.
	//   - isInOwnPolecatWorktree() — the worktree is the thing a session
	//     branch is being created for. A polecat that has wandered into a
	//     sibling's worktree (the gt-hmaf cross-worktree class) or the rig
	//     root stays blocked. This also keeps the doctor's live-fire probe
	//     honest: it runs with GT_POLECAT=live-fire, GT_POLECAT_PATH stripped
	//     and cwd in a /tmp sandbox, so a bare role check would let its
	//     blocked-shape 'git checkout -b' through and read that as broken
	//     matcher wiring (gt-xy4b). Last because it is the only condition
	//     that touches the filesystem.
	if isPolecatSession(proc) && isLeadingBranchCreation(command) &&
		!isPRCreateCommand(command) && isInOwnPolecatWorktree(input, proc) {
		return prWorkflowAllow
	}

	// Self-filter only when a command was actually extracted. Some harness
	// templates (e.g. Copilot's INPUT=$(cat) wrapper, gt-wisp-52y4) drain
	// stdin before invoking this guard, so empty/unparsable input here means
	// "we don't know what command this is", not "nothing to block".
	if command != "" && !matchesPRWorkflowCommand(command) {
		return prWorkflowAllow
	}

	// Check if we're in a Gas Town agent context
	if isGasTownAgentContextIn(proc.getenv, proc.getwd) {
		return prWorkflowBlockAgentContext
	}

	// Check if origin is the maintainer's repo (steveyegge/gastown)
	if isMaintainerOrigin(proc) {
		return prWorkflowBlockMaintainerOrigin
	}

	// Not in Gas Town context and not maintainer origin - allow PRs
	return prWorkflowAllow
}

func printPRWorkflowAgentContextBlock(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ PR WORKFLOW BLOCKED                                          ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(w, "║  Gas Town workers push directly to main. PRs are forbidden.     ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Instead of:  gh pr create / git checkout -b / git switch -c    ║")
	fmt.Fprintln(w, "║  Do this:     git add . && git commit && git push origin main   ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  In your own polecat worktree, creating a session branch is     ║")
	fmt.Fprintln(w, "║  allowed: git checkout -b <branch> / git switch -c <branch>.    ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Why? PRs add friction that breaks autonomous execution.        ║")
	fmt.Fprintln(w, "║  See: ~/gt/docs/PRIMING.md (GUPP principle)                     ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "")
}

func printPRWorkflowMaintainerOriginBlock(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ PR BLOCKED - MAINTAINER ORIGIN                               ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(w, "║  Your origin is steveyegge/gastown - push directly to main.     ║")
	fmt.Fprintln(w, "║  PRs are for external contributors, not maintainers.            ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Instead of:  gh pr create                                      ║")
	fmt.Fprintln(w, "║  Do this:     git push origin main                              ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "")
}

// isGasTownAgentContext returns true if we're running as a Gas Town managed agent.
func isGasTownAgentContext() bool {
	return isGasTownAgentContextIn(os.Getenv, os.Getwd)
}

// isGasTownAgentContextIn is isGasTownAgentContext reading the environment
// through getenv and the working directory through getwd.
func isGasTownAgentContextIn(getenv func(string) string, getwd func() (string, error)) bool {
	// Check environment variables set by Gas Town session management
	envVars := []string{
		"GT_POLECAT",
		"GT_CREW",
		"GT_WITNESS",
		"GT_REFINERY",
		"GT_DEACON",
		"GT_DOG_NAME",
	}
	for _, env := range envVars {
		if getenv(env) != "" {
			return true
		}
	}

	// Also check if we're in a crew or polecat worktree by path
	cwd, err := getwd()
	if err != nil {
		return false
	}

	agentPaths := []string{"/crew/", "/polecats/", "/deacon/dogs/"}
	for _, path := range agentPaths {
		if strings.Contains(cwd, path) {
			return true
		}
	}

	return false
}

// isMaintainerOrigin returns true if the origin remote points to the maintainer's repo.
// This prevents the maintainer from accidentally creating PRs in their own repo.
func isMaintainerOrigin(proc guardProcess) bool {
	url, err := proc.originURL()
	if err != nil {
		return false
	}
	// Match both HTTPS and SSH URL formats:
	// - https://github.com/steveyegge/gastown.git
	// - git@github.com:steveyegge/gastown.git
	return strings.Contains(url, "steveyegge/gastown")
}

// isPolecatSession reports whether the current process is running as a
// polecat, by the same GT_ROLE-first rule `gt sling`, `gt hook` and
// `gt handoff` use: a coordinator session may carry a stale GT_POLECAT in
// its environment from having spawned polecats, so GT_ROLE decides whenever
// it is set and GT_POLECAT is only the fallback.
func isPolecatSession(proc guardProcess) bool {
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		return isPolecatRole(role)
	}
	return proc.getenv("GT_POLECAT") != ""
}

// isPolecatRole reports whether a GT_ROLE value names a polecat — the one
// place the polecat check is written down, so the guards that each carry
// their own fallback marker (isPolecatSession above, inPolecatSession in
// tap_guard_dangerous.go) cannot drift on what the role itself means.
func isPolecatRole(role string) bool {
	parsed, _, _ := parseRoleString(role)
	return parsed == RolePolecat
}

// isInOwnPolecatWorktree reports whether the hook payload's session cwd sits
// inside the worktree that owns this session — "a polecat's own worktree",
// as opposed to a sibling's (gt-hmaf's cross-worktree class), the rig root,
// or a sandbox.
//
// The boundary is resolvePolecatPathScope's, reused rather than re-derived:
// that helper already answers exactly this question for the polecat-paths
// guard (identity from GT_POLECAT, layout from GT_POLECAT_PATH with the
// session cwd as fallback, symlinks resolved, rig mismatch reconciled), and
// two guards disagreeing about which tree is the polecat's would be a bug
// of its own. An unresolvable scope is not a polecat session as far as this
// question goes, so the caller stays on the blocking side — the same
// fail-closed direction resolvePolecatPathScope's callers rely on.
func isInOwnPolecatWorktree(input []byte, proc guardProcess) bool {
	scope, ok := resolvePolecatPathScope(payloadCwd(input), proc)
	if !ok {
		return false
	}
	return scope.isWorktreePath(scope.cwd)
}

// isPRCreateCommand reports whether command runs "gh pr create" — the one
// pr-workflow pattern that stays blocked for every role. It reads the same
// invocations matchesPRWorkflowCommand does, so a gh pr create glued after an
// "&&" (gt-cyz8) or behind a wrapper (gt-zf2vy) cannot chain past the
// exemption that consults this check; a payload too deep to read counts.
func isPRCreateCommand(command string) bool {
	invocations, tooDeep := prWorkflowInvocations(command, 0)
	if tooDeep {
		return true
	}
	for _, argv := range invocations {
		if isGhPRCreate(argv) {
			return true
		}
	}
	return false
}

// isLeadingBranchCreation reports whether command's leading segment is a
// local branch creation — "git checkout -b" or "git switch -c" — read with
// isBranchCreation, the reader the judge blocks with: one reader for both
// halves, so the exemption carries every spelling the guard blocks (extra
// flags, clusters, the long spellings; gt-g27m7) and no shape it does not.
// Reading every word behind git as a flag counted its global options as
// creation flags — "-C dir" and "-c k=v" are git's own, not switch's — so a
// leading segment that only switches branches exempted the line (gt-oyvpp).
//
// The one exemption the pr-workflow guard grants is keyed on this shape: a
// polecat's fresh session branch (gt-6hg7), scoped to its own worktree.
//
// Leading-command by design: the exemption covers the command the line starts
// with and nothing glued behind it. Keying the old
// isFeatureBranchCommand's line-wide containment here instead would let a
// "gh pr create && git checkout -b" chain its PR create past the exemption
// (gt-cyz8) — so a checkout glued after an && on the same line is NOT branch
// creation here and exempts nothing.
func isLeadingBranchCreation(command string) bool {
	// The same token text prWorkflowInvocations reads, so the leading segment
	// is the one the judge sees and not a second reading of it.
	segs := splitShellSegments(shellTokenize(prWorkflowTokenText(command)))
	if len(segs) == 0 {
		return false
	}
	// The leading command is the one the first segment runs, so an env
	// assignment or launcher in front of git (the shapes the matcher now
	// reads, gt-zf2vy) keeps the exemption it had when they hid the command.
	seg, _, _ := prWorkflowCommand(segs[0])
	return isBranchCreation(seg)
}
