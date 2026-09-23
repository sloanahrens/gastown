package guardlint

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadBaseline reads a newline-separated list of Finding.Key() values: the
// instances this check already knew about when it was introduced, and has
// not yet migrated. Blank lines and lines starting with "#" are ignored.
func LoadBaseline(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening baseline %s: %w", path, err)
	}
	defer f.Close()

	keys := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys[line] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading baseline %s: %w", path, err)
	}
	return keys, nil
}
