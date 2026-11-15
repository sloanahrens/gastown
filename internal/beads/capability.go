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

// sampleOffersVerb reports whether the bd serving this store offers the sample
// verb, read from its --help, and reads no database. A bd built before the
// verb (be-1qqu) must be refused by name, not by its unknown-command exit.
func (b *Beads) sampleOffersVerb() (bool, error) {
	help, err := b.run("--help")
	if err != nil {
		return false, err
	}
	return HelpOffersVerb(help, "sample"), nil
}

// HelpOffersVerb reports whether a bd --help text lists verb as a command: a
// line whose first word is the verb. A verb named only in prose is not
// offered, and neither is a longer verb that starts with the same letters.
func HelpOffersVerb(help []byte, verb string) bool {
	for _, line := range strings.Split(string(help), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == verb {
			return true
		}
	}
	return false
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
