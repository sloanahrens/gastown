package docslint

import "fmt"

// RuleWordCeiling is the rule the word-ceiling check reports under.
const RuleWordCeiling = "word-ceiling"

// CheckWordCeiling reports each file of the ceiling tier with more than ceiling
// words. A file at the ceiling passes; one word over is a finding. A file that
// cannot be read is an error, never a pass: a count that skipped it would read
// as a clean tier.
func CheckWordCeiling(fsys FS, ceiling int) ([]Finding, error) {
	files, err := CeilingTier(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range files {
		data, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		if n := WordCount(string(data)); n > ceiling {
			out = append(out, Finding{
				File:    f,
				Line:    1,
				Rule:    RuleWordCeiling,
				Message: fmt.Sprintf("%d words, ceiling %d", n, ceiling),
			})
		}
	}
	return out, nil
}
