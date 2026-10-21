package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The bead the tier sweep files for a red tier is born shaped: the spec
// dispatcher holds an unshaped bead, so a bead carrying only the one-line
// detail waits for an overseer to describe it before a polecat ever sees it
// (gt-1lzy1). tierSweepBeadShape writes the sections the dispatcher's lint
// reads, and an acceptance item a polecat can tick on its own: the narrow
// `go test -run` for the tests the tier log names, or the tier's own script
// for the shell tier.

const (
	// tierSweepBeadIntro opens every sweep bead's description, before the
	// one-line detail. It is the sentence the beads filed before the shape
	// carried, kept so the filer stays named in the bead.
	tierSweepBeadIntro = "The daemon's tier sweep found this red on main (gt-vsct7.5). "

	// tierSweepBeadSize is the D10 Size section: the shape lint refuses a
	// Size that does not say one worker.
	tierSweepBeadSize = "One worker, one landing."

	// tierSweepGoBuildTag is the build tag the tagged Go tiers are built with
	// (scripts/tier-sweep.sh runs `go test -tags integration` for the tagged
	// integration list and the race tier).
	tierSweepGoBuildTag = "integration"

	// tierSweepLogReadLimit caps what the shape reads from a tier log. The
	// stage output is a marker per unit and five fail lines per failing unit,
	// so a file past this size is not the log the sweep wrote, and reading it
	// whole would cost the daemon's memory to learn nothing.
	tierSweepLogReadLimit = 4 << 20
)

