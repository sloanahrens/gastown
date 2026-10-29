package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/exectax"
	"github.com/steveyegge/gastown/internal/util"
)

// The commands' canned output. memory_pressure, top and ps print what the
// macOS tools print; the idle share is the last top sample's, as hoststat reads it.
const (
	memOut      = "The system has 17179869184 (4194304 pages with a page size of 4096).\nSystem-wide memory free percentage: 94%\n"
	topOut      = "Processes: 412 total\nCPU usage: 50.0% user, 30.0% sys, 20.0% idle\nCPU usage: 10.0% user, 20.0% sys, 70.0% idle\n"
	psOut       = "120.5 /usr/local/bin/dolt sql-server\n 40.0 /usr/local/bin/gt daemon run\n  0.1 /usr/sbin/cron\n"
	dockerPsOut = "gt-town-daemon-1\ngastown-dolt\n"
)

// The argv each command is answered under, joined with spaces.
var (
	memKey      = "memory_pressure"
	dockerPsKey = "docker --context desktop-linux ps --format {{.Names}}"
	topKey      = strings.Join(topArgv, " ")
	psKey       = strings.Join(psArgv, " ")
)

// fakeHost answers each command from its canned stdout, keyed by its argv, and
// records every argv it was asked. A command with no answer fails, as a missing
// command does.
type fakeHost struct {
	stdout map[string]string
	fail   map[string]error
	calls  [][]string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		stdout: map[string]string{
			memKey:      memOut,
			dockerPsKey: dockerPsOut,
			topKey:      topOut,
			psKey:       psOut,
		},
		fail: map[string]error{},
	}
}

func (h *fakeHost) run(_ context.Context, argv []string) ([]byte, error) {
	h.calls = append(h.calls, argv)
	key := strings.Join(argv, " ")
	if err := h.fail[key]; err != nil {
		return nil, err
	}
	out, ok := h.stdout[key]
	if !ok {
		return nil, fmt.Errorf("no answer for %q", key)
	}
	return []byte(out), nil
}

// fullCollectors is the set of reads that all succeed: the disk reads 61.5% used
// with 12 GiB free, and the exec tax is 25 ms.
func fullCollectors(h *fakeHost) collectors {
	return collectors{
		run: h.run,
		disk: func(string) (*util.DiskSpaceInfo, error) {
			return &util.DiskSpaceInfo{AvailableBytes: 12 << 30, UsedPercent: 61.5}, nil
		},
		execTax: func(context.Context) (time.Duration, error) { return 25 * time.Millisecond, nil },
		timeout: readTimeout,
	}
}

// fullProbeReading is what collect returns when every read succeeds.
func fullProbeReading() Probes {
	return Probes{
		V:            SchemaVersion,
		At:           testAt(),
		MemFreePct:   Field[float64]{Value: 94},
		Disk:         Field[Disk]{Value: Disk{UsedPct: 61.5, FreeGB: 12}},
		Containers:   Field[[]string]{Value: []string{"gt-town-daemon-1", "gastown-dolt"}},
		CPUIdlePct:   Field[float64]{Value: 70},
		TopConsumers: Field[[]Consumer]{Value: []Consumer{{Name: "dolt", CPU: 120.5}, {Name: "gt", CPU: 40}}},
		ExecTaxMS:    Field[float64]{Value: 25},
	}
}

func TestCollectFillsEveryFieldWhenEveryReadSucceeds(t *testing.T) {
	t.Parallel()
	got := collect(context.Background(), fullCollectors(newFakeHost()), "/town", testAt())
	if want := fullProbeReading(); !reflect.DeepEqual(got, want) {
		t.Errorf("collect = %+v, want %+v", got, want)
	}
}

// errsOf is each field's Err, by the name a failure table row uses.
func errsOf(p Probes) map[string]string {
	return map[string]string{
		"mem":        p.MemFreePct.Err,
		"disk":       p.Disk.Err,
		"containers": p.Containers.Err,
		"cpu":        p.CPUIdlePct.Err,
		"top":        p.TopConsumers.Err,
		"exec":       p.ExecTaxMS.Err,
	}
}

// valuesOf is each field's Value, by the same names.
func valuesOf(p Probes) map[string]any {
	return map[string]any{
		"mem":        p.MemFreePct.Value,
		"disk":       p.Disk.Value,
		"containers": p.Containers.Value,
		"cpu":        p.CPUIdlePct.Value,
		"top":        p.TopConsumers.Value,
		"exec":       p.ExecTaxMS.Value,
	}
}

