package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// helpDepth is how many words of a command path the walk follows: the deepest
// command bd documents is three (bd mol wisp gc).
const helpDepth = 3

// flagFileMode is the permissions the recorded flags file is written with, the
// same as a golden's: the file is committed, so git decides its tracked mode.
const flagFileMode = 0o600

// flagRowRE matches one flag's definition line in cobra's help, capturing its
// shorthand, its long name, and the type pflag prints after a value flag
// (nothing for a boolean, "string[=x]" for an optional value).
var flagRowRE = regexp.MustCompile(`^\s+(?:-(\w), )?--([\w-]+)(?: ([^\s]+))?`)

// commandRowRE matches one subcommand line in a help section: a lowercase
// name indented two spaces, spaces, then its description. A name longer than
// cobra's padding column (close-eligible) is followed by a single space.
var commandRowRE = regexp.MustCompile(`^  ([a-z][a-z0-9-]*) +(\S.*)$`)

// helpFlags is what one command's help documents.
type helpFlags struct {
	value    []string // flags this command reads a value for
	boolean  []string // flags it reads as a boolean, or a value only after an =
	aliases  []string // the other names this command answers to
	commands []string // the subcommands it lists
	usage    []string // the words of its first Usage: line, after "bd"
	operands []string // the positional placeholders of that line, in order
}

// RecordFlagFile walks bd's help from the root, run answering one argv with
// bd's output and exit code, and returns the contents of
// internal/bdshim/testdata/bd-flags.txt: every command bd reads a value flag
// for, and every command that reads a flag as a boolean although another
// command reads a value for it. The shim's tables are checked against this
// file, so a bd upgrade that moves a flag is caught by re-recording it.
func RecordFlagFile(run Runner) ([]byte, error) {
	version, err := bdVersion(run)
	if err != nil {
		return nil, err
	}
	table := map[string]*helpFlags{}
	var global []string
	var walk func(path []string) error
	walk = func(path []string) error {
		help, err := readHelp(run, path)
		if err != nil {
			return err
		}
		if len(path) == 0 {
			// bd's own Flags: section is the persistent set every command has.
			global = help.value
		} else {
			table[strings.Join(path, " ")] = help
		}
		if len(path) == helpDepth {
			return nil
		}
		for _, command := range help.commands {
			if err := walk(append(append([]string{}, path...), command)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nil); err != nil {
		return nil, err
	}
	return renderFlagFile(version, global, table), nil
}

// recordFlags writes the recorded flag file at opts.out, or with -check
// reports whether the file still matches bd's help without writing. It
// returns the command's exit code.
func recordFlags(opts options, run Runner, stderr io.Writer) int {
	want, err := RecordFlagFile(run)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	if opts.check {
		have, err := os.ReadFile(opts.out)
		if err != nil {
			fmt.Fprintf(stderr, "bdshim-record: reading %s: %v\n", opts.out, err)
			return 1
		}
		if !bytes.Equal(have, want) {
			fmt.Fprintf(stderr, "bdshim-record: %s differs from bd's help; run without -check to record it\n", opts.out)
			return 1
		}
		fmt.Fprintf(stderr, "bdshim-record: %s matches bd's help\n", opts.out)
		return 0
	}
	if err := os.WriteFile(opts.out, want, flagFileMode); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: writing %s: %v\n", opts.out, err)
		return 1
	}
	fmt.Fprintf(stderr, "bdshim-record: wrote %s\n", opts.out)
	return 0
}

// readHelp runs bd's help for one command path and parses it.
func readHelp(run Runner, path []string) (*helpFlags, error) {
	args := append(append([]string{}, path...), "--help")
	out, exit, err := run("", args)
	if err != nil {
		return nil, fmt.Errorf("bd %s: %w", strings.Join(args, " "), err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("bd %s exited %d", strings.Join(args, " "), exit)
	}
	help := parseHelp(string(out), len(path) == 0)
	if len(path) > 0 {
		help.operands = positionals(help.usage, len(path))
	}
	return help, nil
}

// positionals returns the positional placeholders of a Usage: line, the words
// after "bd" and the command's path. The placeholders stop at the first flag or
// at [flags] and [command], which name what follows rather than an operand:
// `bd show [id...] [--id=<id>...] [flags]` has [id...], and
// `bd migrate legacy-sqlite --source-db PATH` has none.
func positionals(usage []string, depth int) []string {
	var out []string
	for _, word := range usage[min(depth, len(usage)):] {
		if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "[-") || word == "[flags]" || word == "[command]" {
			break
		}
		out = append(out, word)
	}
	return out
}

// parseHelp reads one help text: the flags in its own Flags: section, the
// persistent value flags in its Global Flags: section, and the subcommands it
// lists. The root command lists its subcommands under topic headings rather
// than "Available Commands:", so grouped says to read every heading between
// Usage: and Flags:.
func parseHelp(text string, grouped bool) *helpFlags {
	help := &helpFlags{}
	section := ""
	afterUsage := false
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, " ") {
			section = strings.TrimRight(line, " ")
			switch section {
			case "Usage:":
				afterUsage = true
			case "Flags:", "Global Flags:":
				afterUsage = false
			}
			continue
		}
		if section == "Usage:" && help.usage == nil && strings.HasPrefix(strings.TrimSpace(line), "bd ") {
			// The first usage line is the command's own; a later one is an example form.
			help.usage = strings.Fields(line)[1:]
			continue
		}
		if section == "Aliases:" && !strings.HasPrefix(line, "    ") {
			// "  create, new": the command's own name, then its aliases.
			for n, name := range strings.Split(strings.TrimSpace(line), ",") {
				if n > 0 {
					help.aliases = append(help.aliases, strings.TrimSpace(name))
				}
			}
			continue
		}
		if m := flagRowRE.FindStringSubmatch(line); m != nil {
			// Global Flags: repeats the persistent set, which bd's own Flags:
			// section holds, so a subcommand's copy is left out.
			if section == "Flags:" {
				names := flagNames(m)
				if m[3] == "" || strings.Contains(m[3], "[=") {
					help.boolean = append(help.boolean, names...)
				} else {
					help.value = append(help.value, names...)
				}
			}
			continue
		}
		list := section == "Available Commands:" || (grouped && afterUsage && section != "Usage:" && section != "Aliases:" && section != "Examples:")
		if !list {
			continue
		}
		// A root command's grouped sections can hold explanation lines as well
		// ("  human   - Requires manual close"); a description that opens with
		// a dash is prose, not a command.
		if m := commandRowRE.FindStringSubmatch(line); m != nil && m[1] != "bd" && !strings.HasPrefix(m[2], "-") {
			help.commands = append(help.commands, m[1])
		}
	}
	return help
}

