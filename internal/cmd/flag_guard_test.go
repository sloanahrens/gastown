package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The docs an agent reads are prose plus fenced shell, and nothing checks the
// flags in that shell. A doc that names a flag its command does not define
// stops the worker that follows it: mol-idea-to-plan told the operator to run
// `gt sling --prompt`, and `gt sling` has never had that flag (gt-6lm4o). The
// rule "check every flag against --help" does not scale; this guard does it.
//
// It reads the shipped formulas and role prompts as the repo holds them (the
// embed sources, so what it checks is what ships), keeps only the fenced lines
// that run `gt`, and resolves the leading words down this binary's cobra tree.
// A named flag must be defined on the command it names, locally or inherited.
//
// Fix a failure by correcting the doc that names the flag, never by loosening
// the guard.

// flagGuardSource is one embedded doc, path repo-relative.
type flagGuardSource struct {
	path string
	text string
}

// flagGuardGlobs are the embedded doc files the guard reads: the shipped
// formulas and every role prompt, including the town-root and polecat CLAUDE.md
// overlays. A path that matches none of these is not agent-facing doc.
var flagGuardGlobs = []string{
	"internal/formula/formulas/*.formula.toml",
	"internal/templates/roles/*.md.tmpl",
	"internal/templates/polecat-CLAUDE.md",
	"internal/templates/townroot/claude.md",
}

// flagGuardDocSources reads every embedded doc the globs name.
func flagGuardDocSources(t *testing.T, root string) []flagGuardSource {
	t.Helper()
	var sources []flagGuardSource
	for _, glob := range flagGuardGlobs {
		matches, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		for _, match := range matches {
			data, err := os.ReadFile(match)
			if err != nil {
				t.Fatalf("read %s: %v", match, err)
			}
			rel, err := filepath.Rel(root, match)
			if err != nil {
				t.Fatalf("rel %s: %v", match, err)
			}
			sources = append(sources, flagGuardSource{path: filepath.ToSlash(rel), text: string(data)})
		}
	}
	return sources
}

// flagGuardCmdFunc matches the role templates' binary-name function in any
// spacing or trim form. It renders to the gt binary's name, so a line naming a
// command through it runs gt.
var flagGuardCmdFunc = regexp.MustCompile(`\{\{-?\s*cmd\s*-?\}\}`)

// flagGuardLine is one fenced line, 1-based and physical.
type flagGuardLine struct {
	number int
	text   string
}

// flagGuardFencedLines returns the lines inside ``` or ~~~ fences. The fence
// markers themselves are not lines; a fence left open runs to the end of the
// file, as a renderer would read it.
func flagGuardFencedLines(text string) []flagGuardLine {
	var lines []flagGuardLine
	inFence := false
	for i, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			lines = append(lines, flagGuardLine{number: i + 1, text: flagGuardCmdFunc.ReplaceAllString(raw, "gt")})
		}
	}
	return lines
}

// flagGuardCommand is one fenced shell command that runs gt, its words joined
// across the line continuations a shell would join.
type flagGuardCommand struct {
	line int    // the physical line the command starts on
	text string // the command, {{ cmd }} folded to gt
}

// flagGuardInvokesGT matches a fenced line that runs gt somewhere other than
// as the first word: piped into (`... | gt escalate`), after a separator, or
// through command substitution (`id=$(gt bead create ...)`). The guard cannot
// tell which command such a line's words belong to, so it counts the line as
// skipped rather than guessing.
var flagGuardInvokesGT = regexp.MustCompile(`(?:^|[|&;(]|\$\()\s*gt[ \t]`)

