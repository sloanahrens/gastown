package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/tiersweep"
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
	// (cmd/tier-sweep runs `go test -tags integration` for the tagged
	// integration list and the race tier).
	tierSweepGoBuildTag = "integration"

	// tierSweepLogReadLimit caps what the shape reads from a tier log. The
	// stage output is a marker per unit and five fail lines per failing unit,
	// so a file past this size is not the log the sweep wrote, and reading it
	// whole would cost the daemon's memory to learn nothing.
	tierSweepLogReadLimit = 4 << 20

	// tierSweepSlowcheckPrefix is the marker every slowcheck finding line
	// opens with: slowcheck's own name for itself, which the finding line the
	// host log showed carries (hq-in8pg), and the marker gt-j6vlz names when it
	// asks for "the slowcheck: finding lines". Such a run writes no FAIL line,
	// so the stage log the sweep captures names no test for it (gt-j6vlz).
	//
	// Only the marker is matched: slowcheck's source is not on this host, so a
	// rule for the rest of the line could not be checked against its writer, and
	// a line in another spelling would be lost. The whole line is quoted, and a
	// test name is taken only from the token after the marker when it is one.
	tierSweepSlowcheckPrefix = "slowcheck: "

	// tierSweepFindingLines caps how many findings a bead quotes: the polecat
	// needs the shape of the overrun, not every line of it.
	tierSweepFindingLines = 3

	// tierSweepFindingLineLen caps each quoted finding. slowcheck's line ends
	// with the reason it judged the test slow, which can run long.
	tierSweepFindingLineLen = 160
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

// tierSweepUnitLogPath is the log the script wrote one unit's own output to:
// "<marker tier>-<package with ./ dropped and / turned into _>.log" under the
// directory its summary line named. Empty when the sweep recorded no directory,
// or when the unit is not a name the script could have logged.
func tierSweepUnitLogPath(logDir, mtier, unit string) string {
	if strings.TrimSpace(logDir) == "" || !tierSweepUnitIsName(unit) {
		return ""
	}
	// A Go tier's package log is named by tiersweep.LogName; the shell
	// tier's script log is "shell-" plus its path, read as the same _ form.
	if mtier != tiersweep.TierShell {
		return filepath.Join(logDir, tiersweep.LogName(mtier, unit))
	}
	base := strings.ReplaceAll(strings.TrimPrefix(unit, "./"), "/", "_")
	return filepath.Join(logDir, mtier+"-"+base+".log")
}

// tierSweepFindingTestRE is the token a finding line can be read for: a
// top-level test's name, as go test runs one. "TestMain" is no test, so a -run
// filter naming it matches nothing and passes without running anything
// (gt-j6vlz).
var tierSweepFindingTestRE = regexp.MustCompile(`^Test[A-Z_0-9][A-Za-z0-9_]*$`)

// tierSweepFindingLine reports whether line is a finding slowcheck wrote, and
// returns it trimmed. Only the marker is read: the rest of the line is kept
// exactly as slowcheck wrote it, because it is quoted into the bead and a
// reader must be able to see the finding whether or not its spelling matches
// any this daemon knows (gt-j6vlz).
func tierSweepFindingLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, tierSweepSlowcheckPrefix) {
		return "", false
	}
	return line, true
}

// tierSweepFindingTest reads the test a finding line names, from the token the
// marker opens with. A line whose token is not a test's name names no test:
// the finding is still quoted, but no -run filter is invented from it, since a
// filter that matches nothing passes without running a test (gt-j6vlz). A
// subtest's entry repeats the parent's name with a "/" suffix; the name stops
// there, the way a FAIL line's name is cut.
func tierSweepFindingTest(line string) (string, bool) {
	line, ok := tierSweepFindingLine(line)
	if !ok {
		return "", false
	}
	fields := strings.Fields(strings.TrimPrefix(line, tierSweepSlowcheckPrefix))
	if len(fields) == 0 {
		return "", false
	}
	name := fields[0]
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	if !tierSweepFindingTestRE.MatchString(name) || name == "TestMain" {
		return "", false
	}
	return name, true
}

