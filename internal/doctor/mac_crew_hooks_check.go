package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MacCrewHooksCheckName is the name 'gt doctor --check' takes for MacCrewHooksCheck.
const MacCrewHooksCheckName = "mac-crew-hooks"

// podEnv is the variable the kubelet sets in every pod, the same signal
// internal/cmd's podEnv and internal/sweeprunner's InClusterHost read.
const podEnv = "KUBERNETES_SERVICE_HOST"

// macCrewHooksNativeHint sends the reader to the run that sees the Mac clones.
const macCrewHooksNativeHint = "Run 'gt doctor --check mac-crew-hooks' on a native gt on the Mac, where the crew clones live"

// MacCrewHooksCheck finds a crew clone on the Mac whose core.hooksPath points
// nowhere. Git then silently skips the hooks, so a guard such as the pre-push
// hook does not run in that clone and nothing reports it.
//
// It reads each <town>/<rig>/crew/<name> main clone's .git/config directly, with
// no git subprocess, so it runs on a native gt on the Mac. A clone whose .git is
// a file is a linked worktree that shares its main clone's config; it is not
// scanned on its own. The check never writes: each finding names the repair.
//
// A run inside a pod reports a warning rather than a pass when it finds nothing,
// because the pod sees only the clones mounted there, not the Mac ones
// (gt-hhkgui).
type MacCrewHooksCheck struct {
	BaseCheck
	// getenv reads the environment; nil reads os.Getenv. A test sets it so its
	// result does not depend on whether the test process runs inside a pod.
	getenv func(string) string
}

