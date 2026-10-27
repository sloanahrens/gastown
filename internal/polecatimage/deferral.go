package polecatimage

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DeferReason is why a rebuild must wait, "" when it may run: a gt sling is
// running, or the gateway lists a sandbox whose registry record does not exist
// yet, so a retag could land between a Prepare's image check and its create.
//
// A probe that cannot answer is a deferral too, so "nothing running" and "the
// probe failed" never read alike.
func (r *Run) DeferReason() string {
	sling := r.call(r.Cfg.Pgrep, "-f", `^([^ ]*/)?gt sling `)
	switch sling.exit {
	case 0:
		return "sandbox sling in progress"
	case 1:
		// pgrep ran and matched nothing.
	default:
		return fmt.Sprintf("cannot check for a running gt sling (%s exited %d)", r.Cfg.Pgrep, sling.exit)
	}
	list := r.call(r.Cfg.Openshell, "sandbox", "list")
	if list.failed() {
		return fmt.Sprintf("cannot list sandboxes (%s exited %d)", r.Cfg.Openshell, list.exit)
	}
	// The listing is a NAME header and one row per sandbox, or the single line
	// "No sandboxes found."; rows count only after the header.
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
		if !isFile(filepath.Join(r.Cfg.SandboxRecords, name+".json")) {
			return "sandbox " + name + " has no registry record yet"
		}
	}
	return ""
}
