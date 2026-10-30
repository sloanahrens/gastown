package polecatimage

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SlingPattern is the pgrep pattern that matches a running `gt sling`, whatever
// path it was invoked by: the daemon cutover's quiet gate asks the same
// question of the same process (gt-qpeei.22).
const SlingPattern = `^([^ ]*/)?gt sling `

// DeferReason is why a rebuild must wait, "" when it may run: a gt sling is
// running, or the gateway lists a sandbox whose registry record does not exist
// yet, so a retag could land between a Prepare's image check and its create.
//
// A probe that cannot answer is a deferral too, so "nothing running" and "the
// probe failed" never read alike.
func (r *Run) DeferReason() string {
	if reason := SlingDeferReason(r.call(r.Cfg.Pgrep, "-f", SlingPattern).exit, r.Cfg.Pgrep); reason != "" {
		return reason
	}
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

// SlingDeferReason is the sling half of DeferReason, given the status pgrep
// exited with for SlingPattern: "" when no gt sling runs, the reason otherwise.
// A pgrep that could not answer defers too, as DeferReason always has.
func SlingDeferReason(pgrepExit int, pgrep string) string {
	switch pgrepExit {
	case 0:
		return "sandbox sling in progress"
	case 1:
		// pgrep ran and matched nothing.
		return ""
	default:
		return fmt.Sprintf("cannot check for a running gt sling (%s exited %d)", pgrep, pgrepExit)
	}
}

// isPlainName reports a name that is one path element, so a name the gateway
// printed cannot name a file outside SandboxRecords.
func isPlainName(name string) bool {
	return name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}