// flagGuardCommands returns the fenced lines whose first word is gt, folding a
// line that ends in a backslash into the next: the shell joins them, so a flag
// on the continuation belongs to the command. A command is not joined past the
// end of the fence. It also counts the fenced lines that invoke gt in any
// other position — behind a pipe, a separator or a command substitution —
// where the guard declines to guess.
func flagGuardCommands(text string) (commands []flagGuardCommand, skipped int) {
	lines := flagGuardFencedLines(text)
	for i := 0; i < len(lines); i++ {
		// A quote-stripped copy, so gt named inside a message ("... ; gt prime")
		// does not read as an invocation.
		bare := flagGuardQuoted.ReplaceAllString(lines[i].text, " ")
		if !flagGuardInvokesGT.MatchString(bare) {
			continue
		}
		fields := strings.Fields(lines[i].text)
		if len(fields) == 0 || fields[0] != "gt" {
			skipped++
			continue
		}
		cmd := flagGuardCommand{line: lines[i].number, text: strings.TrimSpace(lines[i].text)}
		for strings.HasSuffix(strings.TrimSpace(cmd.text), "\\") && i+1 < len(lines) {
			i++
			cmd.text = strings.TrimSuffix(strings.TrimSpace(cmd.text), "\\") + " " + strings.TrimSpace(lines[i].text)
		}
		commands = append(commands, cmd)
	}
	return commands, skipped
}

// flagGuardCommandWord matches a word that can only be a command name. Flags,
// placeholders, paths, bead ids and quoted values all stop the walk, so what
// follows them reads as an argument, not a subcommand.
var flagGuardCommandWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// flagGuardResolve walks the leading command-like words of one gt command line
// down the cobra tree and returns the deepest command they name with the words
// that named it, nil when the first word names none (a shell variable in
// command position, a typo, a command a plugin adds). Returning the deepest
// match, not the exact path, is what lets `gt sling gt-abc myrig --force`
// resolve through sling. The matched words, not CommandPath, name the command
// in a finding: they are what the doc wrote.
func flagGuardResolve(root *cobra.Command, line string) (*cobra.Command, []string) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "gt" {
		return nil, nil
	}
	cmd := root
	var matched []string
	for _, word := range fields[1:] {
		if !flagGuardCommandWord.MatchString(word) {
			break
		}
		child := flagGuardFindChild(cmd, word)
		if child == nil {
			break
		}
		cmd = child
		matched = append(matched, child.Name())
	}
	if cmd == root {
		return nil, nil
	}
	return cmd, matched
}

// flagGuardFindChild returns the child cmd names, by name or alias.
func flagGuardFindChild(cmd *cobra.Command, word string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == word {
			return child
		}
		for _, alias := range child.Aliases {
			if alias == word {
				return child
			}
		}
	}
	return nil
}

// flagGuardFlag matches a long flag or a one-letter short flag. A bare dash,
// `--`, and negative numbers are not flags.
var flagGuardFlag = regexp.MustCompile(`^(--[A-Za-z][A-Za-z0-9-]*|-[A-Za-z])$`)

// flagGuardQuoted matches a single- or double-quoted run, so a flag inside a
// value ("Use --force to override") is not read as one on the command.
var flagGuardQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)

// flagGuardRef is one flag named on a gt command line.
type flagGuardRef struct {
	written string // as the doc wrote it: "--prompt" or "-s"
	short   bool
}

// name is the flag's name with its dashes and any =value stripped.
func (r flagGuardRef) name() string {
	name := strings.TrimLeft(r.written, "-")
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	return name
}

// flagGuardFlags returns the flags on one gt command line. Quoted runs and a
// trailing shell comment are dropped first, so their contents cannot read as
// flags. So does everything from the first command separator on: the flags of
// a piped command belong to that command, not to gt.
func flagGuardFlags(line string) []flagGuardRef {
	line = flagGuardQuoted.ReplaceAllString(line, " ")
	if cut := strings.IndexAny(line, "|&;"); cut >= 0 {
		line = line[:cut]
	}
	if cut := strings.Index(line, " #"); cut >= 0 {
		line = line[:cut]
	}
	var flags []flagGuardRef
	for _, field := range strings.Fields(line) {
		// An unbalanced quote is not stripped above, so one can cling here.
		trimmed := strings.TrimRight(field, `"`)
		if !flagGuardFlag.MatchString(trimmed) {
			continue
		}
		flags = append(flags, flagGuardRef{written: trimmed, short: !strings.HasPrefix(trimmed, "--")})
	}
	return flags
}