func TestCollectNamesTheMissingTownRootAsTheDiskErr(t *testing.T) {
	t.Parallel()
	h := newFakeHost()
	got := collect(context.Background(), fullCollectors(h), "", testAt())
	if got.Disk.Err == "" {
		t.Error("Disk.Err is empty for a collect with no town root, want it set")
	}
	if got.MemFreePct.Err != "" || got.Containers.Err != "" {
		t.Errorf("a missing town root set other fields' Err: mem %q, containers %q", got.MemFreePct.Err, got.Containers.Err)
	}
}

func TestCollectSetsOnlyTheFailedFieldsErr(t *testing.T) {
	t.Parallel()
	failing := []struct {
		name  string
		field string
		setup func(h *fakeHost, c *collectors)
	}{
		{"memory_pressure fails", "mem", func(h *fakeHost, _ *collectors) {
			h.fail[memKey] = errors.New("exit status 1")
		}},
		{"memory_pressure prints no figure", "mem", func(h *fakeHost, _ *collectors) {
			h.stdout[memKey] = "nothing about memory here\n"
		}},
		{"docker ps fails", "containers", func(h *fakeHost, _ *collectors) {
			h.fail[dockerPsKey] = errors.New("exit status 1")
		}},
		{"top fails", "cpu", func(h *fakeHost, _ *collectors) {
			h.fail[topKey] = errors.New("signal: killed")
		}},
		{"top prints no idle share", "cpu", func(h *fakeHost, _ *collectors) {
			h.stdout[topKey] = "Processes: 412 total\n"
		}},
		{"ps fails", "top", func(h *fakeHost, _ *collectors) {
			h.fail[psKey] = errors.New("exit status 1")
		}},
		{"disk read fails", "disk", func(h *fakeHost, c *collectors) {
			c.disk = func(string) (*util.DiskSpaceInfo, error) { return nil, errors.New("statfs: no such file") }
		}},
		{"disk read returns no reading", "disk", func(h *fakeHost, c *collectors) {
			c.disk = func(string) (*util.DiskSpaceInfo, error) { return nil, nil }
		}},
		{"exec tax fails", "exec", func(h *fakeHost, c *collectors) {
			c.execTax = func(context.Context) (time.Duration, error) { return 0, errors.New("exec: permission denied") }
		}},
	}
	for _, row := range failing {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			h := newFakeHost()
			c := fullCollectors(h)
			row.setup(h, &c)
			got := collect(context.Background(), c, "/town", testAt())

			for name, e := range errsOf(got) {
				switch {
				case name == row.field && e == "":
					t.Errorf("%s: Err is empty, want it set", name)
				case name != row.field && e != "":
					t.Errorf("%s: Err = %q, want none: only %s failed", name, e, row.field)
				}
			}
			want := valuesOf(fullProbeReading())
			for name, v := range valuesOf(got) {
				if name != row.field && !reflect.DeepEqual(v, want[name]) {
					t.Errorf("%s: value = %v, want %v: a failed %s must not cost it", name, v, want[name], row.field)
				}
			}
		})
	}
}

