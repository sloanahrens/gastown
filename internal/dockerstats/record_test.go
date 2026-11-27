package dockerstats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

func TestDoltStatsRecordAppendsOneRowAndReturnsIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC)
	row, err := Record(dir, "gt-dolt", now, readLine(`{"CPUPerc":"349.74%","MemUsage":"7.989GiB / 8GiB"}`))
	if err != nil {
		t.Fatalf("Record error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "resources", "2026-11-24.jsonl"))
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("appended %d lines, want 1: %q", n, data)
	}
	var fileRow resourcesampler.Row
	if err := json.Unmarshal(data, &fileRow); err != nil {
		t.Fatalf("decode appended line: %v", err)
	}
	if fileRow.Component != "dolt-server" || fileRow.CPUPct == nil || *fileRow.CPUPct != 349.74 ||
		fileRow.MemBytes != 8578123432 || fileRow.MemLimitBytes == nil || *fileRow.MemLimitBytes != 8589934592 {
		t.Errorf("appended row = %+v, want component dolt-server and the parsed readings", fileRow)
	}
	if !reflect.DeepEqual(row, fileRow) {
		t.Errorf("returned row = %+v, want the appended row %+v", row, fileRow)
	}
}

func TestDoltStatsRecordRefusesWithoutRow(t *testing.T) {
	t.Parallel()

	readErr := errors.New("docker stats: daemon not reachable")
	now := time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC)
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		read    func(container string) ([]byte, error)
		wantErr error // nil accepts any error
		noFile  bool
	}{
		{"read fails", nil, func(string) ([]byte, error) { return nil, readErr }, readErr, true},
		{"line is not JSON", nil, readLine(`CPUPerc=349.74%`), ErrNotJSON, true},
		// A regular file where the rows directory belongs fails the append in a
		// directory a unit test can still build.
		{"append fails", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "resources"), nil, 0o644); err != nil {
				t.Fatalf("block the resources directory: %v", err)
			}
		}, readLine(`{"CPUPerc":"1.0%","MemUsage":"1MiB / 2MiB"}`), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, dir)
			}
			row, err := Record(dir, "gt-dolt", now, tt.read)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("Record error = %v, want %v", err, tt.wantErr)
			}
			if row != (resourcesampler.Row{}) {
				t.Errorf("Record returned row %+v with the error, want no row", row)
			}
			if tt.noFile {
				if _, statErr := os.Stat(filepath.Join(dir, "resources", "2026-11-24.jsonl")); !os.IsNotExist(statErr) {
					t.Errorf("Record appended a row on failure: stat error = %v", statErr)
				}
			}
		})
	}
}
