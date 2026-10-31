package keystore

import (
	"context"
	"errors"
	"testing"
)

// put stores value under the test key and fails the test when Put does.
func put(t *testing.T, f *Fake, workspace, name, value string) Revision {
	t.Helper()
	rev, err := f.Put(context.Background(), workspace, name, []byte(value))
	if err != nil {
		t.Fatalf("Put(%q, %q): %v", workspace, name, err)
	}
	return rev
}

// get returns the value Get reads for the key and fails the test when Get does.
func get(t *testing.T, f *Fake, workspace, name string) string {
	t.Helper()
	secret, err := f.Get(context.Background(), workspace, name)
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", workspace, name, err)
	}
	return string(secret.Bytes())
}

func TestPutCreatesNewRevisionGetReturnsLatest(t *testing.T) {
	t.Parallel()
	f := NewFake()
	first := put(t, f, "ws", "api-key", "first-value")
	second := put(t, f, "ws", "api-key", "second-value")

	if second <= first {
		t.Fatalf("second Put returned revision %d, want above the first revision %d", second, first)
	}
	if got := get(t, f, "ws", "api-key"); got != "second-value" {
		t.Errorf("Get after two Puts = %q, want the latest value %q", got, "second-value")
	}
}

func TestDisabledRevisionNotReturnedToNewReads(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := context.Background()
	first := put(t, f, "ws", "api-key", "first-value")
	second := put(t, f, "ws", "api-key", "second-value")

	if err := f.Disable(ctx, "ws", "api-key", second); err != nil {
		t.Fatalf("Disable(%d): %v", second, err)
	}
	if got := get(t, f, "ws", "api-key"); got != "first-value" {
		t.Fatalf("Get after disabling the latest revision = %q, want the previous enabled value %q", got, "first-value")
	}

	if err := f.Destroy(ctx, "ws", "api-key", second); err != nil {
		t.Fatalf("Destroy(%d): %v", second, err)
	}
	if err := f.Disable(ctx, "ws", "api-key", second); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Disable of a destroyed revision = %v, want ErrRevisionNotFound", err)
	}

	if err := f.Destroy(ctx, "ws", "api-key", first); err != nil {
		t.Fatalf("Destroy(%d): %v", first, err)
	}
	if _, err := f.Get(ctx, "ws", "api-key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after destroying every revision = %v, want ErrNotFound", err)
	}
}

func TestGetUnknownKeyReturnsNotFound(t *testing.T) {
	t.Parallel()
	if _, err := NewFake().Get(context.Background(), "ws", "api-key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a key never put = %v, want ErrNotFound", err)
	}
}

func TestPutRejectsEmptyWorkspaceAndName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workspace string
		key       string
	}{
		{name: "empty workspace", workspace: "", key: "api-key"},
		{name: "empty key name", workspace: "ws", key: ""},
	}
	f := NewFake()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.Put(context.Background(), tt.workspace, tt.key, []byte("value")); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q, %q) = %v, want ErrInvalidKey", tt.workspace, tt.key, err)
			}
		})
	}
}

func TestPutRejectsEmptyValue(t *testing.T) {
	t.Parallel()
	if _, err := NewFake().Put(context.Background(), "ws", "api-key", nil); !errors.Is(err, ErrEmptyValue) {
		t.Errorf("Put of an empty value = %v, want ErrEmptyValue", err)
	}
}

func TestDisableAndDestroyRejectUnknownRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFake()
	put(t, f, "ws", "api-key", "value")
	const unknown Revision = 99

	if err := f.Disable(ctx, "ws", "api-key", unknown); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Disable of a revision never put = %v, want ErrRevisionNotFound", err)
	}
	if err := f.Destroy(ctx, "ws", "api-key", unknown); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Destroy of a revision never put = %v, want ErrRevisionNotFound", err)
	}
	if err := f.Destroy(ctx, "ws", "other", unknown); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Destroy in a key never put = %v, want ErrRevisionNotFound", err)
	}
}

func TestRevisionsAreNumberedPerKey(t *testing.T) {
	t.Parallel()
	f := NewFake()
	first := put(t, f, "ws", "api-key", "value")
	other := put(t, f, "ws", "other-key", "value")

	if other != first {
		t.Errorf("first revision of other-key = %d, want the same number %d: numbering is per key", other, first)
	}
}

func TestDestroyedRevisionNumberIsNotReused(t *testing.T) {
	t.Parallel()
	f := NewFake()
	first := put(t, f, "ws", "api-key", "first-value")
	if err := f.Destroy(context.Background(), "ws", "api-key", first); err != nil {
		t.Fatalf("Destroy(%d): %v", first, err)
	}
	second := put(t, f, "ws", "api-key", "second-value")

	if second <= first {
		t.Errorf("Put after destroying revision %d returned %d, want a number above it", first, second)
	}
}

func TestStoreCopiesValuesAtItsBoundary(t *testing.T) {
	t.Parallel()
	f := NewFake()
	value := []byte("original-value")
	if _, err := f.Put(context.Background(), "ws", "api-key", value); err != nil {
		t.Fatalf("Put: %v", err)
	}
	value[0] = 'X'

	secret, err := f.Get(context.Background(), "ws", "api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	read := secret.Bytes()
	if string(read) != "original-value" {
		t.Fatalf("Get returned %q after the caller rewrote its Put buffer, want %q", read, "original-value")
	}

	read[0] = 'Y'
	if got := get(t, f, "ws", "api-key"); got != "original-value" {
		t.Errorf("Get returned %q after the caller rewrote an earlier Bytes copy, want %q", got, "original-value")
	}
}

func TestCancelledContextStopsEveryMethod(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewFake()

	if _, err := f.Put(ctx, "ws", "api-key", []byte("value")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put with a canceled context = %v, want context.Canceled", err)
	}
	if _, err := f.Get(ctx, "ws", "api-key"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get with a canceled context = %v, want context.Canceled", err)
	}
	if err := f.Disable(ctx, "ws", "api-key", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Disable with a canceled context = %v, want context.Canceled", err)
	}
	if err := f.Destroy(ctx, "ws", "api-key", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Destroy with a canceled context = %v, want context.Canceled", err)
	}
}

func TestRevisionsListsSurvivorsAscendingWithState(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := context.Background()
	first := put(t, f, "ws", "api-key", "v1")
	second := put(t, f, "ws", "api-key", "v2")
	third := put(t, f, "ws", "api-key", "v3")
	if err := f.Disable(ctx, "ws", "api-key", first); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := f.Destroy(ctx, "ws", "api-key", second); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := f.Revisions(ctx, "ws", "api-key")
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	want := []RevisionInfo{{Revision: first, Enabled: false}, {Revision: third, Enabled: true}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Revisions = %v, want %v (the destroyed revision gone, the disabled one marked)", got, want)
	}
}

func TestRevisionsOfAnUnknownKeyIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	got, err := NewFake().Revisions(context.Background(), "ws", "never-written")
	if err != nil || len(got) != 0 {
		t.Errorf("Revisions of an unknown key = %v, %v; want an empty list and no error", got, err)
	}
}
