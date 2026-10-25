package store

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestHashTokenIsStableAndOneWay(t *testing.T) {
	t.Parallel() // unit tier: pure
	a, b := HashToken("trk_abc"), HashToken("trk_abd")
	if len(a) != 32 {
		t.Errorf("hash length = %d, want 32 (sha-256)", len(a))
	}
	if !bytes.Equal(a, HashToken("trk_abc")) {
		t.Error("the same token hashed differently twice")
	}
	if bytes.Equal(a, b) {
		t.Error("different tokens hashed alike")
	}
	if bytes.Contains(a, []byte("trk_abc")) {
		t.Error("the hash contains the token")
	}
}

func TestGenerateTokenShapeAndSource(t *testing.T) {
	t.Parallel()
	zeros := bytes.NewReader(make([]byte, 32))
	tok, err := generateToken(zeros)
	if err != nil {
		t.Fatal(err)
	}
	if want := "trk_" + strings.Repeat("00", 32); tok != want {
		t.Errorf("token = %q, want %q for an all-zero source", tok, want)
	}
	// A short source must fail, not return a weak token.
	if tok, err := generateToken(io.LimitReader(bytes.NewReader(make([]byte, 32)), 10)); err == nil {
		t.Errorf("generateToken with 10 random bytes = %q, nil; want an error", tok)
	}
}
