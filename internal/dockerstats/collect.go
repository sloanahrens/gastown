package dockerstats

import (
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// Collect reads one docker stats line for container through read, parses it
// and returns the resource row for it, stamped at now. read is called once. A
// failing read or a line that does not parse returns its error and no row.
func Collect(read func(container string) ([]byte, error), container string, now time.Time) (resourcesampler.Row, error) {
	line, err := read(container)
	if err != nil {
		return resourcesampler.Row{}, err
	}
	s, err := Parse(string(line))
	if err != nil {
		return resourcesampler.Row{}, err
	}
	return Row(s, now), nil
}