// flagGuardDefined reports whether cmd carries the flag, locally or by
// inheritance. A short flag lives in pflag's shorthand table, which Lookup
// does not read, so the two forms take different lookups.
func flagGuardDefined(cmd *cobra.Command, ref flagGuardRef) bool {
	lookup := func(flags *pflag.FlagSet) bool {
		if ref.short {
			return flags.ShorthandLookup(ref.name()) != nil
		}
		return flags.Lookup(ref.name()) != nil
	}
	return lookup(cmd.Flags()) || lookup(cmd.InheritedFlags())
}

// flagGuardFinding is one gt invocation naming a flag its command does not define.
type flagGuardFinding struct {
	path    string // repo-relative doc file
	line    int    // line the command starts on
	command string // the cobra path the words resolved to, "gt sling"
	flag    string // the flag as written, "--prompt"
}

func (f flagGuardFinding) String() string {
	return fmt.Sprintf("%s:%d: %s has no %s flag", f.path, f.line, f.command, f.flag)
}

// flagGuardScan checks every fenced gt command in text against root. It returns
// the findings, how many commands it resolved, and how many candidate lines it
// skipped: gt behind a pipe, a separator or a command substitution, or a first
// command word the tree does not hold. Skipped lines are reported, not guessed
// at.
func flagGuardScan(path, text string, root *cobra.Command) ([]flagGuardFinding, int, int) {
	var findings []flagGuardFinding
	commands, skipped := flagGuardCommands(text)
	resolved := 0
	for _, cmd := range commands {
		target, matched := flagGuardResolve(root, cmd.text)
		if target == nil {
			skipped++
			continue
		}
		resolved++
		name := "gt " + strings.Join(matched, " ")
		for _, flag := range flagGuardFlags(cmd.text) {
			if flagGuardDefined(target, flag) {
				continue
			}
			findings = append(findings, flagGuardFinding{path: path, line: cmd.line, command: name, flag: flag.written})
		}
	}
	return findings, resolved, skipped
}

// TestFlagGuardEmbeddedDocsNameRealFlags walks the shipped formulas and role
// prompts and fails when one tells an agent to run a gt command with a flag
// that command does not define. A doc that is right today is what makes the
// guard cheap to keep: nothing else notices when a flag is renamed away.
func TestFlagGuardEmbeddedDocsNameRealFlags(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	sources := flagGuardDocSources(t, root)
	// A scanner that goes blind fails loudly. The 2026-10-03 tree holds 29
	// sources and resolves 150 fenced gt commands; a glob typo or a moved
	// directory drops either floor to zero.
	if len(sources) < 20 {
		t.Fatalf("found %d embedded docs, want at least 20 — did the globs stop matching?", len(sources))
	}

	var findings []flagGuardFinding
	total, skipped := 0, 0
	for _, source := range sources {
		found, resolved, miss := flagGuardScan(source.path, source.text, rootCmd)
		findings = append(findings, found...)
		total += resolved
		skipped += miss
	}
	if total < 100 {
		t.Fatalf("resolved only %d fenced gt commands, want at least 100 — the fence scanner went blind", total)
	}
	t.Logf("flag guard: %d docs, %d gt commands checked, %d candidate lines skipped", len(sources), total, skipped)

	if len(findings) == 0 {
		return
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})
	lines := make([]string, len(findings))
	for i, f := range findings {
		lines[i] = f.String()
	}
	t.Errorf("embedded docs name flags their gt command does not define:\n%s", strings.Join(lines, "\n"))
}