// tierSweepUnitRE is what a failing unit from a summary line must look like
// before it is written into a check command: a package path or a repo-relative
// script path, never a flag. The first character excludes "-" so a string like
// "-race" cannot pass as a unit. tierSweepUnitIsName adds the "it is a path"
// half of the test, which the sweep's stand-ins fail.
var tierSweepUnitRE = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/~-]*$`)

// tierSweepUnitIsName reports whether unit is a package or script path the
// script can have run, rather than one of the reasons the sweep writes when it
// could not name one. Every real unit is "./pkg" or a repo-relative script
// path and holds a "/"; the stand-ins ("(no failing unit named)", "timeout",
// "no verdict line (exit 1)") do not.
func tierSweepUnitIsName(unit string) bool {
	return tierSweepUnitRE.MatchString(unit) && strings.Contains(unit, "/")
}

// tierSweepTestNameRE is a top-level Go test name. A subtest's own --- FAIL
// line repeats the parent's name with a "/" suffix, and a name that is not an
// identifier never goes into a -run filter.
var tierSweepTestNameRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// tierSweepFailLineRE matches a failing test's line in a tier log, indented or
// not (go test indents a subtest's line). The name runs to the first space,
// before the elapsed time.
var tierSweepFailLineRE = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)

// tierSweepLogUnits splits a tier log into the failing test names each unit's
// marker opened: the script prints "tier-sweep: <tier> <unit> ..." before
// running a unit, then the failing lines it echoed from that unit's own log.
// The names are keyed by marker tier, because the integration stage runs both
// its package lists under the one daemon log and the same package can fail in
// the integration and the race tier for different reasons.
//
// A failing line before any marker is dropped: without the marker there is no
// saying which unit failed it, and attributing every line to one unit makes a
// -run filter that matches nothing in that package, which passes vacuously.
func tierSweepLogUnits(log string) map[string][]string {
	units := map[string][]string{}
	var current string
	for _, line := range strings.Split(log, "\n") {
		if tier, unit, ok := tierSweepLogMarker(line); ok {
			current = tier + "\x00" + unit
			continue
		}
		m := tierSweepFailLineRE.FindStringSubmatch(line)
		if m == nil || current == "" {
			continue
		}
		name := m[1]
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		if !tierSweepTestNameRE.MatchString(name) {
			continue
		}
		units[current] = append(units[current], name)
	}
	return units
}

// tierSweepLogMarker reads the script's per-unit line,
// "tier-sweep: <tier> <unit> ...". It is not the summary line, which ends in
// its elapsed time.
func tierSweepLogMarker(line string) (tier, unit string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "tier-sweep: ") || !strings.HasSuffix(line, " ...") {
		return "", "", false
	}
	fields := strings.Fields(line)
	if len(fields) != 4 {
		return "", "", false
	}
	return fields[1], fields[2], true
}

// tierSweepLogMarkerTiers are the marker tiers whose failures belong to tier:
// the integration tier runs its tagged packages and then the untagged docker
// ones under their own marker, while both are reported as integration.
func tierSweepLogMarkerTiers(tier string) []string {
	if tier == "integration" {
		return []string{"integration", "integration-docker"}
	}
	return []string{tier}
}

// tierSweepMarkerBuildTag is the build tag the script ran a marker tier's
// packages with. The docker list is run untagged, so a re-run that kept
// -tags integration would compile a different file set than the run that
// failed.
func tierSweepMarkerBuildTag(mtier string) string {
	if mtier == "integration-docker" {
		return ""
	}
	return tierSweepGoBuildTag
}

// tierSweepReadLog reads at most tierSweepLogReadLimit bytes of a tier log. A
// log cut by the limit still names the units it reached; the rest fall back.
// The cut is moved back to the last newline, because half a fail line names a
// test that does not exist and a -run filter over it passes without running
// anything.
func tierSweepReadLog(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the sweep's own log path, recorded by tierSweepWriteLog
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, tierSweepLogReadLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > tierSweepLogReadLimit {
		data = data[:tierSweepLogReadLimit]
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else {
			data = nil
		}
	}
	return string(data), nil
}

// tierSweepUnitFailures reads the tier log and returns the top-level tests the
// tier's runs of unit failed, in log order and deduplicated, with the build
// tag those runs used. A log that cannot be read names none: filing a shaped
// bead must not depend on the log still existing. A unit the script ran both
// tagged and untagged has no single command that re-runs it, so it names none
// either.
func tierSweepUnitFailures(logPath, tier, unit string) (names []string, buildTag string) {
	if strings.TrimSpace(logPath) == "" {
		return nil, ""
	}
	log, err := tierSweepReadLog(logPath)
	if err != nil {
		return nil, ""
	}
	units := tierSweepLogUnits(log)
	var tag string
	tagged := false
	for _, mtier := range tierSweepLogMarkerTiers(tier) {
		got := units[mtier+"\x00"+unit]
		if len(got) == 0 {
			continue
		}
		mtag := tierSweepMarkerBuildTag(mtier)
		if tagged && mtag != tag {
			return nil, ""
		}
		tag, tagged = mtag, true
		names = append(names, got...)
	}
	return tierSweepUnique(names), tag
}

// tierSweepUnique drops repeats while keeping the log's order.
func tierSweepUnique(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// tierSweepCheckCommand is the narrow command that re-runs one failing unit:
// the tier's own script for the shell tier, and `go test` filtered to the
// failing tests for a Go tier. A filtered run is what the polecat test guard
// allows on a heavy package, and a whole package is what it refuses. The Go
// command keeps the sweep's own wrapper: GT_TEST_DOCKER=1, or the container
// tests skip and a green tick proves nothing, and gt slot run around it, or
// the container-suite guard refuses it. Empty when no command can be named
// from what the sweep knows, so the caller falls back to the log.
func tierSweepCheckCommand(tier, unit string, ts tierSweepTierResult) string {
	if !tierSweepUnitIsName(unit) {
		return ""
	}
	if tier == "shell" {
		return "GT_TEST_DOCKER=0 bash " + unit
	}
	names, tag := tierSweepUnitFailures(ts.Log, tier, unit)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("gt slot run -- env GT_TEST_DOCKER=1 go test ")
	if tier == "race" {
		// The race tier is the only reason a race regression is seen at all,
		// so its re-run keeps -race.
		b.WriteString("-race ")
	}
	if tag != "" {
		b.WriteString("-tags " + tag + " ")
	}
	b.WriteString("-run '^(")
	b.WriteString(strings.Join(names, "|"))
	b.WriteString(")$' -count=1 ")
	b.WriteString(unit)
	return b.String()
}

// tierSweepUnitSubject renders the failing unit for the bead's prose. A
// stand-in name the sweep writes when it could not name a unit ("no verdict
// line (exit 1)") is the sweep's own explanation, not a package or script, so
// the prose names the tier instead of quoting the stand-in as a unit.
func tierSweepUnitSubject(tier, unit string) string {
	if tierSweepUnitIsName(unit) {
		return unit
	}
	return "the " + tier + " tier"
}

// tierSweepAcceptanceItem is the one criterion a polecat ticks: the narrow
// re-run of the failing unit. When the sweep cannot name a command (a missing
// log, no failing test line, a unit name it cannot trust) it asks the polecat
// to read the log and re-run the test, which is still a check one worker can
// do alone.
func tierSweepAcceptanceItem(tier, unit string, ts tierSweepTierResult) string {
	if cmd := tierSweepCheckCommand(tier, unit, ts); cmd != "" {
		return "- [ ] `" + cmd + "` passes on a worktree rebased onto main."
	}
	log := strings.TrimSpace(ts.Log)
	where := ""
	if log != "" {
		where = " (" + log + ")"
	}
	return fmt.Sprintf("- [ ] Read the tier log%s and re-run the failing test in %s with a narrow -run filter until it passes on a worktree rebased onto main.", where, tierSweepUnitSubject(tier, unit))
}

// tierSweepRedDetail is the one-line detail every sweep bead carries: the
// verdict, the tip it was taken at, the counts and the log.
func tierSweepRedDetail(tier, sha string, ts tierSweepTierResult) string {
	return fmt.Sprintf("%s tier RED on main at %s (%d passed, %d failed). Log: %s",
		tier, shortSHA(sha), ts.Passed, ts.Failed, ts.Log)
}

// tierSweepTemplate is the spec template the dispatcher lints against: the
// patrol block's template field, else the operator's default file, read by the
// same loader the dispatcher uses (gt-uzkic). The patrol block is read through
// ReadPatrolConfig, the reader the dispatcher's tick uses too. A block that
// cannot be read leaves the default path in place, and LoadTemplate falls back
// to the built-in sections, so filing a bead never fails on its shape. The
// tick itself stops on an unreadable block and dispatches nothing, so the
// fallback never files a bead a running dispatcher would hold.
func (d *Daemon) tierSweepTemplate() specdispatch.Template {
	configured := ""
	if d.config != nil {
		if pc, err := ReadPatrolConfig(d.config.TownRoot); err == nil && pc != nil && pc.Patrols != nil && pc.Patrols.SpecDispatch != nil {
			configured = pc.Patrols.SpecDispatch.Template
		}
	}
	return specdispatch.LoadTemplate(specdispatch.ResolveTemplatePath(configured))
}

// tierSweepSectionBody is the body the sweep writes under one section of the
// spec template. A section the sweep does not know points at the acceptance
// check, so the lint reads it as non-empty.
func tierSweepSectionBody(section, tier, subject, sha, item string) string {
	switch strings.ToLower(section) {
	case "goal":
		return fmt.Sprintf("Make the %s tier green on main again. %s is red at %s, so the sweep re-files this bead on every landing until the tier passes.", tier, subject, shortSHA(sha))
	case "constraints":
		return fmt.Sprintf("- Fix the failure in %s; the sweep closes this bead by itself once the tier is green.\n", subject) +
			"- Re-run with the narrow filter below, not the whole package: the polecat test guard refuses an unfiltered run of internal/cmd, internal/daemon and internal/polecat.\n" +
			"- Leave scripts/tier-sweep.sh, its TIER_SWEEP_SKIP list and the sweep's schedule alone."
	case "out of scope":
		return "The other tiers, the sweep's own scheduling, and any change to scripts/tier-sweep.sh."
	case "gate":
		return "make gate, plus the check in ## Acceptance."
	case "size":
		return tierSweepBeadSize
	case "acceptance":
		return item
	default:
		return fmt.Sprintf("The ## Acceptance check covers %s on the %s tier. This section has no other ask.", subject, tier)
	}
}

// tierSweepBeadShape returns the description the dispatcher's shape lint reads
// and the acceptance block for the bead's field. Both carry the same items:
// gt done reads the field and `gt spec lint` reads the description, so one
// without the other is a bead half a polecat can finish (gt-1lzy1).
//
// The sections come from tmpl, the template the dispatcher lints against, in
// its order, so a template that adds or renames a section still yields a bead
// that passes the lint (gt-uzkic). The Acceptance section always closes the
// description unless the template names it itself.
func tierSweepBeadShape(tier, unit, sha string, ts tierSweepTierResult, tmpl specdispatch.Template) (description, acceptance string) {
	item := tierSweepAcceptanceItem(tier, unit, ts)
	subject := tierSweepUnitSubject(tier, unit)
	names := append([]string(nil), tmpl.Sections...)
	if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, "acceptance") }) {
		names = append(names, "Acceptance")
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		body := tierSweepSectionBody(name, tier, subject, sha, item)
		parts = append(parts, "## "+name+"\n\n"+body)
	}
	return tierSweepBeadIntro + tierSweepRedDetail(tier, sha, ts) + "\n\n" + strings.Join(parts, "\n\n") + "\n", item
}
