package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// TestGatewayCapTrackerGrowth feeds a tracker successive /v1/stats reads. The
// first read only sets the base; a TooManyCalls rise since the previous read
// raises with the growth; Peak is each read's PeakInFlight.
func TestGatewayCapTrackerGrowth(t *testing.T) {
	t.Parallel()
	type read struct {
		tooManyCalls uint64
		peak         int
		want         gatewayCapReading
	}
	cases := []struct {
		name  string
		reads []read
	}{
		{
			name: "first read only sets the base",
			reads: []read{
				{tooManyCalls: 7, peak: 4, want: gatewayCapReading{Peak: 4}},
			},
		},
		{
			name: "growth since the previous read raises with the growth",
			reads: []read{
				{tooManyCalls: 0, peak: 2, want: gatewayCapReading{Peak: 2}},
				{tooManyCalls: 3, peak: 5, want: gatewayCapReading{Raise: true, Refused: 3, Peak: 5}},
			},
		},
		{
			name: "a read equal to the previous one raises nothing",
			reads: []read{
				{tooManyCalls: 0, peak: 2, want: gatewayCapReading{Peak: 2}},
				{tooManyCalls: 3, peak: 5, want: gatewayCapReading{Raise: true, Refused: 3, Peak: 5}},
				{tooManyCalls: 3, peak: 5, want: gatewayCapReading{Peak: 5}},
			},
		},
		{
			name: "a counter drop raises nothing",
			reads: []read{
				{tooManyCalls: 5, peak: 1, want: gatewayCapReading{Peak: 1}},
				{tooManyCalls: 2, peak: 1, want: gatewayCapReading{Peak: 1}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tracker gatewayCapTracker
			for i, r := range tc.reads {
				got := tracker.observe(bdgateway.GatewayStats{TooManyCalls: r.tooManyCalls, PeakInFlight: r.peak, CapsPresent: true})
				if got != r.want {
					t.Fatalf("read %d: observe(TooManyCalls=%d, PeakInFlight=%d) = %+v, want %+v",
						i, r.tooManyCalls, r.peak, got, r.want)
				}
			}
		})
	}
}

// TestGatewayCapTrackerAbsentRaisesNothing: a read whose body carried no cap
// fields (CapsPresent false) is not a counter of zero. It raises nothing and
// leaves the base where it was, so a present read after it measures growth from
// the last present base, and a first present read after it only sets the base.
func TestGatewayCapTrackerAbsentRaisesNothing(t *testing.T) {
	t.Parallel()
	type read struct {
		absent       bool
		tooManyCalls uint64
		peak         int
		want         gatewayCapReading
	}
	cases := []struct {
		name  string
		reads []read
	}{
		{
			name: "an absent read raises nothing and the next present read raises from the old base",
			reads: []read{
				{tooManyCalls: 0, peak: 2, want: gatewayCapReading{Peak: 2}},
				{absent: true, tooManyCalls: 9, want: gatewayCapReading{}},
				{tooManyCalls: 3, peak: 5, want: gatewayCapReading{Raise: true, Refused: 3, Peak: 5}},
			},
		},
		{
			name: "an absent first read does not set the base",
			reads: []read{
				{absent: true, tooManyCalls: 0, want: gatewayCapReading{}},
				{tooManyCalls: 3, peak: 1, want: gatewayCapReading{Peak: 1}},
				{tooManyCalls: 4, peak: 1, want: gatewayCapReading{Raise: true, Refused: 1, Peak: 1}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tracker gatewayCapTracker
			for i, r := range tc.reads {
				got := tracker.observe(bdgateway.GatewayStats{
					TooManyCalls: r.tooManyCalls, PeakInFlight: r.peak, CapsPresent: !r.absent,
				})
				if got != r.want {
					t.Fatalf("read %d: observe(absent=%v, TooManyCalls=%d, PeakInFlight=%d) = %+v, want %+v",
						i, r.absent, r.tooManyCalls, r.peak, got, r.want)
				}
			}
		})
	}
}

// TestGatewayCapTrackerRestart: a gateway restart resets TooManyCalls lower. The
// drop raises nothing and sets the new base, so the next growth raises from that
// base rather than from the counter the restart replaced.
func TestGatewayCapTrackerRestart(t *testing.T) {
	t.Parallel()
	type read struct {
		tooManyCalls uint64
		peak         int
		want         gatewayCapReading
	}
	cases := []struct {
		name  string
		reads []read
	}{
		{
			name: "a restart to a lower base raises from the new base",
			reads: []read{
				{tooManyCalls: 4, peak: 1, want: gatewayCapReading{Peak: 1}},
				{tooManyCalls: 9, peak: 2, want: gatewayCapReading{Raise: true, Refused: 5, Peak: 2}},
				{tooManyCalls: 1, peak: 0, want: gatewayCapReading{Peak: 0}},
				{tooManyCalls: 3, peak: 1, want: gatewayCapReading{Raise: true, Refused: 2, Peak: 1}},
			},
		},
		{
			name: "a restart to zero raises from zero",
			reads: []read{
				{tooManyCalls: 9, peak: 3, want: gatewayCapReading{Peak: 3}},
				{tooManyCalls: 0, peak: 0, want: gatewayCapReading{Peak: 0}},
				{tooManyCalls: 2, peak: 1, want: gatewayCapReading{Raise: true, Refused: 2, Peak: 1}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tracker gatewayCapTracker
			for i, r := range tc.reads {
				got := tracker.observe(bdgateway.GatewayStats{TooManyCalls: r.tooManyCalls, PeakInFlight: r.peak, CapsPresent: true})
				if got != r.want {
					t.Fatalf("read %d: observe(TooManyCalls=%d, PeakInFlight=%d) = %+v, want %+v",
						i, r.tooManyCalls, r.peak, got, r.want)
				}
			}
		})
	}
}