// tierSweepSlowcheckFindings returns the finding lines a unit log holds, in log
// order and deduplicated. The whole line is kept, because the bead quotes it;
// the test name a -run filter uses is the one tierSweepFindingTest names.
//
// Every line of the log belongs to the unit the log is named for: the sweep
// writes one unit's own output to its own file (tiersweep.LogName: the tier,
// the package with _ for /, then a hash). Nothing in a line's spelling of a
// package is parsed or compared, so a finding cannot be lost to a spelling this
// daemon does not know, and cannot be handed to another unit either (gt-j6vlz).
func tierSweepSlowcheckFindings(log string) []string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		finding, ok := tierSweepFindingLine(line)
		if !ok {
			continue
		}
		lines = append(lines, finding)
	}
	return tierSweepUnique(lines)
}

// tierSweepUnitRed is what a red run of one unit left in the sweep's logs.
type tierSweepUnitRed struct {
	// Names are the top-level tests the stage log's FAIL lines name.
	Names []string
	// Findings are slowcheck's finding lines, read from the unit's own log.
	Findings []string
	// BuildTag is the build tag the unit's runs used, empty for the untagged
	// docker list.
	BuildTag string
}

// tierSweepReadUnitRed reads what a red run of one unit left in the sweep's
// logs: the top-level tests the stage log's FAIL lines name, the slowcheck
// findings that unit's own log holds, and the build tag the runs used. A
// finding is evidence of its own: a cap trip writes no FAIL line, so the stage
// log names nothing for it and only the unit log does (gt-j6vlz). Both logs are
// read here, once, and the result is passed to everything that renders the bead.
//
// A log that cannot be read contributes nothing, so filing a shaped bead never
// depends on a log surviving. A unit the script ran both tagged and untagged
// names nothing, because no single command re-runs both.
func tierSweepReadUnitRed(ts tierSweepTierResult, tier, unit string) tierSweepUnitRed {
	var red tierSweepUnitRed
	stage := ""
	if strings.TrimSpace(ts.Log) != "" {
		if log, err := tierSweepReadLog(ts.Log); err == nil {
			stage = log
		}
	}
	units := tierSweepLogUnits(stage)
	tagged := false
	for _, mtier := range tierSweepLogMarkerTiers(tier) {
		names := append([]string(nil), units[mtier+"\x00"+unit]...)
		var findings []string
		if path := tierSweepUnitLogPath(ts.LogDir, mtier, unit); path != "" {
			if log, err := tierSweepReadLog(path); err == nil {
				findings = tierSweepSlowcheckFindings(log)
			}
		}
		if len(names) == 0 && len(findings) == 0 {
			continue
		}
		mtag := tierSweepMarkerBuildTag(mtier)
		if tagged && mtag != red.BuildTag {
			return tierSweepUnitRed{}
		}
		red.BuildTag, tagged = mtag, true
		red.Names = append(red.Names, names...)
		red.Findings = append(red.Findings, findings...)
	}
	red.Names = tierSweepUnique(red.Names)
	red.Findings = tierSweepUnique(red.Findings)
	return red
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
// for the shell tier, the command that ran for the rig (ranCmd: the Go runner,
// or the rig's own script), and `go test` filtered to the failing tests for a
// Go tier. A filtered run is what the polecat test guard allows on a heavy
// package, and a whole package is what it refuses. The shell command takes no
// filter, so it runs every shell test. The Go command keeps the sweep's own
// wrapper: GT_TEST_DOCKER=1, or the container tests skip and a green tick
// proves nothing, and gt slot run around it, or the container-suite guard
// refuses it. Empty when no command can be named from what the sweep knows, so
// the caller falls back to the log.
func tierSweepCheckCommand(tier, unit, ranCmd string, red tierSweepUnitRed) string {
	if !tierSweepUnitIsName(unit) {
		return ""
	}
	if tier == "shell" {
		if ranCmd == "" {
			return ""
		}
		return "GT_TEST_DOCKER=0 " + ranCmd
	}
	// A finding names its test in its own line, so the filter it earns is the
	// one a FAIL line earns. A test with both is named once.
	names := append([]string(nil), red.Names...)
	for _, line := range red.Findings {
		if name, ok := tierSweepFindingTest(line); ok {
			names = append(names, name)
		}
	}
	names = tierSweepUnique(names)
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
	if red.BuildTag != "" {
		b.WriteString("-tags " + red.BuildTag + " ")
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

// tierSweepRecordCommand is the command a shell record says ran. A record
// written before Command existed ran the Go shell runner, since every tree ran
// it then, so its check still names the runner. An empty Command on a red unit
// with a real name is that legacy case: a stage that ran nothing files its
// stand-in reason as the unit, and tierSweepCheckCommand returns empty for it.
func tierSweepRecordCommand(tier string, ts tierSweepTierResult) string {
	if tier == "shell" && ts.Command == "" {
		return tierSweepShellCommand
	}
	return ts.Command
}

// tierSweepAcceptanceItem is the one criterion a polecat ticks: the narrow
// re-run of the failing unit. When the sweep cannot name a command (a missing
// log, no failing test line, a unit name it cannot trust) it asks the polecat
// to read the log and re-run the test, which is still a check one worker can
// do alone.
func tierSweepAcceptanceItem(tier, unit string, ts tierSweepTierResult, red tierSweepUnitRed) string {
	if cmd := tierSweepCheckCommand(tier, unit, tierSweepRecordCommand(tier, ts), red); cmd != "" {
		item := "- [ ] `" + cmd + "` passes on a worktree rebased onto main."
		if tier != "shell" {
			// A -run filter taken from a log line can match no test, and go test
			// then passes having run nothing; the item says what that output means
			// so a worker does not tick a box nothing was run for.
			item += " Output that says `no tests to run` ran nothing: the name matched no test, so say so on this bead instead of ticking this box."
		}
		return item
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

// tierSweepClip quotes line for an agent-read bead: at most n bytes, cut on a
// rune boundary and marked with "..." so a reader can tell a clipped finding
// from a complete one. A byte that is not valid UTF-8 becomes the replacement
// character and a control character (an escape sequence slowcheck echoed from
// a test's output included) is dropped, so the quoting keeps the finding's
// printable text instead of losing the line or pasting terminal control into a
// bead (gt-j6vlz). A tab or other whitespace control reads as a space, so it
// separates words as it did; any other control character is dropped. A run of
// spaces is collapsed to one, and a cut never ends on a space before the "..."
// marker, so the quoted line reads as slowcheck's text with nothing of the
// quoting in it.
func tierSweepClip(line string, n int) string {
	var b strings.Builder
	b.Grow(n)
	cut := false
	var last rune = -1
	for _, r := range line {
		// Whitespace controls separate words, so they read as a space; any
		// other control character is dropped.
		switch r {
		case '\t', '\n', '\r', '\v', '\f':
			r = ' '
		default:
			if unicode.IsControl(r) {
				continue
			}
		}
		if r == ' ' && last == ' ' {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > n {
			cut = true
			break
		}
		b.WriteRune(r)
		last = r
	}
	quoted := strings.TrimRight(b.String(), " ")
	if !cut {
		return quoted
	}
	return quoted + "..."
}

// tierSweepFindingsBlock renders slowcheck's findings for the bead's own unit
// inline, so a polecat that cannot open the host's unit log can tell a cap trip
// from a real failure and still name the test it must re-run. Each line is
// quoted, not trusted: it is text slowcheck wrote about someone else's test
// run, so it is sanitized and bounded before it reaches a bead an agent reads.
// The whole line is quoted whatever its spelling, so a finding is never lost to
// a shape this daemon does not know. Empty when the unit has no readable
// finding, which leaves the bead as it was (gt-j6vlz).
func tierSweepFindingsBlock(red tierSweepUnitRed) string {
	findings := red.Findings
	if len(findings) == 0 {
		return ""
	}
	if len(findings) > tierSweepFindingLines {
		findings = findings[:tierSweepFindingLines]
	}
	var b strings.Builder
	b.WriteString("slowcheck's findings for this unit, quoted from its log (slowcheck's own lines, not FAIL lines; read each for what it reports):\n")
	for _, line := range findings {
		b.WriteString("  " + tierSweepClip(line, tierSweepFindingLineLen) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
			"- Leave cmd/tier-sweep, its TIER_SWEEP_SKIP list and the sweep's schedule alone."
	case "out of scope":
		return "The other tiers, the sweep's own scheduling, and any change to cmd/tier-sweep."
	case "gate":
		return "make gate, plus the check in ## Acceptance."
	case "size":
		return tierSweepBeadSize
	case "acceptance":
		return item
	default:
		return fmt.Sprintf("The ## Acceptance check covers %s. This section has no other ask.", subject)
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
	red := tierSweepReadUnitRed(ts, tier, unit)
	item := tierSweepAcceptanceItem(tier, unit, ts, red)
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
	detail := tierSweepBeadIntro + tierSweepRedDetail(tier, sha, ts)
	// The findings follow the one-line detail, so a polecat that cannot read
	// the host's unit log still learns what tripped the cap.
	if block := tierSweepFindingsBlock(red); block != "" {
		detail += "\n\n" + block
	}
	return detail + "\n\n" + strings.Join(parts, "\n\n") + "\n", item
}

// tierSweepReshapeOpen rewrites the open sweep bead for a unit that a red run
// found still red, when the dispatcher's lint refuses its text for a missing
// section or acceptance. A bead filed before the shape fix (gt-1lzy1) is
// prose-only, so the dispatcher holds it forever (gt-hang2). A bead a polecat
// has claimed is left to its owner, and a bead already in shape keeps its text.
func (d *Daemon) tierSweepReshapeOpen(bd tierSweepBeadStore, rig, tier, name, sha string, ts tierSweepTierResult, open tierSweepUnitBead) {
	if open.issue == nil {
		return
	}
	is := open.issue
	spec := specdispatch.Spec{
		Title:       is.Title,
		Type:        is.Type,
		Labels:      is.Labels,
		Description: is.Description,
		Acceptance:  is.AcceptanceCriteria,
	}
	tmpl := d.tierSweepTemplate()
	if !tierSweepUnshaped(spec, tmpl) {
		return
	}
	description, acceptance := tierSweepBeadShape(tier, name, sha, ts, tmpl)
	if err := bd.Update(open.id, beads.UpdateOptions{Description: &description, Acceptance: &acceptance}); err != nil {
		d.logger.Printf("tier_sweep: %s: reshaping %s (%s %s): %v", rig, open.id, tier, name, err)
	}
}

// tierSweepUnshaped reports whether the dispatcher's lint would refuse spec for
// a section it needs or its acceptance. A refusal on anything else (a Size
// that does not say one worker, a bead that is not a work bead, or the target
// branch's files, which the sweep has no repo to read) is left for the
// dispatcher to report, since rewriting the sweep's text does not fix it.
func tierSweepUnshaped(spec specdispatch.Spec, tmpl specdispatch.Template) bool {
	if specdispatch.NotWorkBead(spec) != "" {
		return false
	}
	for _, r := range specdispatch.LintShape(spec, tmpl).Refusals {
		if r.Field == "acceptance" || strings.HasPrefix(r.Field, "## ") {
			return true
		}
	}
	return false
}
