package polecatimage

import "strings"

// InstalledCommit is the commit the installed gt was built from, or "" when
// `gt version` names no 7-character lowercase hex commit in its last
// parenthesised group.
//
// The group is "(<build>: <commit>)" or "(<build>: <branch>@<commit>)", the
// shape the sandbox backend reads. A read that fails reads as "" rather than an
// error, as the script's pipe to head does.
func (r *Run) InstalledCommit() string {
	return commitOfVersion(r.call(r.Cfg.GT, "version").output())
}

// commitOfVersion reads the commit out of the stdout of `gt version`.
func commitOfVersion(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	open := strings.LastIndex(line, "(")
	if open < 0 {
		return ""
	}
	group := line[open+1:]
	if end := strings.Index(group, ")"); end >= 0 {
		group = group[:end]
	}
	colon := strings.Index(group, ":")
	if colon < 0 {
		return ""
	}
	field := group[colon+1:]
	if at := strings.LastIndex(field, "@"); at >= 0 {
		field = field[at+1:]
	}
	field = strings.ReplaceAll(field, " ", "")
	if !isCommit(field) {
		return ""
	}
	return field
}

// isCommit reports a string that can only name a commit: lowercase hexadecimal
// at least as long as the shortest sha git prints, so it cannot reach a program
// as an option or a revision expression.
func isCommit(s string) bool {
	return len(s) >= 7 && isHex(s)
}

// isHex reports lowercase hexadecimal alone, of any length.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dockerNoValue is what docker's template prints for a field the image does not
// carry, which a missing label is.
const dockerNoValue = "<no value>"

// ImageCommit is the gt commit the image's label records, or "" when the image
// or the label is missing.
//
// Only stdout counts: a docker warning on stderr must not read as a commit.
func (r *Run) ImageCommit() string {
	format := `{{index .Config.Labels "` + r.Cfg.Label + `"}}`
	res := r.call(r.Cfg.Docker, "image", "inspect", "--format", format, r.Cfg.Image)
	label := strings.TrimSpace(res.output())
	if label == dockerNoValue {
		return ""
	}
	return label
}

// SameCommit reports whether two commits name the same one: each at least 7
// characters, and one a prefix of the other, the rule the sandbox backend
// applies.
func SameCommit(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
