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

// bdStdinFlags take "-" for stdin under any bd command: bd reads them through
// one shared reader that treats "-" as stdin. The beads source that shows this,
// with file and line, is recorded on gt-msc5w8.
var bdStdinFlags = map[string]bool{
	"--body-file": true, "--description-file": true, "--design-file": true, "--reason-file": true,
}

// bdFileCommands are the commands whose -f/--file takes "-" (same source, same
// record on gt-msc5w8). Keys are "command" or "command subcommand".
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

// valueFlags take their value from the next argument when it is not attached.
// Their values are skipped when the command words are read, so a path is never
// taken for a subcommand.
var valueFlags = map[string]bool{
	"--body-file": true, "--description-file": true, "--design-file": true, "--reason-file": true,
	"-f": true, "--file": true, "--graph": true, "--metadata": true, "--from-file": true,
	"--ids-file": true, "-i": true, "--input": true, "-o": true, "--output": true,
	"--template": true, "--from-export": true,
}

// fileShorthands are the one-letter file flags of the command, for the attached
// forms pflag accepts: "-fFILE", "-f=FILE" and a group such as "-xf FILE".
func fileShorthands(tool, command, sub string) string {
	switch {
	case tool == "bd" && (bdFileCommands[command] || bdFileCommands[command+" "+sub] || command == "create"):
		return "f"
	case tool == "bd" && command == "import":
		return "i"
	case tool == "bd" && command == "export":
		return "o"
	case tool == "gt" && command == "session" && sub == "inject":
		return "f"
	}
	return ""
}

// ScanFileArgs finds the Mac files a bd or gt call names and rewrites the ones
// the tool can read from stdin. It returns the arguments with that file's value
// replaced by "-" and the file, or the arguments unchanged and a nil file when
// the call names none. The pod cannot read a Mac path, so a call is refused
// when it names a file the tool opens as a path, two files, a file together
// with a stdin the caller already supplied by --stdin or a "-" value, or a
// token that cannot be classified safely. Other tools pass unchanged.
//
// pflag lets a file flag carry its value in these forms, all scanned here:
// "--flag value", "--flag=value", "-f value", "-f=value" and "-fvalue". A group
// of shorthands that holds a file letter after the first ("-xf value",
// "-xfvalue", "-xf=value") cannot be told from a value-taking shorthand's value
// without knowing every shorthand of the tool, so the call is refused: it fails
// closed rather than passing a Mac path through. Nothing after "--" is a flag.
//
// The command is the first non-flag word and the subcommand the second, not
// counting the values of the file flags above, so any other global flag that
// takes a value must come after them.
func ScanFileArgs(tool string, args []string) (rewritten []string, file *FileArg, err error) {
	if tool != "bd" && tool != "gt" {
		return args, nil, nil
	}
	var words []string
	for i := 0; i < len(args) && len(words) < 2; i++ {
		a := args[i]
		switch {
		case a == "--":
			i = len(args)
		case valueFlags[a]:
			i++
		case !strings.HasPrefix(a, "-"):
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
	shorthands := fileShorthands(tool, command, sub)

	out := append([]string(nil), args...)
	var files []FileArg
	stdinUsed := false
	for i := 0; i < len(out); i++ {
		if out[i] == "--" {
			break
		}
		flag, value, inline, sep := out[i], "", false, ""
		switch {
		case strings.HasPrefix(flag, "--"):
			if name, v, ok := strings.Cut(flag, "="); ok {
				flag, value, inline, sep = name, v, true, "="
			}
		case strings.HasPrefix(flag, "-") && len(flag) > 2:
			name, _, _ := strings.Cut(flag[1:], "=")
			at := -1
			if shorthands != "" {
				at = strings.IndexAny(name, shorthands)
			}
			switch {
			case at == 0:
				flag, value, inline = flag[:2], out[i][2:], true
				if rest, ok := strings.CutPrefix(value, "="); ok {
					value, sep = rest, "="
				}
			case at > 0:
				return nil, nil, fmt.Errorf("gt-wrap: %s groups shorthands with the file flag -%c; pass them as separate flags", out[i], name[at])
			default:
				if n, v, ok := strings.Cut(flag, "="); ok {
					flag, value, inline, sep = n, v, true, "="
				}
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
					out[i] = flag + sep + "-"
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