// NewMacCrewHooksCheck creates the check.
func NewMacCrewHooksCheck() *MacCrewHooksCheck {
	return &MacCrewHooksCheck{
		BaseCheck: BaseCheck{
			CheckName:        MacCrewHooksCheckName,
			CheckDescription: "Report a crew clone whose core.hooksPath points at a missing directory or whose .git/config cannot be read",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run scans every main crew clone under the town root.
func (c *MacCrewHooksCheck) Run(ctx *CheckContext) *CheckResult {
	rigs, err := os.ReadDir(ctx.TownRoot)
	if err != nil {
		return c.result(StatusError, "Cannot list the town root "+ctx.TownRoot,
			[]string{err.Error()}, "Check that the town root is readable and re-run the check")
	}

	var clones []string
	crewEntries := 0
	for _, rig := range rigs {
		if !rig.IsDir() {
			continue
		}
		crewDir := filepath.Join(ctx.TownRoot, rig.Name(), "crew")
		entries, err := os.ReadDir(crewDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return c.result(StatusError, "Cannot list the crew directory of rig "+rig.Name(),
				[]string{err.Error()}, "Make "+crewDir+" a readable directory and re-run the check")
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			crewEntries++
			clone := filepath.Join(crewDir, entry.Name())
			isMain, err := isDir(filepath.Join(clone, ".git"))
			if err != nil {
				return c.result(StatusError, "Cannot read the .git entry of crew clone "+clone,
					[]string{err.Error()}, "Make the .git entry readable and re-run the check")
			}
			if isMain {
				clones = append(clones, clone)
			}
		}
	}

	if crewEntries == 0 {
		return c.result(StatusWarning,
			"no crew clones are visible here: run this check on a native gt on the Mac",
			nil, "Run 'gt doctor --check mac-crew-hooks' from a gt built from main on the Mac, where the crew clones live")
	}

	sort.Strings(clones)
	var findings []string
	for _, clone := range clones {
		if finding := checkCrewClone(clone); finding != "" {
			findings = append(findings, finding)
		}
	}
	if len(findings) > 0 {
		return c.result(StatusError,
			fmt.Sprintf("%d crew clone(s) have a core.hooksPath that does not work", len(findings)),
			findings, "Apply the repair named on each line, then re-run the check")
	}
	if c.inPod() {
		return c.result(StatusWarning,
			fmt.Sprintf("ran inside a pod: the %d crew clone(s) visible here are not the Mac crew clones, which this run cannot see", len(clones)),
			nil, macCrewHooksNativeHint)
	}
	return c.result(StatusOK,
		fmt.Sprintf("All %d main crew clone(s) have an unset or existing core.hooksPath", len(clones)), nil, "")
}

// inPod reports whether the check runs in a pod, whose scan sees only the clones
// mounted there (gt-hhkgui).
func (c *MacCrewHooksCheck) inPod() bool {
	getenv := c.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv(podEnv) != ""
}

func (c *MacCrewHooksCheck) result(status CheckStatus, msg string, details []string, hint string) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  status,
		Message: msg,
		Details: details,
		FixHint: hint,
	}
}

// checkCrewClone returns the finding for one main clone, or "" when its
// core.hooksPath is unset or names an existing directory.
func checkCrewClone(clone string) string {
	data, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if err != nil {
		return fmt.Sprintf("%s: cannot read .git/config: %v", clone, err)
	}
	value, set, err := readCoreHooksPath(data)
	if err != nil {
		return fmt.Sprintf("%s: cannot read .git/config: %v", clone, err)
	}
	if !set {
		return ""
	}

	target := value
	if !filepath.IsAbs(value) {
		target = filepath.Join(clone, filepath.FromSlash(value))
	}
	info, err := os.Stat(target)
	if err == nil && info.IsDir() {
		return ""
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s: cannot read core.hooksPath %s: %v", clone, value, err)
	}

	hasGithooks, err := isDir(filepath.Join(clone, ".githooks"))
	if err != nil {
		return fmt.Sprintf("%s: cannot read .githooks: %v", clone, err)
	}
	worktrees, err := countLinkedWorktrees(clone)
	shared := fmt.Sprintf("%d linked worktree(s) share it", worktrees)
	if err != nil {
		shared = fmt.Sprintf("linked worktree count unknown: %v", err)
	}
	return fmt.Sprintf("%s: dangling core.hooksPath %s (%s); repair: %s",
		clone, value, shared, crewCloneRepair(clone, hasGithooks))
}

// crewCloneRepair names the repair for a dangling hooksPath: point it at the
// tree's .githooks when the tree has one, otherwise unset it.
func crewCloneRepair(clone string, hasGithooks bool) string {
	if hasGithooks {
		return fmt.Sprintf("git -C %s config core.hooksPath .githooks", clone)
	}
	return fmt.Sprintf("git -C %s config --unset core.hooksPath", clone)
}

// countLinkedWorktrees counts the entries of the main clone's .git/worktrees. A
// clone with no linked worktrees has no such directory.
func countLinkedWorktrees(clone string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(clone, ".git", "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return len(entries), err
}

// isDir reports whether p is a directory. A path that does not exist is false;
// any other error is returned, never read as absent.
func isDir(p string) (bool, error) {
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// readCoreHooksPath returns the value of core.hooksPath in a git config file and
// whether the file sets it. Section and key names match case-insensitively. A
// line it cannot parse is an error, never an unset key, so a file it cannot
// read is reported rather than passed.
func readCoreHooksPath(data []byte) (string, bool, error) {
	var value string
	set := false
	inSection, inCore := false, false
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			name, hasSub, err := parseConfigSectionHeader(line)
			if err != nil {
				return "", false, fmt.Errorf("line %d: %w", n, err)
			}
			inSection = true
			inCore = !hasSub && strings.EqualFold(name, "core")
			continue
		}

		key, val, hasEq := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !validConfigKey(key) {
			return "", false, fmt.Errorf("line %d: %q is not a key", n, key)
		}
		if !inSection {
			return "", false, fmt.Errorf("line %d: key %s is outside any section", n, key)
		}
		parsed, err := parseConfigValue(strings.TrimLeft(val, " \t"))
		if err != nil {
			return "", false, fmt.Errorf("line %d: %w", n, err)
		}
		if !inCore || !strings.EqualFold(key, "hooksPath") {
			continue
		}
		if !hasEq || parsed == "" {
			return "", false, fmt.Errorf("line %d: core.hooksPath has no value", n)
		}
		value, set = parsed, true
	}
	return value, set, nil
}

// parseConfigSectionHeader parses a line that starts with '[' and reports the
// section name and whether it has a quoted subsection. Only a comment may follow
// the closing ']'.
func parseConfigSectionHeader(line string) (string, bool, error) {
	s := line[1:]
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	if end < 0 {
		end = len(s)
	}
	if end == 0 {
		return "", false, errors.New("section header has no valid name")
	}
	name := s[:end]
	s = strings.TrimLeft(s[end:], " \t")

	hasSub := false
	if strings.HasPrefix(s, `"`) {
		hasSub = true
		_, rest, err := scanConfigQuoted(s)
		if err != nil {
			return "", false, fmt.Errorf("section header subsection: %w", err)
		}
		s = strings.TrimLeft(rest, " \t")
	}
	if !strings.HasPrefix(s, "]") {
		return "", false, errors.New("unterminated section header")
	}
	rest := strings.TrimSpace(s[1:])
	if rest != "" && rest[0] != '#' && rest[0] != ';' {
		return "", false, errors.New("text after a section header is not read")
	}
	return name, hasSub, nil
}

// scanConfigQuoted reads a double-quoted run that starts at s[0]. It returns the
// unescaped content and the text after the closing quote.
func scanConfigQuoted(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			i++
			if i == len(s) {
				return "", "", errors.New("quoted text ends in a backslash")
			}
			esc, err := unescapeConfigByte(s[i])
			if err != nil {
				return "", "", err
			}
			b.WriteByte(esc)
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", errors.New("unterminated quote")
}

// parseConfigValue reads a key's value: quoted runs and escapes are unwrapped,
// text from an unquoted '#' or ';' is a comment, and unquoted trailing blanks are
// dropped.
func parseConfigValue(raw string) (string, error) {
	var out, pending strings.Builder
	quoted := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\':
			i++
			if i == len(raw) {
				return "", errors.New("value ends in a backslash: line continuation is not read")
			}
			esc, err := unescapeConfigByte(raw[i])
			if err != nil {
				return "", err
			}
			out.WriteString(pending.String())
			pending.Reset()
			out.WriteByte(esc)
		case c == '"':
			quoted = !quoted
			out.WriteString(pending.String())
			pending.Reset()
		case !quoted && (c == '#' || c == ';'):
			return out.String(), nil
		case !quoted && (c == ' ' || c == '\t'):
			pending.WriteByte(c)
		default:
			out.WriteString(pending.String())
			pending.Reset()
			out.WriteByte(c)
		}
	}
	if quoted {
		return "", errors.New("unterminated quote")
	}
	return out.String(), nil
}

// unescapeConfigByte maps the byte after a backslash in a config file.
func unescapeConfigByte(c byte) (byte, error) {
	switch c {
	case 'n':
		return '\n', nil
	case 't':
		return '\t', nil
	case 'b':
		return '\b', nil
	case '"', '\\':
		return c, nil
	}
	return 0, fmt.Errorf("unknown escape \\%c", c)
}

// validConfigKey reports whether a key is a letter followed by letters, digits
// or dashes, the only key names git accepts.
func validConfigKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		alpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		digitOrDash := (r >= '0' && r <= '9') || r == '-'
		if i == 0 && !alpha {
			return false
		}
		if !alpha && !digitOrDash {
			return false
		}
	}
	return true
}
