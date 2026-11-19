package session

import (
	"reflect"
	"testing"
)

// TestSandboxCensusRunning reads the gateway's listing through the census and
// answers the names it lists as Ready. An empty or failed listing is an error,
// never an empty list.
func TestSandboxCensusRunning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listing string
		listErr int
		want    []string
		wantErr bool
	}{
		{
			name:    "ready sandboxes only, sorted",
			listing: listingOf(map[string]string{"gt-b": "Ready", "gt-stopped": "Stopped", "gt-a": "Ready", "gt-err": "Error"}),
			want:    []string{"gt-a", "gt-b"},
		},
		{
			name:    "no ready sandbox is an empty answer, not an error",
			listing: listingOf(map[string]string{"gt-stopped": "Stopped"}),
			want:    nil,
		},
		{
			name:    "empty listing is an error",
			listing: listingOf(map[string]string{}),
			wantErr: true,
		},
		{
			name:    "failed listing is an error",
			listing: listingOf(map[string]string{"gt-a": "Ready"}),
			listErr: 1,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gw := &censusGateway{listing: tc.listing, listErrs: tc.listErr}
			got, err := NewSandboxCensus("", gw.run).Running("gw")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Running error = %v, want error %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Running = %v, want %v", got, tc.want)
			}
		})
	}
}
