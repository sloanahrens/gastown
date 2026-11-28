package hostload

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var topIdleRE = regexp.MustCompile(`(\S+)% idle`)
var topMemRE = regexp.MustCompile(`(\S+) (used|compressor)`)

// ParseTop reads the last CPU usage and PhysMem lines of `top -l 2` output,
// returning the idle percent and the used and compressor memory in bytes.
func ParseTop(text string) (idle float64, used, compressor uint64, err error) {
	var cpuLine, memLine string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "CPU usage:"):
			cpuLine = line
		case strings.HasPrefix(line, "PhysMem:"):
			memLine = line
		}
	}
	m := topIdleRE.FindStringSubmatch(cpuLine)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("top text has no CPU usage line with an idle percent")
	}
	idle, err = strconv.ParseFloat(m[1], 64)
	if err != nil || math.IsNaN(idle) || math.IsInf(idle, 0) {
		return 0, 0, 0, fmt.Errorf("CPU usage line's idle value is not a finite number")
	}
	if memLine == "" {
		return 0, 0, 0, fmt.Errorf("top text has no PhysMem line")
	}
	var haveUsed, haveCompressor bool
	for _, m := range topMemRE.FindAllStringSubmatch(memLine, -1) {
		n, err := topBytes(m[1])
		if err != nil {
			return 0, 0, 0, err
		}
		if m[2] == "used" {
			used, haveUsed = n, true
		} else {
			compressor, haveCompressor = n, true
		}
	}
	if !haveUsed || !haveCompressor {
		amount := "used"
		if haveUsed {
			amount = "compressor"
		}
		return 0, 0, 0, fmt.Errorf("PhysMem line has no %s amount", amount)
	}
	return idle, used, compressor, nil
}

// topBytes reads one PhysMem amount such as "126G" as bytes. top's units are
// powers of 1024, and an amount whose bytes do not fit uint64 is refused.
func topBytes(amount string) (uint64, error) {
	mult := map[string]uint64{"B": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30}
	per, ok := mult[amount[len(amount)-1:]]
	if !ok {
		return 0, errors.New("PhysMem line's amount is not a count with a B, K, M or G unit")
	}
	n, err := strconv.ParseUint(amount[:len(amount)-1], 10, 64)
	if errors.Is(err, strconv.ErrRange) || (err == nil && n > math.MaxUint64/per) {
		return 0, errors.New("PhysMem line's amount does not fit in uint64 bytes")
	}
	if err != nil {
		return 0, errors.New("PhysMem line's amount is not a count with a B, K, M or G unit")
	}
	return n * per, nil
}
