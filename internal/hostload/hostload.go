// Package hostload writes the host's load average to a file a day, one line a
// call, so a stall can be laid beside the load of the minute it happened
// (gt-qri7ug). The number counts threads, not saturation: the Mac reads 30
// with half its CPU idle (gt-auiji).
package hostload

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Parse reads macOS's `sysctl -n vm.loadavg` ("{ 21.53 22.70 21.14 }") or a
// Linux /proc/loadavg line ("21.53 22.70 21.14 3/900 12345"), and refuses any
// other text with an error naming its first 40 bytes.
func Parse(text string) ([3]float64, error) {
	var load [3]float64
	t := strings.TrimSpace(text)
	f := strings.Fields(t)
	switch {
	case strings.HasPrefix(t, "{"):
		if f = strings.Fields(strings.Trim(t, "{}")); !strings.HasSuffix(t, "}") || len(f) != 3 {
			return load, refuse(text)
		}
	case len(f) != 5 || !strings.Contains(f[3], "/"):
		// /proc/loadavg puts the runnable n/m field and the last pid after
		// the three averages.
		return load, refuse(text)
	}
	for i := range load {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return load, refuse(text)
		}
		load[i] = v
	}
	return load, nil
}

// Line renders one record: the reading's own local time, the three averages
// to two decimals, and the core count.
func Line(now time.Time, load [3]float64, ncpu int) string {
	return fmt.Sprintf("%s load1=%.2f load5=%.2f load15=%.2f ncpu=%d",
		now.Format(time.RFC3339), load[0], load[1], load[2], ncpu)
}

// Append appends line to dir's file for now's own local date, YYYY-MM-DD.log,
// creating dir (0755) and the file (0644) and never rewriting one.
func Append(dir string, now time.Time, line string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating host load directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, now.Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening host load file %s: %w", path, err)
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("appending to host load file %s: %w", path, err)
	}
	return nil
}

// refuse names text's first 40 bytes, which is enough to find a bad read in a
// log without printing all of it.
func refuse(text string) error {
	return fmt.Errorf("host load text %q is neither { l1 l5 l15 } nor a /proc/loadavg line",
		text[:min(len(text), 40)])
}
