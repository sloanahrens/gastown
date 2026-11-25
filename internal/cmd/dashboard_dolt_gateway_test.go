package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// TestDoltGatewaySectionShowsInFlight: a read whose answer carries in_flight 3
// and peak_in_flight 7 draws the six ten-minute rows, the row holding now last,
// and a note naming both counts and when the per-ten-minute counts begin.
func TestDoltGatewaySectionShowsInFlight(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 24, 0, 0, time.UTC)
	section := newDoltGatewaySection(func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{InFlight: 3, PeakInFlight: 7, CapsPresent: true}, nil
	}, "bd-gateway.gt.svc:7440")
	got, err := section.read(context.Background(), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Title != "bd gateway load" {
		t.Errorf("title = %q, want %q", got.Title, "bd gateway load")
	}
	if len(got.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(got.Rows))
	}
	if first, last := got.Rows[0][0], got.Rows[5][0]; first != "14:30-14:40" || last != "15:20-15:30" {
		t.Errorf("rows span %q to %q, want 14:30-14:40 to 15:20-15:30", first, last)
	}
	for _, want := range []string{"in flight now: 3", "peak in flight since start: 7", "start when the dashboard starts"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note = %q, want it to hold %q", got.Note, want)
		}
	}

	noCaps := newDoltGatewaySection(func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, nil
	}, "bd-gateway.gt.svc:7440")
	got, err = noCaps.read(context.Background(), now)
	if err != nil {
		t.Fatalf("read without caps: %v", err)
	}
	if !strings.Contains(got.Note, "carry no in-flight or 429 counters") {
		t.Errorf("note = %q, want it to say the counters are missing", got.Note)
	}
}

// TestDoltGatewaySectionErrsNamingTheHost: a stats read that fails is the
// section's error naming the gateway's host and the error, never the bearer
// token the pusher read from its token file.
func TestDoltGatewaySectionErrsNamingTheHost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	token := "s3cret-token-value"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	unreachable := bdgateway.GatewayPusher{
		URL:       "http://127.0.0.1:7440",
		TokenFile: tokenFile,
		HTTP:      doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }),
	}
	for _, tc := range []struct {
		name string
		read doltSectionRead
		want string
	}{
		{"a fake stats reader's error", newDoltGatewaySection(
			func(context.Context) (bdgateway.GatewayStats, error) {
				return bdgateway.GatewayStats{}, errors.New("dial tcp: connection refused")
			},
			"bd-gateway.gt.svc:7440").read, "bd-gateway.gt.svc:7440"},
		{"a pusher whose gateway does not answer", newDoltGatewaySection(
			unreachable.Stats, doltGatewayHost(unreachable.URL)).read, "127.0.0.1:7440"},
		{"a real transport error that repeats the whole request URL", newDoltGatewaySection(
			func(context.Context) (bdgateway.GatewayStats, error) {
				return bdgateway.GatewayStats{}, &url.Error{Op: "Get", URL: "http://user:pw" + "@bd-gateway.gt.svc:7440/v1/stats?token=abc", Err: errors.New("connection refused")}
			},
			"bd-gateway.gt.svc:7440").read, "bd-gateway.gt.svc:7440"},
		{"no gateway url configured", newDoltGatewayReader(func(string) string { return "" }), "no gateway url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.read(context.Background(), now)
			if err == nil {
				t.Fatal("read = nil error, want the unreadable gateway refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
			for _, leak := range []string{"http://", "https://", "/v1/stats", "token=abc", "user:pw"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("err = %q, want the host only: it holds %q of the request URL", err, leak)
				}
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("err = %q, want no bearer token", err)
			}
		})
	}
}

// The section is registered under `gateway` by its own file.
func TestDoltGatewaySectionRegisters(t *testing.T) {
	t.Parallel()

	for _, r := range doltSections.snapshot() {
		if r.key == doltGatewaySectionKey {
			return
		}
	}
	t.Fatalf("the Dolt panel has no %s section registered", doltGatewaySectionKey)
}
