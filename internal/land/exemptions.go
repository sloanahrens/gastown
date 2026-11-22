package land

import (
	"fmt"
	"strings"
)

// parseExemptions reads the exemption list's text, one `<pkg> <TestName> # <reason>`
// per line, so the registry check and the test that shrinks the list share one parser.
func parseExemptions(text string) (map[string]string, error) {
	exempt := map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, found := strings.Cut(line, "#")
		key, reason = strings.Join(strings.Fields(key), " "), strings.TrimSpace(reason)
		if !found || reason == "" {
			return nil, fmt.Errorf("exemption line %d %q has no reason: want <pkg> <TestName> # <reason>", i+1, line)
		}
		if _, dup := exempt[key]; dup {
			return nil, fmt.Errorf("exemption line %d repeats %s", i+1, key)
		}
		exempt[key] = reason
	}
	return exempt, nil
}
