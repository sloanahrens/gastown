package beads

import (
	"errors"
	"testing"
)

// TestSQLRefusalMemoryRemembersRefusal: once a run returns the gateway
// refusal, the next Run returns that error without calling read.
func TestSQLRefusalMemoryRemembersRefusal(t *testing.T) {
	t.Parallel()
	var mem SQLRefusalMemory
	refusal := errors.New(gatewaySQLRefusal)
	calls := 0
	read := func() ([]byte, error) { calls++; return nil, refusal }
	if _, err := mem.Run(read); !errors.Is(err, refusal) {
		t.Fatalf("first Run error = %v, want the gateway refusal", err)
	}
	if _, err := mem.Run(read); !errors.Is(err, refusal) {
		t.Errorf("second Run error = %v, want the stored refusal", err)
	}
	if calls != 1 {
		t.Errorf("read called %d times, want 1", calls)
	}
}

// TestSQLRefusalMemoryDoesNotStoreOtherFailures: an error that is not the
// refusal is returned but not remembered, so the next Run calls read.
func TestSQLRefusalMemoryDoesNotStoreOtherFailures(t *testing.T) {
	t.Parallel()
	var mem SQLRefusalMemory
	otherErr := errors.New("bd unavailable")
	want := []byte("read after the unrelated failure")
	calls := 0
	read := func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, otherErr
		}
		return want, nil
	}
	if _, err := mem.Run(read); !errors.Is(err, otherErr) {
		t.Fatalf("first Run error = %v, want %v", err, otherErr)
	}
	out, err := mem.Run(read)
	if err != nil {
		t.Fatalf("second Run error = %v, want nil", err)
	}
	if string(out) != string(want) {
		t.Errorf("second Run output = %q, want %q", out, want)
	}
	if calls != 2 {
		t.Errorf("read called %d times, want 2", calls)
	}
}

// TestSQLRefusalMemoryNilCallsRead: a nil memory has nothing remembered, so
// every Run calls read.
func TestSQLRefusalMemoryNilCallsRead(t *testing.T) {
	t.Parallel()
	var mem *SQLRefusalMemory
	calls := 0
	read := func() ([]byte, error) { calls++; return []byte("data"), nil }
	for i := 0; i < 2; i++ {
		if _, err := mem.Run(read); err != nil {
			t.Fatalf("Run %d error = %v, want nil", i+1, err)
		}
	}
	if calls != 2 {
		t.Errorf("read called %d times, want 2", calls)
	}
}
