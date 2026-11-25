package done

import (
	"fmt"
	"regexp"
)

// runPatternSelects reports whether a go test -run pattern selects the
// top-level test name. Only the first element (the pattern split at slashes
// outside brackets and parentheses) is matched, unanchored, against the name;
// later elements select subtests. An empty first element selects every name,
// and one that does not compile is an error naming the pattern.
func runPatternSelects(pattern, name string) (bool, error) {
	first := firstRunPatternElement(pattern)
	if first == "" {
		return true, nil
	}
	re, err := regexp.Compile(first)
	if err != nil {
		return false, fmt.Errorf("-run pattern %q: first element %q does not compile: %w", pattern, first, err)
	}
	return re.MatchString(name), nil
}

// firstRunPatternElement returns the text before the first slash outside
// brackets and parentheses; a backslash escapes the next byte, as in go's splitter.
func firstRunPatternElement(pattern string) string {
	brackets, parens := 0, 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			brackets++
		case ']':
			if brackets > 0 {
				brackets--
			}
		case '(':
			if brackets == 0 {
				parens++
			}
		case ')':
			if brackets == 0 && parens > 0 {
				parens--
			}
		case '/':
			if brackets == 0 && parens == 0 {
				return pattern[:i]
			}
		}
	}
	return pattern
}
