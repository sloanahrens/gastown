package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// sandboxGT is where the image keeps gt. The host's settings name the host's
// own binary, which is not in the sandbox.
const sandboxGT = "/usr/local/bin/gt"

// hostPathGuard is the host's python path guard. The sandbox does not need it:
// each sandbox has its own filesystem and the landlock policy is stricter than
// the guard.
const hostPathGuard = "/.claude/tools/polecat-path-guard.py"

// pythonInterpreterRE matches a python interpreter as a settings file names one —
// python, python3, python3.11 — and not a program that merely begins with
// python.
var pythonInterpreterRE = regexp.MustCompile(`^python(3(\.\d+)*)?$`)

// renderSandboxSettings is the host's Claude settings for use inside a
// sandbox: every hook that runs gt keeps its arguments and runs the image's gt;
// the host-only python path guard is dropped; any other hook is an error, never
// a silent drop. Everything outside "hooks" is copied.
//
// A hooks structure it cannot inspect is an error for the same reason: a
// malformed file must not pass host paths into the sandbox or lose hooks
// without saying so.
func renderSandboxSettings(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("polecat settings: %w", err)
	}
	events := map[string]any{}
	if hooks, present := doc["hooks"]; present {
		object, ok := hooks.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("polecat settings: hooks is not an object of events")
		}
		events = object
	}
	for event, groupsAny := range events {
		groups, ok := groupsAny.([]any)
		if !ok {
			return nil, fmt.Errorf("polecat settings: hooks %s is not a list of hook groups", event)
		}
		var keptGroups []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("polecat settings: hooks %s holds a group that is not an object", event)
			}
			hooks, ok := group["hooks"].([]any)
			if !ok {
				return nil, fmt.Errorf("polecat settings: hooks %s holds a group without a list of hooks", event)
			}
			var kept []any
			for _, h := range hooks {
				hook, ok := h.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("polecat settings: hooks %s holds a hook that is not an object", event)
				}
				cmd, ok := hook["command"].(string)
				if !ok {
					return nil, fmt.Errorf("polecat settings: hooks %s holds a hook with no command; render it explicitly or remove it", event)
				}
				rendered, drop, err := renderHookCommand(cmd)
				if err != nil {
					return nil, fmt.Errorf("polecat settings: hook %s: %w", event, err)
				}
				if drop {
					continue
				}
				hook["command"] = rendered
				kept = append(kept, hook)
			}
			if len(kept) == 0 {
				continue
			}
			group["hooks"] = kept
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(events, event)
			continue
		}
		events[event] = keptGroups
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// renderHookCommand maps one host hook command to the sandbox: a command that
// runs gt runs the image's gt, the host-only path guard is dropped, anything
// else is refused so a new host-only hook cannot silently vanish.
func renderHookCommand(cmd string) (rendered string, drop bool, err error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", false, fmt.Errorf("empty command")
	}
	// The program word is the first field; the argv after it passes through as
	// written, whitespace and quoting included.
	argv := cmd[len(cmd)-len(strings.TrimLeftFunc(cmd, unicode.IsSpace))+len(fields[0]):]
	switch program := path.Base(fields[0]); {
	// A program named gt is the town's binary whatever directory the host keeps
	// it in, so the base name decides.
	case program == "gt":
		return sandboxGT + argv, false, nil
	case pythonInterpreterRE.MatchString(program) && len(fields) > 1 && strings.HasSuffix(fields[1], hostPathGuard):
		return "", true, nil
	}
	return "", false, fmt.Errorf("%q runs a host-only program the sandbox does not have; render it explicitly or remove it", cmd)
}

// RewriteTownRoot replaces the host town root with the sandbox's wherever it
// stands as a whole path in s: after the start, whitespace, a quote, '=', ':',
// '`' or '(', and before '/', whitespace, a quote, ':', '`', ')' or the end. A
// longer name that merely begins with the root (/h/gt-other) and a path that
// merely contains it (/x/h/gt) are left alone.
func RewriteTownRoot(s, from, to string) string {
	if from == "" || from == to {
		return s
	}
	before := func(i int) bool { return i == 0 || strings.ContainsRune(" \t\n'\"=:`(", rune(s[i-1])) }
	after := func(end int) bool { return end == len(s) || strings.ContainsRune("/ \t\n'\":`)", rune(s[end])) }
	var b strings.Builder
	rest := s
	offset := 0
	for {
		i := strings.Index(rest, from)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		at, end := offset+i, offset+i+len(from)
		if before(at) && after(end) {
			b.WriteString(rest[:i])
			b.WriteString(to)
		} else {
			b.WriteString(rest[:i+len(from)])
		}
		rest = rest[i+len(from):]
		offset = end
	}
}
