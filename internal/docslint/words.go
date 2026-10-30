package docslint

import (
	"fmt"
	"strings"
)

// WordCount is the number of whitespace-separated fields in text, counted by
// strings.Fields so the answer never depends on an installed locale (gt-shrnc).
func WordCount(text string) int {
	return len(strings.Fields(text))
}

// Words is the word total over the whole agent-facing tier, exempt formulas
// included; on an error naming a file it could not read, the total it also
// returns is meaningless and must not be reported.
func Words(fsys FS) (int, error) {
	files, err := AgentFacing(fsys)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range files {
		// A file that cannot be read is not a zero: a total that quietly skipped
		// it would read as a result for a tier the run never counted.
		data, err := fsys.ReadFile(p)
		if err != nil {
			return 0, fmt.Errorf("docs-lint: reading %s: %w", p, err)
		}
		total += WordCount(string(data))
	}
	return total, nil
}