// flagNames returns the spellings of one flag: its long form and its shorthand,
// both of which the shim has to recognize.
func flagNames(m []string) []string {
	names := []string{"--" + m[2]}
	if m[1] != "" {
		names = append(names, "-"+m[1])
	}
	return names
}

// bdVersion reads the version bd reports, which the file's heading names so a
// reader knows which bd's help was recorded.
func bdVersion(run Runner) (string, error) {
	out, exit, err := run("", []string{"version"})
	if err != nil {
		return "", fmt.Errorf("bd version: %w", err)
	}
	if exit != 0 {
		return "", fmt.Errorf("bd version exited %d", exit)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[0] != "bd" || fields[1] != "version" {
		return "", fmt.Errorf("bd version printed %q, want a \"bd version <semver>\" line", firstLine(out))
	}
	return fields[2], nil
}

// firstLine returns out's first line, for an error that names what bd printed
// without echoing all of it.
func firstLine(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}

// renderFlagFile writes the rows the shim's tables are checked against.
func renderFlagFile(version string, global []string, table map[string]*helpFlags) []byte {
	sort.Strings(global)
	persistent := make(map[string]bool, len(global))
	for _, flag := range global {
		persistent[flag] = true
	}
	// documented is every flag some command reads a value for: a flag that is
	// a boolean on one command and a value nowhere is not a conflict.
	documented := map[string]bool{}
	for _, flag := range global {
		documented[flag] = true
	}
	commands := make([]string, 0, len(table))
	for command := range table {
		commands = append(commands, command)
		for _, flag := range table[command].value {
			documented[flag] = true
		}
	}
	sort.Strings(commands)

	var b bytes.Buffer
	fmt.Fprintf(&b, `# What bd %s reads a value for, per command: recorded from the help of
# every command `+"`bd --help`"+` lists, three levels deep (the subsections of
# `+"`bd <command> --help`"+`, then theirs). Refresh it after a bd upgrade with
#     go run ./cmd/bdshim-record -flags
# which walks bd's help and writes this file; add -check to report drift
# without writing, and update internal/bdshim/flags.go to match.
#
# persistent<TAB>the value flags bd puts on every command (bd --help, Flags:),
#     left out of each command row because every command has them
# alias<TAB><name><TAB>the command path bd reads that name as ("new" is create)
# operand<TAB><command><TAB>the positional placeholders of the command's usage
#     line, space separated and in order ("[id...]", "[title]"); a command
#     whose usage names none has no row
# value<TAB><command><TAB>the flags bd reads a value for, space separated
# bool<TAB><command><TAB>the flags bd reads as a boolean or an optional value
#     here (--deps takes its value only after an =) although another command
#     reads a value for the same flag
`, version)
	if len(global) > 0 {
		fmt.Fprintf(&b, "persistent\t%s\n", strings.Join(global, " "))
	}
	// An alias takes the place of the last word of its command's path: bd reads
	// "backup add" as "backup init" and "new" as "create".
	aliasPaths := map[string]string{}
	for _, command := range commands {
		words := strings.Fields(command)
		for _, alias := range table[command].aliases {
			path := strings.Join(append(append([]string{}, words[:len(words)-1]...), alias), " ")
			aliasPaths[path] = command
		}
	}
	aliases := make([]string, 0, len(aliasPaths))
	for path := range aliasPaths {
		aliases = append(aliases, path)
	}
	sort.Strings(aliases)
	for _, path := range aliases {
		fmt.Fprintf(&b, "alias\t%s\t%s\n", path, aliasPaths[path])
	}
	for _, command := range commands {
		if operands := table[command].operands; len(operands) > 0 {
			fmt.Fprintf(&b, "operand\t%s\t%s\n", command, strings.Join(operands, " "))
		}
	}
	for _, command := range commands {
		if flags := sortedFlags(table[command].value, persistent, nil); len(flags) > 0 {
			fmt.Fprintf(&b, "value\t%s\t%s\n", command, strings.Join(flags, " "))
		}
	}
	for _, command := range commands {
		if flags := sortedFlags(table[command].boolean, persistent, documented); len(flags) > 0 {
			fmt.Fprintf(&b, "bool\t%s\t%s\n", command, strings.Join(flags, " "))
		}
	}
	return b.Bytes()
}

// sortedFlags returns flags sorted and without repeats, minus the persistent
// ones (a command's own Flags: section can repeat --actor, which bd also
// declares globally) and minus any flag keep rejects.
func sortedFlags(flags []string, persistent, keep map[string]bool) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(flags))
	for _, flag := range flags {
		if persistent[flag] || seen[flag] || (keep != nil && !keep[flag]) {
			continue
		}
		seen[flag] = true
		out = append(out, flag)
	}
	sort.Strings(out)
	return out
}
