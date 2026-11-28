package main

import (
	"errors"
	"strings"
	"testing"
)

// TestHostCPUCollect reads the idle share and both memory values from a good
// top read.
func TestHostCPUCollect(t *testing.T) {
	t.Parallel()
	rec, errs := collectCPU((&topRead{text: topReadSample}).read)
	if len(errs) != 0 {
		t.Fatalf("collectCPU errors = %v, want none", errs)
	}
	if rec.IdlePct == nil || rec.UsedBytes == nil || rec.CompressorBytes == nil {
		t.Fatalf("collectCPU record = %+v, want idle and both memory values", rec)
	}
	if *rec.IdlePct != 6.68 || *rec.UsedBytes != 126<<30 || *rec.CompressorBytes != 40<<30 {
		t.Errorf("collectCPU = %v, %d, %d: want 6.68, %d, %d", *rec.IdlePct, *rec.UsedBytes, *rec.CompressorBytes,
			int64(126)<<30, int64(40)<<30)
	}
}

// TestHostCPUCollectRefuses leaves every top value nil and returns one error
// naming top for a failed read and for a memory amount beyond int64 bytes.
func TestHostCPUCollectRefuses(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		why  string
		text string
		read error
	}{
		{"failed read", "", boom},
		{"memory beyond int64 bytes", strings.Replace(topReadSample, "126G", "12000000000G", 1), nil},
	} {
		rec, errs := collectCPU((&topRead{text: c.text, err: c.read}).read)
		if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "top: ") || (c.read != nil && !errors.Is(errs[0], c.read)) {
			t.Errorf("%s: collectCPU errors = %v, want one naming top", c.why, errs)
		}
		if rec.IdlePct != nil || rec.UsedBytes != nil || rec.CompressorBytes != nil {
			t.Errorf("%s: collectCPU record = %+v, want idle and memory nil", c.why, rec)
		}
	}
}
