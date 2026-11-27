package bdgateway

import (
	"encoding/json"
	"testing"
)

// TestStatsCountsUnreachable is acceptance box 1: a counted call that bd failed
// with its store_unavailable exit (25) raises unreachable by one, and a call
// that exits 1 or 0 leaves it.
func TestStatsCountsUnreachable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		exit int
		want uint64
	}{{25, 1}, {1, 0}, {0, 0}} {
		l, err := NewCallLog(testOptions(t))
		if err != nil {
			t.Fatalf("NewCallLog: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
		l.Log(CallRecord{Argv: []string{"show", "gt-itjuwu"}, ExitCode: tt.exit})
		if got := l.Stats().Unreachable; got != tt.want {
			t.Errorf("Unreachable = %d after one call exiting %d, want %d", got, tt.exit, tt.want)
		}
	}
}

// TestDecodeGatewayStatsUnreachable is acceptance box 2: the view reads
// unreachable (through the key Stats serves it under), a body without the key
// leaves it nil and never zero, and a key of the wrong type is an error.
func TestDecodeGatewayStatsUnreachable(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Stats{Unreachable: 4})
	if err != nil {
		t.Fatalf("marshal Stats: %v", err)
	}
	if got, err := decodeGatewayStats(body); err != nil || got.Unreachable == nil || *got.Unreachable != 4 {
		t.Errorf("decodeGatewayStats(%s) = %+v, %v; want Unreachable 4", body, got, err)
	}
	if got, err := decodeGatewayStats([]byte(`{"calls":1}`)); err != nil || got.Unreachable != nil {
		t.Errorf("decodeGatewayStats = %+v, %v; want Unreachable nil without the key", got, err)
	}
	if got, err := decodeGatewayStats([]byte(`{"unreachable":"many"}`)); err == nil {
		t.Errorf("decodeGatewayStats = %+v, nil; want an error for a wrong-typed key", got)
	}
}
