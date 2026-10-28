package polecatimage

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DeferReason is why a rebuild must wait, "" when it may run: the gateway lists
// a sandbox whose registry record does not exist yet, so a retag could land
// while that sandbox is still being prepared.
//
// A probe that cannot answer is a deferral too, so "no sandboxes" and "the
// probe failed" never read alike.
func (r *Run) DeferReason() string {
	list := r.call(r.Cfg.Openshell, "sandbox", "list")
	if list.failed() {
		return fmt.Sprintf("cannot list sandboxes (%s exited %d)", r.Cfg.Openshell, list.exit)
	}
	// The listing is a NAME header and one row per sandbox, or the single line
	// "No sandboxes found."; rows count only after the header. Anything else is
	// a probe that could not answer, and so defers, because a reply this run
	// cannot read must not pass for "no sandboxes".
	if strings.TrimSpace(list.output()) == "No sandboxes found." {
		return ""
	}
	headered := false
	for _, line := range strings.Split(list.output(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "NAME" {
			headered = true
			continue
		}
		if !headered {
			continue
		}
		if !isPlainName(name) {
			return "cannot read the sandbox listing (a sandbox name is not a plain file name)"
		}
		if !isFile(filepath.Join(r.Cfg.SandboxRecords, name+".json")) {
			return "sandbox " + name + " has no registry record yet"
		}
	}
	if !headered {
		return "cannot read the sandbox listing (no NAME header)"
	}
	return ""
}

// isPlainName reports a name that is one path element, so a name the gateway
// printed cannot name a file outside SandboxRecords.
func isPlainName(name string) bool {
	return name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}
