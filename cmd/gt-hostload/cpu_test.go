package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// cpuRead answers collectCPU's top read from top and its ps read from ps, so a
// test can fail either one and see what the other still recorded.
func cpuRead(top, ps string, topErr, psErr error) func(time.Duration, string, ...string) (string, error) {
	return func(_ time.Duration, name string, _ ...string) (string, error) {
		if name == "ps" {
			return ps, psErr
		}
		return top, topErr
	}
}

// TestHostCPUCollect reads the idle share, both memory values and the scanner's
// CPU seconds from good top and ps reads, and names the failing step while
// keeping what the other read gave: a failed ps keeps idle and memory, a failed
// top still records the scanner, and no scanner leaves its key absent.
func TestHostCPUCollect(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		why                  string
		top, ps              string
		topErr, psErr        error
		step                 string // "" when neither read fails
		wantTop, wantScanner bool
	}{
		{"good reads", topReadSample, procsSample, nil, nil, "", true, true},
		{"no scanner", topReadSample, "  42 0:01.50 /bin/sh -c sleep 30\n", nil, nil, "", true, false},
		{"failed ps read", topReadSample, "", nil, boom, "ps", true, false},
		{"failed top read", "", procsSample, boom, nil, "top", false, true},
		{"memory beyond int64 bytes", strings.Replace(topReadSample, "126G", "12000000000G", 1), procsSample, nil, nil, "top", false, true},
	} {
		rec, errs := collectCPU(cpuRead(c.top, c.ps, c.topErr, c.psErr))
		if c.step == "" {
			if len(errs) != 0 {
				t.Fatalf("%s: collectCPU errors = %v, want none", c.why, errs)
			}
		} else if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), c.step+": ") {
			t.Fatalf("%s: collectCPU errors = %v, want one naming %s", c.why, errs, c.step)
		}
		gotTop := rec.IdlePct != nil && rec.UsedBytes != nil && rec.CompressorBytes != nil
		if gotTop != c.wantTop {
			t.Errorf("%s: idle and memory recorded = %v: want %v", c.why, gotTop, c.wantTop)
		} else if gotTop && (*rec.IdlePct != 6.68 || *rec.UsedBytes != 126<<30 || *rec.CompressorBytes != 40<<30) {
			t.Errorf("%s: idle and memory = %v, %d, %d: want the values top gave", c.why, *rec.IdlePct, *rec.UsedBytes, *rec.CompressorBytes)
		}
		sec, ok := rec.Seconds["scanner"]
		if ok != c.wantScanner || (ok && sec != 125.25) {
			t.Errorf("%s: scanner seconds = %v, %v: want recorded = %v", c.why, sec, ok, c.wantScanner)
		}
	}
}