// TestFlagGuardReportsABogusFlag is the positive control: the guard must report
// exactly the flag a synthetic doc gets wrong. Without it a scanner that found
// nothing — because it read no fences, or resolved no command — would look like
// a clean tree.
func TestFlagGuardReportsABogusFlag(t *testing.T) {
	t.Parallel()

	const doc = "description = \"\"\"\n" +
		"Run the dispatch:\n" +
		"\n" +
		"```bash\n" +
		"gt sling --prompt x\n" +
		"```\n" +
		"\"\"\"\n"

	findings, resolved, skipped := flagGuardScan("synthetic.formula.toml", doc, rootCmd)
	if resolved != 1 || skipped != 0 {
		t.Fatalf("resolved=%d skipped=%d, want 1 and 0", resolved, skipped)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(findings), findings)
	}
	got := findings[0]
	if got.flag != "--prompt" || got.command != "gt sling" || got.line != 5 {
		t.Errorf("finding = %+v, want the --prompt flag on gt sling at line 5", got)
	}

	// The same shape with a real flag must stay quiet, or the control above
	// would pass even for a guard that reports every flag it sees.
	const good = "description = \"\"\"\n" +
		"```bash\n" +
		"gt mail send mayor/ -s \"Subject\" --stdin\n" +
		"```\n" +
		"\"\"\"\n"
	findings, resolved, skipped = flagGuardScan("good.formula.toml", good, rootCmd)
	if resolved != 1 || skipped != 0 || len(findings) != 0 {
		t.Fatalf("real flags: resolved=%d skipped=%d findings=%v, want 1, 0 and none", resolved, skipped, findings)
	}

	// A line whose command word cannot resolve is skipped and counted, never
	// guessed at: `$SHELL` in command position names no gt command.
	const unresolved = "```bash\n" +
		"gt $SUBCOMMAND --definitely-not-a-flag\n" +
		"```\n"
	findings, resolved, skipped = flagGuardScan("unresolved.md", unresolved, rootCmd)
	if resolved != 0 || skipped != 1 || len(findings) != 0 {
		t.Fatalf("unresolved command: resolved=%d skipped=%d findings=%v, want 0, 1 and none", resolved, skipped, findings)
	}

	// gt behind a pipe or a command substitution is counted as skipped too, not
	// read as if its line's flags belonged to gt.
	const behindPipe = "```bash\n" +
		"echo x | gt escalate --not-a-real-flag\n" +
		"id=$(gt bead create --not-a-real-flag)\n" +
		"```\n"
	findings, resolved, skipped = flagGuardScan("behind.md", behindPipe, rootCmd)
	if resolved != 0 || skipped != 2 || len(findings) != 0 {
		t.Fatalf("gt behind a pipe or substitution: resolved=%d skipped=%d findings=%v, want 0, 2 and none", resolved, skipped, findings)
	}

	// A continuation carries its flags onto the command that starts before it.
	const continued = "```bash\n" +
		"gt mail send mayor/ -s \"Subject\" \\\n" +
		"  --not-a-real-flag\n" +
		"```\n"
	findings, _, _ = flagGuardScan("continued.md", continued, rootCmd)
	if len(findings) != 1 || findings[0].flag != "--not-a-real-flag" || findings[0].line != 2 {
		t.Fatalf("continuation: findings=%v, want the bogus flag reported on the line the command starts", findings)
	}

	// A piped command's flags belong to the command after the pipe, not to gt.
	const piped = "```bash\n" +
		"gt session status myrig/rust --json | jq -r '.running' | grep -q true\n" +
		"```\n"
	findings, resolved, skipped = flagGuardScan("piped.md", piped, rootCmd)
	if resolved != 1 || skipped != 0 || len(findings) != 0 {
		t.Fatalf("piped command: resolved=%d skipped=%d findings=%v, want 1, 0 and none", resolved, skipped, findings)
	}
}

// TestFlagGuardIgnoresProseAndQuotedFlags: the guard reads fenced shell only.
// A flag named in prose, in a fenced comment, or inside a quoted value is not
// an invocation and must not be reported.
func TestFlagGuardIgnoresProseAndQuotedFlags(t *testing.T) {
	t.Parallel()

	const doc = "The retired `gt sling --prompt` form is gone.\n" +
		"\n" +
		"```bash\n" +
		"# gt sling --prompt x\n" +
		"gt mail send mayor/ -s \"pass --mail-back to keep the thread\"\n" +
		"```\n"

	findings, resolved, skipped := flagGuardScan("prose.md", doc, rootCmd)
	if resolved != 1 || skipped != 0 {
		t.Fatalf("resolved=%d skipped=%d, want 1 and 0", resolved, skipped)
	}
	if len(findings) != 0 {
		t.Fatalf("prose and quoted flags reported: %v", findings)
	}
}
