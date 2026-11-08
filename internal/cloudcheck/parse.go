package cloudcheck

import (
	"bufio"
	"io"
	"net/url"
	"strings"
)

const (
	// maxAppsLineBytes refuses a line too long to be one check instead of
	// reading it as a truncated one.
	maxAppsLineBytes = 64 << 10
	// maxAppNameRunes bounds a name that becomes part of a path: <name>.off.
	maxAppNameRunes = 64
)

// ParseApps reads the apps list: one check per name=url line, with # comments
// and blank lines, and an optional app=<name> line naming the section that
// follows. It returns the checks in file order, the defects it reports, and
// the reader error that stopped the parse.
func ParseApps(r io.Reader) ([]App, []Problem, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4<<10), maxAppsLineBytes)
	apps := []App{}
	problems := []Problem{}
	section := ""
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			problems = append(problems, Problem{Level: LevelWarn, Line: line, Reason: "expected name=url"})
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "app" {
			if value == "" {
				// ops/cloud-check.sh reports an empty app= as a bad line.
				problems = append(problems, Problem{Level: LevelWarn, Line: line, Reason: "has an empty app= field"})
				continue
			}
			if !plainName(value) {
				problems = append(problems, Problem{Level: LevelWarn, Line: line, Reason: "app= names no plain app name"})
				continue
			}
			section = value
			continue
		}
		if !plainName(key) {
			problems = append(problems, Problem{Level: LevelWarn, Line: line, Reason: "the check name is not a plain name"})
			continue
		}
		if !httpURL(value) {
			problems = append(problems, Problem{Level: LevelWarn, Line: line, Reason: "the URL is not an absolute http or https URL"})
			continue
		}
		apps = append(apps, App{Section: section, Name: key, URL: value})
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(apps) == 0 {
		// A run that checks nothing must not report success.
		problems = append(problems, Problem{Level: LevelFail, Line: 0, Reason: "names no apps"})
	}
	return apps, problems, nil
}

// plainName reports whether s is safe as part of the .off marker path and as a
// displayed name: a short run of letters, digits, '-', '_' and '.'.
func plainName(s string) bool {
	if s == "" || len([]rune(s)) > maxAppNameRunes || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// httpURL reports whether s is an absolute URL with an http or https scheme
// and a host: the only kind the command may fetch.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
