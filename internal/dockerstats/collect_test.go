package dockerstats

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// readLine is a fake docker stats read that answers with line and no error.
func readLine(line string) func(container string) ([]byte, error) {
	return func(string) ([]byte, error) {
		return []byte(line), nil
	}
}

func TestDoltStatsCollectReadsOnceWithContainer(t *testing.T) {
	t.Parallel()

	var calls []string
	read := func(container string) ([]byte, error) {
		calls = append(calls, container)
		return []byte(`{"CPUPerc":"0.00%","MemUsage":"512MiB / 1GiB"}`), nil
	}

	if _, err := Collect(read, "gt-dolt", time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Collect error = %v", err)
	}
	if len(calls) != 1 || calls[0] != "gt-dolt" {
		t.Fatalf("read calls = %q, want one call with gt-dolt", calls)
	}
}

func TestDoltStatsCollectRowFromLine(t *testing.T) {
	t.Parallel()

	row, err := Collect(readLine(`{"CPUPerc":"349.74%","MemUsage":"7.989GiB / 8GiB"}`), "gt-dolt",
		time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect error = %v", err)
	}

	if row.Component != "dolt-server" {
		t.Errorf("Component = %q, want dolt-server", row.Component)
	}
	if row.CPUPct == nil || *row.CPUPct != 349.74 {
		t.Errorf("CPUPct = %v, want 349.74", row.CPUPct)
	}
	if row.MemBytes != 8578123432 {
		t.Errorf("MemBytes = %d, want 8578123432", row.MemBytes)
	}
	if row.MemLimitBytes == nil || *row.MemLimitBytes != 8589934592 {
		t.Errorf("MemLimitBytes = %v, want 8589934592", row.MemLimitBytes)
	}
}

func TestDoltStatsCollectRefusesWithoutRow(t *testing.T) {
	t.Parallel()

	readErr := errors.New("docker stats: daemon not reachable")
	tests := []struct {
		name    string
		read    func(container string) ([]byte, error)
		wantErr error
	}{
		{
			name:    "read fails",
			read:    func(string) ([]byte, error) { return nil, readErr },
			wantErr: readErr,
		},
		{
			name:    "line is not JSON",
			read:    readLine(`CPUPerc=349.74%`),
			wantErr: ErrNotJSON,
		},
		{
			name:    "line misses MemUsage",
			read:    readLine(`{"CPUPerc":"1.0%"}`),
			wantErr: ErrMissingField,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row, err := Collect(tt.read, "gt-dolt", time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Collect error = %v, want %v", err, tt.wantErr)
			}
			if row != (resourcesampler.Row{}) {
				t.Errorf("Collect returned row %+v with the error, want no row", row)
			}
		})
	}
}
