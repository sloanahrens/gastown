package keystore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	const value = "customer-key-034c1f9e2b7a4d55"
	const distinctive = "034c1f9e2b7a4d55"
	ctx := context.Background()
	f := NewFake()
	rev := put(t, f, "ws", "api-key", value)

	secret, err := f.Get(ctx, "ws", "api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	printed := map[string]string{
		"%v":                     fmt.Sprintf("%v", secret),
		"%s":                     fmt.Sprintf("%s", secret),
		"%+v":                    fmt.Sprintf("%+v", secret),
		"%#v":                    fmt.Sprintf("%#v", secret),
		"%q":                     fmt.Sprintf("%q", secret),
		"Sprint":                 fmt.Sprint(secret),
		"in a struct":            fmt.Sprintf("%v", struct{ Key Secret }{secret}),
		"in an unexported field": fmt.Sprintf("%+v", struct{ key Secret }{secret}),
	}
	encoded, err := json.Marshal(struct{ Key Secret }{secret})
	if err != nil {
		t.Fatalf("marshal a struct holding a Secret: %v", err)
	}
	printed["json"] = string(encoded)

	// The error of a failed Get is built while the store still holds the value.
	if err := f.Disable(ctx, "ws", "api-key", rev); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_, getErr := f.Get(ctx, "ws", "api-key")
	if getErr == nil {
		t.Fatal("Get after disabling the only revision returned no error")
	}
	printed["error"] = getErr.Error()
	printed["wrapped error"] = fmt.Errorf("load key: %w", getErr).Error()

	for label, out := range printed {
		if strings.Contains(out, value) {
			t.Errorf("%s printed the value %q, want it redacted: %s", label, value, out)
		}
		if strings.Contains(out, distinctive) {
			t.Errorf("%s printed part of the value %q, want it redacted: %s", label, distinctive, out)
		}
	}

	if got := fmt.Sprintf("%v", secret); got != "[redacted]" {
		t.Errorf("fmt of a Secret = %q, want %q", got, "[redacted]")
	}
}

// Destroy clears the value of a Secret a reader already holds, so the value is
// not reachable through a copy read before the revision was destroyed.
func TestDestroyClearsASecretAlreadyRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFake()
	rev := put(t, f, "ws", "api-key", "customer-key-value")
	secret, err := f.Get(ctx, "ws", "api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := f.Destroy(ctx, "ws", "api-key", rev); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got := secret.Bytes(); len(got) != 0 {
		t.Errorf("Bytes of a Secret read before Destroy = %q, want it cleared", got)
	}
}
