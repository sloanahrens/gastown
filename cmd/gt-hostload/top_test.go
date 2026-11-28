package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// topReadSample is the tail of a real two-sample `top -l 2` run; the second
// sample is the one that counts (gt-efoycy).
const topReadSample = `CPU usage: 50.00% user, 10.00% sys, 40.00% idle
PhysMem: 8G used (1G wired, 2G compressor), 8G unused.

CPU usage: 81.91% user, 11.40% sys, 6.68% idle
PhysMem: 126G used (13G wired, 40G compressor), 533M unused.
`

// topRead is a fake read that answers with text or err and records how
// readTop called it.
type topRead struct {
	timeout time.Duration
	name    string
	args    []string
	text    string
	err     error
}

func (r *topRead) read(timeout time.Duration, name string, args ...string) (string, error) {
	r.timeout, r.name, r.args = timeout, name, args
	return r.text, r.err
}

// TestHostCPUReadTop reads the Mac's idle share and both memory values through
// the top command it records.
func TestHostCPUReadTop(t *testing.T) {
	t.Parallel()
	r := &topRead{text: topReadSample}
	idle, used, compressor, err := readTop(r.read)
	if err != nil {
		t.Fatalf("readTop: %v", err)
	}
	if idle != 6.68 || used != 126<<30 || compressor != 40<<30 {
		t.Errorf("readTop = %v, %d, %d: want 6.68, %d, %d",
			idle, used, compressor, uint64(126)<<30, uint64(40)<<30)
	}
	if r.name != "top" || strings.Join(r.args, " ") != "-l 2 -n 0 -s 1" || r.timeout != 30*time.Second {
		t.Errorf("read(%q, %v, %v): want top -l 2 -n 0 -s 1 with 30s", r.name, r.args, r.timeout)
	}
}

// TestHostCPUReadTopRefuses names top for a failed read and for a read whose
// text ParseTop cannot parse.
func TestHostCPUReadTopRefuses(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		why  string
		read error
		text string
	}{
		{"failed read", boom, ""},
		{"unparsable", nil, "CPU usage: many% idle\nPhysMem: 1G used (0B wired, 0B compressor)\n"},
	} {
		r := &topRead{text: c.text, err: c.read}
		want := `one starting "top: "`
		if c.read != nil {
			want += " and wrapping the read's error"
		}
		if _, _, _, err := readTop(r.read); err == nil || !strings.HasPrefix(err.Error(), "top: ") || (c.read != nil && !errors.Is(err, c.read)) {
			t.Errorf("%s: readTop error = %v, want %s", c.why, err, want)
		}
	}
}
