package gtwrap

import (
	"fmt"
	"strings"
)

// FileArg is the one file argument of a call that reaches the pod on stdin: the
// flag that named it and the path the caller gave, which is a Mac path.
type FileArg struct {
	Flag string
	Path string
}

// argKind says what the kubernetes target does with a flag that names a file.
type argKind int

const (
	// argPlain is a flag that names no Mac file the pod would read.
	argPlain argKind = iota
	// argStdin is a flag the tool reads from stdin when its value is "-".
	argStdin
	// argRefuse is a flag the tool opens as a path with no "-" form: the pod
	// cannot read a Mac path, so the call is refused rather than failing there.
	argRefuse
)

// bdStdinFlags take "-" for stdin under any bd command. The facts are those of
// the beads rig at origin/main 86c717cb1: readBodyFile (cmd/bd/flags.go:188-195)
// serves the first three, bd close reads --reason-file through it (close.go:641).
var bdStdinFlags = map[string]bool{
	"--body-file": true, "--description-file": true, "--design-file": true, "--reason-file": true,
}

// bdFileCommands are the commands whose -f/--file takes "-": comments add, note
// and human respond go through textSources (flags.go:264-266), and dep add reads
// "-" itself (dep.go:570). Keys are "command" or "command subcommand".
var bdFileCommands = map[string]bool{
	"comments add": true, "note": true, "human respond": true, "dep add": true,
}

// classify says what the call does with flag. command is the first non-flag
// word of the arguments and sub the second; value is the flag's value, for
// --metadata, which names a file only as "@file".
func classify(tool, command, sub, flag, value string) argKind {
	switch tool {
	case "bd":
		return classifyBd(command, sub, flag, value)
	case "gt":
		switch {
		case command == "spec" && sub == "lint" && flag == "--template",
			command == "session" && sub == "inject" && (flag == "-f" || flag == "--file"),
			command == "sandbox" && sub == "endpoint" && flag == "--from-export":
			return argRefuse
		}
	}
	return argPlain
}

func classifyBd(command, sub, flag, value string) argKind {
	if bdStdinFlags[flag] {
		return argStdin
	}
	switch {
	case flag == "-f" || flag == "--file":
		if bdFileCommands[command] || bdFileCommands[command+" "+sub] {
			return argStdin
		}
		if command == "create" {
			return argRefuse
		}
	case flag == "--graph" && command == "create",
		flag == "--metadata" && (command == "create" || command == "update") && strings.HasPrefix(value, "@"),
		flag == "--from-file" && command == "delete",
		flag == "--ids-file" && command == "migrate",
		(flag == "-i" || flag == "--input") && command == "import",
		(flag == "-o" || flag == "--output") && command == "export":
		return argRefuse
	}
	return argPlain
}

// ScanFileArgs finds the Mac files a bd or gt call names and rewrites the ones
// the tool can read from stdin. It returns the arguments with that file's value
// replaced by "-" and the file, or the arguments unchanged and a nil file when
// the call names none. The pod cannot read a Mac path, so a call is refused
// when it names a file the tool opens as a path, two files, or a file together
// with a stdin the caller already supplied by --stdin or a "-" value. Other
// tools pass unchanged.
//
// The command is the first non-flag word and the subcommand the second, so a
// global flag that takes a value must come after them.
func ScanFileArgs(tool string, args []string) (rewritten []string, file *FileArg, err error) {
	if tool != "bd" && tool != "gt" {
		return args, nil, nil
	}
	var words []string
	for _, a := range args {
		if len(words) == 2 {
			break
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	command, sub := "", ""
	if len(words) > 0 {
		command = words[0]
	}
	if len(words) > 1 {
		sub = words[1]
	}

	out := append([]string(nil), args...)
	var files []FileArg
	stdinUsed := false
	for i := 0; i < len(out); i++ {
		flag, value, inline := out[i], "", false
		if strings.HasPrefix(flag, "--") {
			if name, v, ok := strings.Cut(flag, "="); ok {
				flag, value, inline = name, v, true
			}
		}
		if !inline && i+1 < len(out) {
			value = out[i+1]
		}
		if flag == "--stdin" && tool == "bd" {
			stdinUsed = true
			continue
		}
		switch classify(tool, command, sub, flag, value) {
		case argRefuse:
			return nil, nil, fmt.Errorf("gt-wrap: %s names a Mac file the pod cannot read; pass the content inline or on stdin if the tool takes it", flag)
		case argStdin:
			if value == "-" {
				stdinUsed = true
			} else if value != "" {
				files = append(files, FileArg{Flag: flag, Path: value})
				if inline {
					out[i] = flag + "=-"
				} else {
					out[i+1] = "-"
				}
			}
			if !inline {
				i++
			}
		}
	}
	switch {
	case len(files) > 1:
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Flag + " " + f.Path
		}
		return nil, nil, fmt.Errorf("gt-wrap: the call names two files (%s) and stdin carries one; pass one as text and run the other in a second call", strings.Join(names, ", "))
	case len(files) == 1 && stdinUsed:
		return nil, nil, fmt.Errorf("gt-wrap: %s %s cannot be sent because stdin is already in use", files[0].Flag, files[0].Path)
	case len(files) == 1:
		return out, &files[0], nil
	}
	return args, nil, nil
}
