package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// vmProcsSample adds colima-gt-sbx's VM at 0:30.50 to procsSample (gt-6touna).
const vmProcsSample = procsSample + "  501 0:30.50 /System/Library/Frameworks/Virtualization.framework/Versions/A/Support/com.apple.Virtualization.VirtualMachine\n"

// cpuRead answers top and ps from its arguments, lsof -p pid from paths, and
// counts each lsof run so a test can see that a failed ps runs none.
func cpuRead(top, ps string, topErr, psErr error, paths map[string]string, lsofCalls *int) func(time.Duration, string, ...string) (string, error) {
	return func(_ time.Duration, name string, args ...string) (string, error) {
		switch name {
		case "ps":
			return ps, psErr
		case "lsof":
			*lsofCalls++
			if path, ok := paths[args[1]]; ok {
				return path, nil
			}
			return "", errors.New("exit status 1")
		}
		return top, topErr
	}
}

// TestHostCPUCollect reads the idle share, memory and the scanner's and each
// VM's CPU seconds from good top, ps and lsof reads, keeping what other reads
// gave: a failed ps runs no lsof, a failed top still records the scanner and
// VM, a failed lsof keeps both, and no scanner leaves its key absent.
func TestHostCPUCollect(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	vmPaths := map[string]string{"501": "/Users/me/.colima/_lima/colima-gt-sbx/disk"}
	for _, c := range []struct {
		why                  string
		top, ps              string
		topErr, psErr        error
		paths                map[string]string
		step                 string // "" when no read fails
		wantTop, wantScanner bool
		wantVM               float64 // colima-gt-sbx's seconds, 0 when absent
		wantLsof             int
	}{
		{"good reads", topReadSample, vmProcsSample, nil, nil, vmPaths, "", true, true, 30.5, 1},
		{"no scanner", topReadSample, "  42 0:01.50 /bin/sh -c sleep 30\n", nil, nil, nil, "", true, false, 0, 0},
		{"failed ps read", topReadSample, "", nil, boom, vmPaths, "ps", true, false, 0, 0},
		{"failed top read", "", vmProcsSample, boom, nil, vmPaths, "top", false, true, 30.5, 1},
		{"memory beyond int64 bytes", strings.Replace(topReadSample, "126G", "12000000000G", 1), procsSample, nil, nil, nil, "top", false, true, 0, 0},
		{"failed lsof", topReadSample, vmProcsSample, nil, nil, nil, "lsof -p 501", true, true, 0, 1},
	} {
		var lsofCalls int
		rec, errs := collectCPU(cpuRead(c.top, c.ps, c.topErr, c.psErr, c.paths, &lsofCalls))
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
		vm, vmOK := rec.Seconds["sbx"]
		if vm != c.wantVM || vmOK != (c.wantVM != 0) || lsofCalls != c.wantLsof {
			t.Errorf("%s: sbx = %v (recorded %v), lsof runs = %d: want %v, %d", c.why, vm, vmOK, lsofCalls, c.wantVM, c.wantLsof)
		}
	}
}
