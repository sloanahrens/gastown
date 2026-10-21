package drift

import (
	"regexp"
	"sort"
	"strings"
)

const (
	// maxSections caps the doc sections one landing is asked about.
	maxSections = 5

	// maxSectionRunes caps one doc section's text in the state.
	maxSectionRunes = 1500

	// maxIdentifiers caps the identifiers taken from one diff.
	maxIdentifiers = 40
)

var (
	// headingRe matches an ATX heading: one to six hashes and a space.
	headingRe = regexp.MustCompile(`^#{1,6} `)

	// flagRe matches a command-line flag such as --recent.
	flagRe = regexp.MustCompile(`--[a-z][a-z0-9-]*[a-z0-9]`)

	// keyRe matches a snake_case or kebab-case name, the shape of a config
	// key or a command word such as jev_drift or max-usd.
	keyRe = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:[_-][a-z0-9]+)+\b`)

	// cmdRe matches a gt command and its subcommand, such as gt jev landing.
	cmdRe = regexp.MustCompile(`\bgt [a-z][a-z-]*(?: [a-z][a-z-]*)?`)
)

// Section is one heading's section of a doc file: the heading and its body up
// to the next heading.
type Section struct {
	// Doc is the file's path in the rig's repository.
	Doc string
	// Heading is the heading line without its hashes. A section above the
	// file's first heading has an empty Heading.
	Heading string
	// Text is the section's lines, cut to maxSectionRunes.
	Text string
	// at is the section's place among its doc's sections, from the top.
	at int
	// score counts the diff identifiers the section contains.
	score int
}

// splitSections cuts one doc file into sections. A heading inside a code fence
// is body text, not a heading, so a shell comment does not start a section.
func splitSections(doc, text string) []Section {
	var out []Section
	var lines []string
	heading := ""
	inFence := false
	flush := func() {
		body := strings.TrimSpace(strings.Join(lines, "\n"))
		if body != "" {
			out = append(out, Section{Doc: doc, Heading: heading, Text: truncate(body, maxSectionRunes), at: len(out)})
		}
		lines = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && headingRe.MatchString(line) {
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		lines = append(lines, line)
	}
	flush()
	return out
}

// identifiers returns the names a diff's changed lines use, in first-seen
// order and capped at maxIdentifiers: flags, snake_case and kebab-case names,
// and gt command phrases. Only lines the diff adds or removes count, so a
// context line never names a section.
func identifiers(patch string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(term string) {
		if len(out) >= maxIdentifiers || len(term) < 4 || seen[term] {
			return
		}
		seen[term] = true
		out = append(out, term)
	}
	for _, line := range strings.Split(patch, "\n") {
		if (!strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "-")) ||
			strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		for _, re := range []*regexp.Regexp{flagRe, keyRe, cmdRe} {
			for _, term := range re.FindAllString(line, -1) {
				add(term)
			}
		}
	}
	return out
}

// rank scores each section by the identifiers it contains, keeps the ones
// with a match and returns the best maxSections. Ties go to the section that
// sorts first by doc path, then by its place in the doc, so a rerun picks the
// same sections.
func rank(terms []string, secs []Section) []Section {
	var hits []Section
	for _, s := range secs {
		s.score = 0
		for _, term := range terms {
			if strings.Contains(s.Text, term) {
				s.score++
			}
		}
		if s.score > 0 {
			hits = append(hits, s)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].Doc != hits[j].Doc {
			return hits[i].Doc < hits[j].Doc
		}
		return hits[i].at < hits[j].at
	})
	if len(hits) > maxSections {
		hits = hits[:maxSections]
	}
	return hits
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
