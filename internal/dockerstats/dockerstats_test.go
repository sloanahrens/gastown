package dockerstats

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func TestParse(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name    string
		line    string
		want    Stats
		wantErr error
	}

	// The real docker stats line from 2026-11-24, the reading no sampler
	// recorded before the 21:37 outage.
	tests := []testCase{
		{
			name: "outage line",
			line: `{"CPUPerc":"349.74%","MemUsage":"7.989GiB / 8GiB"}`,
			want: Stats{CPUPct: 349.74, MemBytes: 8578123432, MemLimitBytes: 8589934592},
		},
		{
			name: "MiB use with a GiB limit",
			line: `{"CPUPerc":"0.00%","MemUsage":"512MiB / 1GiB"}`,
			want: Stats{CPUPct: 0, MemBytes: 512 << 20, MemLimitBytes: 1 << 30},
		},
		{
			name: "kB use and limit",
			line: `{"CPUPerc":"1.5%","MemUsage":"1.5kB / 4kB"}`,
			want: Stats{CPUPct: 1.5, MemBytes: 1500, MemLimitBytes: 4000},
		},
		{name: "not JSON", line: `CPUPerc=349.74%`, wantErr: ErrNotJSON},
		{name: "JSON array", line: `[]`, wantErr: ErrNotJSON},
		{name: "missing CPUPerc", line: `{"MemUsage":"1MiB / 2MiB"}`, wantErr: ErrMissingField},
		{name: "missing MemUsage", line: `{"CPUPerc":"1.0%"}`, wantErr: ErrMissingField},
		{name: "unknown unit", line: `{"CPUPerc":"1.0%","MemUsage":"1XB / 2GiB"}`, wantErr: ErrUnknownUnit},
		{name: "percent without a number", line: `{"CPUPerc":"abc%","MemUsage":"1MiB / 2MiB"}`, wantErr: ErrBadPercent},
		{name: "percent without the sign", line: `{"CPUPerc":"1.0","MemUsage":"1MiB / 2MiB"}`, wantErr: ErrBadPercent},
		{name: "percent NaN", line: `{"CPUPerc":"NaN%","MemUsage":"1MiB / 2MiB"}`, wantErr: ErrBadPercent},
		{name: "percent infinite", line: `{"CPUPerc":"Inf%","MemUsage":"1MiB / 2MiB"}`, wantErr: ErrBadPercent},
		{name: "percent negative", line: `{"CPUPerc":"-1%","MemUsage":"1MiB / 2MiB"}`, wantErr: ErrBadPercent},
		{name: "size without a limit", line: `{"CPUPerc":"1.0%","MemUsage":"1GiB"}`, wantErr: ErrBadSize},
		{name: "size starts with a unit", line: `{"CPUPerc":"1.0%","MemUsage":"GiB / 2GiB"}`, wantErr: ErrBadSize},
		{name: "size without a unit", line: `{"CPUPerc":"1.0%","MemUsage":"12 / 2GiB"}`, wantErr: ErrBadSize},
		{name: "size with two decimal points", line: `{"CPUPerc":"1.0%","MemUsage":"1.2.3GiB / 2GiB"}`, wantErr: ErrBadSize},
		{name: "limit overflows int64", line: `{"CPUPerc":"1.0%","MemUsage":"1TiB / 999999999TiB"}`, wantErr: ErrBadSize},
	}

	// Every unit the parser reads gets a row, so a unit dropped from the table
	// is red.
	units := []struct {
		unit    string
		perUnit int64
	}{
		{"B", 1},
		{"kB", 1000},
		{"KiB", 1 << 10},
		{"MB", 1000000},
		{"MiB", 1 << 20},
		{"GB", 1000000000},
		{"GiB", 1 << 30},
		{"TB", 1000000000000},
		{"TiB", 1 << 40},
	}
	for _, u := range units {
		tests = append(tests, testCase{
			name: "unit " + u.unit,
			line: fmt.Sprintf(`{"CPUPerc":"1%%","MemUsage":"2%s / 4%s"}`, u.unit, u.unit),
			want: Stats{CPUPct: 1, MemBytes: 2 * u.perUnit, MemLimitBytes: 4 * u.perUnit},
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tt.line)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%s) error = %v, want %v", tt.line, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%s) error = %v", tt.line, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%s) = %+v, want %+v", tt.line, got, tt.want)
			}
		})
	}
}