// TestCollectBoundsEachReadSoAHungCommandFailsOnlyItsField holds ps until its
// read's deadline. The exec tax is read after ps and must still be filled: a
// read with no deadline of its own would hold it up. A ps with no deadline
// fails at once rather than waiting, so dropping the deadline turns this red.
func TestCollectBoundsEachReadSoAHungCommandFailsOnlyItsField(t *testing.T) {
	t.Parallel()
	h := newFakeHost()
	c := fullCollectors(h)
	c.timeout = time.Millisecond
	c.run = func(ctx context.Context, argv []string) ([]byte, error) {
		if strings.Join(argv, " ") != psKey {
			return h.run(ctx, argv)
		}
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("no deadline on the read")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	got := collect(context.Background(), c, "/town", testAt())

	if !strings.Contains(got.TopConsumers.Err, "context deadline exceeded") {
		t.Errorf("TopConsumers.Err = %q, want the deadline named", got.TopConsumers.Err)
	}
	for name, e := range errsOf(got) {
		if name != "top" && e != "" {
			t.Errorf("%s: Err = %q, want none: only the hung ps failed", name, e)
		}
	}
	want := valuesOf(fullProbeReading())
	for name, v := range valuesOf(got) {
		if name != "top" && !reflect.DeepEqual(v, want[name]) {
			t.Errorf("%s: value = %v, want %v: a hung ps must not cost it", name, v, want[name])
		}
	}
}

// dockerContextErr names how a Docker argv fails to name the desktop-linux
// context, or returns nil when it does.
func dockerContextErr(argv []string) error {
	if len(argv) == 0 || argv[0] != "docker" {
		return nil
	}
	if len(argv) < 3 || argv[1] != "--context" || argv[2] != dockerContext {
		return fmt.Errorf("docker call %q does not name --context %s", strings.Join(argv, " "), dockerContext)
	}
	return nil
}

func TestCollectNamesTheDesktopContextOnEveryDockerCall(t *testing.T) {
	t.Parallel()
	h := newFakeHost()
	collect(context.Background(), fullCollectors(h), "/town", testAt())

	var docker int
	for _, argv := range h.calls {
		if len(argv) > 0 && argv[0] == "docker" {
			docker++
		}
		if err := dockerContextErr(argv); err != nil {
			t.Error(err)
		}
	}
	if docker == 0 {
		t.Fatal("no Docker call was recorded, so the context check proved nothing")
	}
}

func TestDockerContextErrFlagsAnArgvThatOmitsTheContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		argv []string
		bad  bool
	}{
		{"names the context", []string{"docker", "--context", "desktop-linux", "ps"}, false},
		{"omits the context", []string{"docker", "ps", "--format", "{{.Names}}"}, true},
		{"reads the global context", []string{"docker", "context", "show"}, true},
		{"names another context", []string{"docker", "--context", "default", "ps"}, true},
		{"is not a Docker call", []string{"memory_pressure"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := dockerContextErr(tc.argv); (err != nil) != tc.bad {
				t.Errorf("dockerContextErr(%q) = %v, want an error: %v", tc.argv, err, tc.bad)
			}
		})
	}
}

// TestProbeExecTaxRunsEachSampleThroughTheRunner stands in for exectax.Probe
// with a probe that runs its one sample, so no program is written.
func TestProbeExecTaxRunsEachSampleThroughTheRunner(t *testing.T) {
	t.Parallel()
	var ran [][]string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		ran = append(ran, argv)
		return nil, nil
	}
	probe := func(ctx context.Context, o exectax.Options) (exectax.Result, error) {
		d, err := o.Exec(ctx, "/sample/one")
		if err != nil {
			return exectax.Result{}, err
		}
		return exectax.Result{Median: d, Samples: []time.Duration{d}}, nil
	}
	if _, err := probeExecTax(context.Background(), probe, run); err != nil {
		t.Fatalf("probeExecTax: %v", err)
	}
	if want := [][]string{{"/sample/one"}}; !reflect.DeepEqual(ran, want) {
		t.Errorf("runner was asked %q, want %q", ran, want)
	}
}

func TestProbeExecTaxReportsAFailure(t *testing.T) {
	t.Parallel()
	okRun := func(context.Context, []string) ([]byte, error) { return nil, nil }
	failRun := func(context.Context, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	failProbe := func(context.Context, exectax.Options) (exectax.Result, error) {
		return exectax.Result{}, errors.New("no samples written")
	}
	emptyProbe := func(context.Context, exectax.Options) (exectax.Result, error) {
		return exectax.Result{}, nil
	}
	passProbe := func(ctx context.Context, o exectax.Options) (exectax.Result, error) {
		_, err := o.Exec(ctx, "/sample/one")
		return exectax.Result{}, err
	}
	for _, tc := range []struct {
		name  string
		probe execProbe
		run   Runner
	}{
		{"the probe fails", failProbe, okRun},
		{"the probe takes no samples", emptyProbe, okRun},
		{"a sample fails to exec", passProbe, failRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := probeExecTax(context.Background(), tc.probe, tc.run); err == nil {
				t.Error("probeExecTax returned no error, want one")
			}
		})
	}
}

func TestParseContainers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"no containers is an empty list", "", []string{}},
		{"one name per line", "gt-town-daemon-1\ngastown-dolt\n", []string{"gt-town-daemon-1", "gastown-dolt"}},
		{"blank lines and spaces are dropped", "\n  a \n\n", []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseContainers(tc.out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseContainers(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}
