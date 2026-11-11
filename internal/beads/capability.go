package beads

import "strings"

// UpdateOffersFlag reports whether the bd serving this store offers flag on
// bd update, read from its --help, and reads no database. A test that needs a
// flag newer than the bd on PATH asks this before it runs: the image's bd is
// built at the committed pin and moves only with an image pair, so the test
// skips with the reason instead of holding a sweep red until the pair it waits
// for exists (gastown gt-8mvpvz).
func (b *Beads) UpdateOffersFlag(flag string) (bool, error) {
	help, err := b.run("update", "--help")
	if err != nil {
		return false, err
	}
	return HelpOffersFlag(help, flag), nil
}

// HelpOffersFlag reports whether a bd --help text offers flag: the flag opens a
// line of the flag table (alone, with a value, or beside its short form). A flag
// named only in prose, or a longer flag that starts with the same letters, is
// not offered.
func HelpOffersFlag(help []byte, flag string) bool {
	for _, line := range strings.Split(string(help), "\n") {
		for _, f := range strings.Fields(line) {
			if f == flag || strings.HasPrefix(f, flag+"=") || strings.HasPrefix(f, flag+",") {
				return true
			}
			if strings.HasPrefix(f, "-") {
				continue
			}
			break
		}
	}
	return false
}
