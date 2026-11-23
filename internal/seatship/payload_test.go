package seatship

import (
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

func testPayload() Payload {
	return Payload{Rig: "gastown", Polecat: "rictus", Bead: "gt-16i6rx", Submitted: true,
		Usage: &dashboard.UsageRecord{Bead: "gt-16i6rx", Rig: "gastown"}, SessionIDs: []string{"s1"},
		Outbox: map[string]string{"done": "gt-16i6rx"}, TranscriptTail: "tail"}
}

func TestSeatShipPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	in := testPayload()
	comment, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, found, err := Decode(comment)
	if err != nil || !found || !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %+v (found %v, err %v), want %+v", got, found, err, in)
	}
}

func TestSeatShipPayloadDropsTail(t *testing.T) {
	t.Parallel()
	in := testPayload()
	in.TranscriptTail = strings.Repeat("x", MaxCommentBytes)
	comment, err := Encode(in)
	if err != nil || len(comment) > MaxCommentBytes {
		t.Fatalf("Encode = %d bytes, err %v; want at most %d", len(comment), err, MaxCommentBytes)
	}
	got, found, err := Decode(comment)
	if err != nil || !found || got.TranscriptTail != "" || got.Bead != in.Bead {
		t.Errorf("Decode = bead %q, tail %d bytes; want bead %q and no tail", got.Bead, len(got.TranscriptTail), in.Bead)
	}
}

func TestSeatShipPayloadTooLarge(t *testing.T) {
	t.Parallel()
	in := testPayload()
	in.Outbox = map[string]string{"big": strings.Repeat("x", MaxCommentBytes)}
	if _, err := Encode(in); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Encode err = %v, want ErrTooLarge", err)
	}
}

func TestSeatShipPayloadMarshalError(t *testing.T) {
	t.Parallel()
	in := testPayload()
	in.Usage.EstCostUSD = math.NaN() // JSON has no NaN, so the marshal fails
	if _, err := Encode(in); err == nil {
		t.Fatal("Encode accepted a usage record JSON cannot encode")
	}
}

func TestSeatShipPayloadDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, comment  string
		found, wantErr bool
	}{
		{"foreign comment", "SP ESTIMATE v1 {}", false, false},
		{"malformed json", CommentPrefix + `{"bead":`, false, true},
		{"no bead", CommentPrefix + `{"rig":"gastown"}`, false, true},
		{"empty body", CommentPrefix, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, found, err := Decode(tt.comment); found != tt.found || (err != nil) != tt.wantErr {
				t.Errorf("Decode(%q) = found %v, err %v; want found %v, wantErr %v", tt.comment, found, err, tt.found, tt.wantErr)
			}
		})
	}
}
