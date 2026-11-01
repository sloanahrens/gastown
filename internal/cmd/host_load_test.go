package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// TestHostLoadHeaderStatesLimit checks the printed header says which processes
// the table cannot see, before the table it limits.
func TestHostLoadHeaderStatesLimit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeHostLoadReport(&buf, hostload.Sample{
		At:          time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC),
		Load:        hostprobe.Field[hostload.Load]{Value: hostload.Load{One: 4.08, Five: 2.06, Fifteen: 1.02}},
		BDLatencyMS: hostprobe.Field[float64]{Value: 130},
		TickMS:      hostprobe.Field[float64]{Value: 5400},
		Seats:       hostprobe.Field[int]{Value: 6},
		Owners: hostprobe.Field[[]hostload.Row]{Value: []hostload.Row{
			{Owner: "crew/dunlin", CPU: 30, Procs: 2},
		}},
	})

	const limit = "A process that started and exited between samples is not counted."
	if !strings.Contains(buf.String(), limit) {
		t.Errorf("the header does not state the short-lived-process limit:\n%s", buf.String())
	}
	if table := strings.Index(buf.String(), "OWNER"); table >= 0 && strings.Index(buf.String(), limit) > table {
		t.Errorf("the limit is printed after the table it limits:\n%s", buf.String())
	}
	for _, want := range []string{"crew/dunlin", "load 4.08 2.06 1.02", "bd 130 ms", "tick 5400 ms", "seats 6"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the report does not print %q:\n%s", want, buf.String())
		}
	}
}

// TestHostLoadRefusesUnusedFlags drives the flag refusals: a flag that would
// change nothing, and a sampler that would write nowhere, are refused before
// the host is read.
func TestHostLoadRefusesUnusedFlags(t *testing.T) {
	t.Parallel()
	d := hostLoadDeps{
		findTownRoot: func() (string, error) {
			t.Fatal("the town was read before the flags were checked")
			return "", nil
		},
		newProber: func(string) *hostload.Prober {
			t.Fatal("a prober was built before the flags were checked")
			return nil
		},
		openOut: func(string) (io.WriteCloser, error) {
			t.Fatal("a file was opened before the flags were checked")
			return nil, nil
		},
		out: io.Discard,
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "--out without --sample", args: []string{"--out", "load.jsonl"}, want: "--out holds samples"},
		{name: "--every without --sample", args: []string{"--every", "2s"}, want: "--every sets the sample interval"},
		{name: "--sample without --out", args: []string{"--sample"}, want: "--sample needs --out"},
		{name: "--sample with a zero interval", args: []string{"--sample", "--out", "load.jsonl", "--every", "0s"}, want: "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newHostLoadCmd(d)
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("gt host load %v ran", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
