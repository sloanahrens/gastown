package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// sandboxGT is where the image keeps gt. The host's settings name the host's
// own binary, which is not in the sandbox.
const sandboxGT = "/usr/local/bin/gt"

// renderSandboxSettings is the host's Claude settings for use inside a
// sandbox: every hook that runs gt keeps its arguments and runs the image's gt;
// the host-only python path guard is dropped (each sandbox has its own
// filesystem and the landlock policy is stricter than the guard); any other
// hook is an error, never a silent drop. Everything outside "hooks" is copied.
func renderSandboxSettings(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("polecat settings: %w", err)
	}
	events, _ := doc["hooks"].(map[string]any)
	for event, groupsAny := range events {
		groups, _ := groupsAny.([]any)
		var keptGroups []any
		for _, g := range groups {
			group, _ := g.(map[string]any)
			hooks, _ := group["hooks"].([]any)
			var kept []any
			for _, h := range hooks {
				hook, _ := h.(map[string]any)
				cmd, isCmd := hook["command"].(string)
				if !isCmd {
					kept = append(kept, h)
					continue
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

// renderHookCommand maps one host hook command to the sandbox: a gt command
// runs the image's gt, the known host-only path guard is dropped, anything
// else is refused so a new host-only hook cannot silently vanish.
func renderHookCommand(cmd string) (rendered string, drop bool, err error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", false, fmt.Errorf("empty command")
	}
	switch base := path.Base(fields[0]); {
	case base == "gt":
		return sandboxGT + strings.TrimPrefix(cmd, fields[0]), false, nil
	case strings.HasPrefix(base, "python") && len(fields) > 1 && path.Base(fields[1]) == "polecat-path-guard.py":
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
