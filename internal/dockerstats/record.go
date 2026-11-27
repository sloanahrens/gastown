package dockerstats

import (
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// Record reads one docker stats line for container through read, appends the
// resource row to the day's file under dir and returns that row. A failing read
// or parse appends nothing, and a failing append returns its error; every error
// return comes with the zero Row.
func Record(dir string, container string, now time.Time, read func(container string) ([]byte, error)) (resourcesampler.Row, error) {
	row, err := Collect(read, container, now)
	if err != nil {
		return resourcesampler.Row{}, err
	}
	if err := resourcesampler.AppendRow(dir, row, now); err != nil {
		return resourcesampler.Row{}, err
	}
	return row, nil
}
