package doltserver

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recordedVMMapSummary is `vmmap -summary <pid>` output as macOS prints it for
// the Dolt server on 2026-10-28 (gt-v1zyv): RSS read 14.6 GB while the
// footprint was 1.7 GB, peaking at 10.2 GB.
const recordedVMMapSummary = `Process:         dolt [4711]
Path:            /usr/local/bin/dolt
Identifier:      dolt
Version:         0
Code Type:       ARM-64 (Native)
Platform:        macOS-15.0.1-arm64
Parent Process:  gt [1]

Virtual Memory Map:

Physical footprint:         1.7G
Physical footprint (peak):  10.2G
`

// TestParseVMMapFootprint reads recorded vmmap -summary output, one case per
// unit vmmap prints.
func TestParseVMMapFootprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		out           string
		wantFootprint int64
		wantPeak      int64
	}{
		{
			name:          "recorded summary in gigabytes",
			out:           recordedVMMapSummary,
			wantFootprint: 1825361101, // 1.7 GiB, rounded
			wantPeak:      10952166605,
		},
		{
			name:          "kilobytes",
			out:           "Physical footprint:         512.0K\nPhysical footprint (peak):  900.0K\n",
			wantFootprint: 524288,
			wantPeak:      921600,
		},
		{
			name:          "megabytes",
			out:           "Physical footprint:         34.5M\nPhysical footprint (peak):  128.0M\n",
			wantFootprint: 36175872,
			wantPeak:      134217728,
		},
		{
			name:          "missing lines leave both zero",
			out:           "Process: dolt [4711]\nVirtual Memory Map:\n",
			wantFootprint: 0,
			wantPeak:      0,
		},
		{
			name:          "missing peak line leaves the footprint",
			out:           "Physical footprint:         1.7G\n",
			wantFootprint: 1825361101,
			wantPeak:      0,
		},
		{
			name:          "value without a unit reads as zero",
			out:           "Physical footprint:         1.7\nPhysical footprint (peak):  10.2G\n",
			wantFootprint: 0,
			wantPeak:      10952166605,
		},
		{
			name:          "NaN reads as zero",
			out:           "Physical footprint:         NaNG\nPhysical footprint (peak):  10.2G\n",
			wantFootprint: 0,
			wantPeak:      10952166605,
		},
		{
			name:          "infinity reads as zero",
			out:           "Physical footprint:         InfG\nPhysical footprint (peak):  10.2G\n",
			wantFootprint: 0,
			wantPeak:      10952166605,
		},
		{
			name:          "size past int64 reads as zero",
			out:           "Physical footprint:         1e300G\nPhysical footprint (peak):  10.2G\n",
			wantFootprint: 0,
			wantPeak:      10952166605,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			footprint, peak := parseVMMapFootprint(tc.out)
			if footprint != tc.wantFootprint {
				t.Errorf("footprint = %d bytes, want %d", footprint, tc.wantFootprint)
			}
			if peak != tc.wantPeak {
				t.Errorf("peak = %d bytes, want %d", peak, tc.wantPeak)
			}
		})
	}
}

// TestServerFootprint drives the reader through a host: the darwin path reads
// a scripted vmmap, and every way the number can be unavailable reads as
// zero rather than an error.
func TestServerFootprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		goos          string
		pid           int
		vmmap         *fakeReply
		wantFootprint int64
		wantPeak      int64
		wantVMMapRan  bool
	}{
		{
			name:          "darwin reads the footprint",
			goos:          "darwin",
			pid:           4711,
			vmmap:         &fakeReply{stdout: recordedVMMapSummary},
			wantFootprint: 1825361101,
			wantPeak:      10952166605,
			wantVMMapRan:  true,
		},
		{
			name:         "vmmap failure leaves both zero",
			goos:         "darwin",
			pid:          4711,
			vmmap:        &fakeReply{stderr: "vmmap: pid 4711 is not a valid process\n", code: 1},
			wantVMMapRan: true,
		},
		{
			name:         "output without the lines leaves both zero",
			goos:         "darwin",
			pid:          4711,
			vmmap:        &fakeReply{stdout: "Process: dolt [4711]\n"},
			wantVMMapRan: true,
		},
		{
			name: "non-darwin host never runs vmmap",
			goos: "linux",
			pid:  4711,
		},
		{
			name: "no pid never runs vmmap",
			goos: "darwin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeHost()
			if tc.vmmap != nil {
				f.on("vmmap -summary 4711", *tc.vmmap)
			}
			h := f.host()
			h.goos = func() string { return tc.goos }

			footprint, peak := h.serverFootprint(tc.pid)

			if footprint != tc.wantFootprint {
				t.Errorf("footprint = %d bytes, want %d", footprint, tc.wantFootprint)
			}
			if peak != tc.wantPeak {
				t.Errorf("peak = %d bytes, want %d", peak, tc.wantPeak)
			}
			if ran := len(f.ranMatching("vmmap")) > 0; ran != tc.wantVMMapRan {
				t.Errorf("vmmap ran = %v, want %v (commands: %v)", ran, tc.wantVMMapRan, f.commands())
			}
		})
	}
}

