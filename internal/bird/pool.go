package bird

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// embeddedNames is the pool that ships in the binary; Pool parses it.
//
//go:embed names.txt
var embeddedNames string

// nameShape is what a name must look like to reach a session name, a git
// identity and a bead label unchanged.
var nameShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// loadPool parses the embedded pool once; the bytes are fixed at build time,
// so a parse failure is a packaging defect no caller can recover from.
var loadPool = sync.OnceValue(func() []string {
	names, err := ParsePool(strings.NewReader(embeddedNames))
	if err != nil {
		panic("bird: the embedded name pool does not parse: " + err.Error())
	}
	return names
})

// Pool returns the bird names embedded in the binary.
func Pool() []string {
	return slices.Clone(loadPool())
}

// ValidName reports whether name may be used as a bird name.
func ValidName(name string) bool {
	return nameShape.MatchString(name)
}

// ParsePool reads a name pool from r, skipping blank lines and lines whose
// first non-space character is '#'.
func ParsePool(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	var names []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the name pool: %w", err)
	}
	return names, nil
}