// TestGetHealthMetrics_Footprint proves the status path carries the footprint:
// with the platform's vmmap answering, GetHealthMetrics reports it, and a
// vmmap that fails leaves the fields zero without marking the server unhealthy.
func TestGetHealthMetrics_Footprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		vmmap         fakeReply
		wantFootprint int64
		wantPeak      int64
	}{
		{
			name:          "footprint reported",
			vmmap:         fakeReply{stdout: recordedVMMapSummary},
			wantFootprint: 1825361101,
			wantPeak:      10952166605,
		},
		{
			name:  "vmmap failure reports nothing",
			vmmap: fakeReply{stderr: "vmmap: cannot read process\n", code: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const port = 4599
			f := newFakeHost().townPort(port).on("dolt *", fakeReply{})
			h := f.host()
			h.goos = func() string { return "darwin" }
			townRoot := testTown(t)
			pid := f.doltServer(townRoot, port)
			writePIDFile(t, h, townRoot, pid)
			f.on("vmmap -summary "+strconv.Itoa(pid), tc.vmmap)

			metrics := h.GetHealthMetrics(townRoot)

			if metrics.FootprintBytes != tc.wantFootprint {
				t.Errorf("FootprintBytes = %d, want %d", metrics.FootprintBytes, tc.wantFootprint)
			}
			if metrics.FootprintPeakBytes != tc.wantPeak {
				t.Errorf("FootprintPeakBytes = %d, want %d", metrics.FootprintPeakBytes, tc.wantPeak)
			}
			for _, w := range metrics.Warnings {
				if strings.Contains(w, "footprint") {
					t.Errorf("footprint warned in the status output: %q", w)
				}
			}
			if !metrics.Healthy {
				t.Errorf("footprint marked the server unhealthy: warnings %v", metrics.Warnings)
			}
		})
	}
}

// A vmmap that never answers must not hold the status path. The test injects
// a short deadline for vmmap through probeTimeout and records that vmmap was
// asked for one. GetHealthMetrics returns while the vmmap command is still
// hung (the release channel closes only after the call), which only the
// deadline allows; the footprint reads as unavailable. No clock is read, and
// awaitHung proves the hang was reached.
func TestGetHealthMetrics_HungVMMapReadsAsUnavailable(t *testing.T) {
	t.Parallel()
	const port = 4600
	f := newFakeHost().townPort(port).on("dolt *", fakeReply{})
	h := f.host()
	h.goos = func() string { return "darwin" }
	vmmapAsked := false
	h.probeTimeout = func(argv []string) time.Duration {
		if argv[0] != "vmmap" {
			return 0
		}
		vmmapAsked = true
		return time.Millisecond
	}
	townRoot := testTown(t)
	pid := f.doltServer(townRoot, port)
	writePIDFile(t, h, townRoot, pid)
	key := "vmmap -summary " + strconv.Itoa(pid)
	release := f.hangOn(key)
	defer close(release)

	metrics := h.GetHealthMetrics(townRoot)
	f.awaitHung(t, key)

	if !vmmapAsked {
		t.Error("vmmap was never asked for a probe deadline, so the footprint probe did not go through probeTimeout")
	}
	if metrics.FootprintBytes != 0 || metrics.FootprintPeakBytes != 0 {
		t.Errorf("hung vmmap reported footprint %d / peak %d, want both zero",
			metrics.FootprintBytes, metrics.FootprintPeakBytes)
	}
}

// TestProductionProbeTimeout pins the deadlines the real machine gives each
// probe: vmmap gets the footprint deadline, which is below the default, and
// every other probe gets zero, so it keeps probeTimeoutDefault.
func TestProductionProbeTimeout(t *testing.T) {
	t.Parallel()

	if footprintProbeTimeout >= probeTimeoutDefault {
		t.Errorf("footprint deadline %v is not below the default %v", footprintProbeTimeout, probeTimeoutDefault)
	}

	tests := []struct {
		name string
		argv []string
		want time.Duration
	}{
		{name: "vmmap gets the footprint deadline", argv: []string{"vmmap", "-summary", "4711"}, want: footprintProbeTimeout},
		{name: "lsof keeps the default", argv: []string{"lsof", "-i", ":4600", "-sTCP:LISTEN", "-t"}},
		{name: "ss keeps the default", argv: []string{"ss", "-tlnp", "sport = :4600"}},
		{name: "docker keeps the default", argv: []string{"docker", "inspect", "-f", "{{.State.Running}}", "dolt"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := productionProbeTimeout(tc.argv); got != tc.want {
				t.Errorf("productionProbeTimeout(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestStdProbeTimeoutIsWired pins that std gives vmmap the footprint deadline
// and lsof zero (the default). A nil probeTimeout would give vmmap the default.
func TestStdProbeTimeoutIsWired(t *testing.T) {
	t.Parallel()
	if std.probeTimeout == nil {
		t.Fatal("std.probeTimeout is nil: vmmap gets probeTimeoutDefault, not the footprint deadline")
	}
	for _, tc := range []struct {
		argv []string
		want time.Duration
	}{
		{argv: []string{"vmmap", "-summary", "4711"}, want: footprintProbeTimeout},
		{argv: []string{"lsof", "-i", ":4600", "-sTCP:LISTEN", "-t"}, want: 0},
	} {
		if got := std.probeTimeout(tc.argv); got != tc.want {
			t.Errorf("std.probeTimeout(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestHealthMetrics_FootprintJSON shows the status JSON carries both values
// under their own keys.
func TestHealthMetrics_FootprintJSON(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(&HealthMetrics{FootprintBytes: 1825361101, FootprintPeakBytes: 10952166605})
	if err != nil {
		t.Fatalf("marshal HealthMetrics: %v", err)
	}
	for _, want := range []string{`"footprint_bytes":1825361101`, `"footprint_peak_bytes":10952166605`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON %s does not carry %s", data, want)
		}
	}
}
