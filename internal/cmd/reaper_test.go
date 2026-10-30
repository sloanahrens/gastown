package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestReaperDatabaseNamesTrimsConfiguredList(t *testing.T) {
	t.Parallel()
	got := parseReaperDatabaseList(" hq, gastown ,, beads ")
	want := []string{"hq", "gastown", "beads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reaperDatabaseNames() = %#v, want %#v", got, want)
	}
}

// TestReaperAutoClosePreviewFlag pins the flag that carries the dry run's
// authorization (gt-39bu). `gt reaper run` composes its own preview in-process,
// so the flag belongs to auto-close alone; a bare `gt reaper auto-close` has to
// be able to refuse for want of it.
func TestReaperAutoClosePreviewFlag(t *testing.T) {
	t.Parallel()
	flag := reaperAutoCloseCmd.Flags().Lookup("preview")
	if flag == nil {
		t.Fatal("gt reaper auto-close has no --preview flag: a live run could not be bound to a dry run")
	}
	if flag.DefValue != "" {
		t.Errorf("--preview default = %q, want empty: a default would authorize every live run", flag.DefValue)
	}
	if runFlag := reaperRunCmd.Flags().Lookup("preview"); runFlag != nil {
		t.Error("gt reaper run takes --preview, but it previews in-process before writing and should not accept a caller's hash")
	}
}

func TestWaitBeforeReaperDatabase(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err := waitBeforeReaperDatabase(clk, 0, "not-a-duration"); err != nil {
		t.Fatalf("first database wait returned error: %v", err)
	}
	if err := waitBeforeReaperDatabase(clk, 1, "0s"); err != nil {
		t.Fatalf("zero-delay wait returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- waitBeforeReaperDatabase(clk, 1, "250ms") }()
	if err := clk.BlockUntilContext(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(250 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("250ms wait returned error: %v", err)
	}

	if err := waitBeforeReaperDatabase(clk, 1, "not-a-duration"); err == nil {
		t.Fatal("invalid delay should return an error")
	}
}

// Without a town endpoint the reaper gets no port, never a guessed 3307
// (gt-y3pgh.3).
func TestDefaultReaperEndpointWithoutTownHasNoPort(t *testing.T) {
	t.Parallel()
	host, port := reaperEndpoint("")
	if host != "127.0.0.1" || port != 0 {
		t.Fatalf("reaperEndpoint(\"\") = %s:%d, want 127.0.0.1:0", host, port)
	}
}

func TestDefaultReaperEndpointUsesTownConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	host, port := reaperEndpoint(townRoot)
	if host != "127.0.0.2" || port != 5507 {
		t.Fatalf("defaultReaperEndpoint() = %s:%d, want 127.0.0.2:5507", host, port)
	}
}

// TestWriteAutoCloseReportPrintsTheFloorRefusal covers the reporting gap the
// below-floor refusal opened. The refusal arrives with the candidate set in the
// result, and that set is the whole report — nothing was closed, so the set the
// mis-set threshold would take is the only thing there is to act on. A report
// gated on the error instead would drop it and print "auto-closed 0" alone,
// which reads as a clean run rather than as the threshold to go fix (gt-ecpj).
func TestWriteAutoCloseReportPrintsTheFloorRefusal(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database:  "hq",
		Floored:   true,
		FlooredAt: time.Hour,
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 60, Database: "hq"},
			{ID: "hq-b", Title: "abandoned hq-b", AgeDays: 60, Database: "hq"},
		},
	})

	if closed != 0 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 0: a below-floor sweep closes nothing", closed)
	}
	if want := reaper.FloorNotice(time.Hour, 2); !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want the below-floor notice %q", stderr.String(), want)
	}
	for _, id := range []string{"hq-a", "hq-b"} {
		if !strings.Contains(stdout.String(), id) {
			t.Errorf("stdout = %q, want the refused candidate %s listed: the set is the refusal's report",
				stdout.String(), id)
		}
	}
	if strings.Contains(stdout.String(), "auto-closed") {
		t.Errorf("stdout = %q, want no close count for a refused sweep: nothing was closed", stdout.String())
	}
}

// TestWriteAutoCloseReportHintsNoPreviewForARefusal keeps the refusal from
// reading as a pass in dry-run mode. A below-floor dry run still has a candidate
// count and a preview hash, so the ordinary "[DRY RUN] would auto-closed N —
// live run: --preview=H" line would both claim a close and hand over a hash the
// live run refuses (gt-ecpj).
func TestWriteAutoCloseReportHintsNoPreviewForARefusal(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database:    "hq",
		DryRun:      true,
		Floored:     true,
		FlooredAt:   time.Hour,
		PreviewHash: "abc123",
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 60, Database: "hq"},
		},
	})

	if closed != 0 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 0: a refused dry run closes nothing", closed)
	}
	for _, unwanted := range []string{"would auto-closed", "--preview="} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("stdout = %q, want no %q: the live run refuses this threshold", stdout.String(), unwanted)
		}
	}
	if !strings.Contains(stdout.String(), "hq-a") {
		t.Errorf("stdout = %q, want the candidate listed", stdout.String())
	}
}

// TestWriteAutoCloseReportCountsWhatAClosedSweepClosed is the other side: an
// ordinary sweep totals its closes, so the below-floor refusal's zero cannot be
// mistaken for a reporting path that never counts anything.
func TestWriteAutoCloseReportCountsWhatAClosedSweepClosed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database: "hq",
		Closed:   3,
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 90, Database: "hq"},
		},
	})

	if closed != 3 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 3", closed)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: a sweep at the floor has no refusal to report", stderr.String())
	}
	if !strings.Contains(stdout.String(), "auto-closed 3 stale issues") {
		t.Errorf("stdout = %q, want the close count", stdout.String())
	}
}

// TestReportReaperRunOpenWisps drives the hand-run alert's whole decision: what
// counts as a reading of the town, what it is compared against, and what it
// leaves behind for the next run (gt-11kyy).
func TestReportReaperRunOpenWisps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		seed     *reaper.OpenWispSample
		run      reaperRunAlert
		wantWarn bool
		wantSkip bool
		// wantRecorded is the baseline the hand-run series holds afterwards;
		// nil means the run left no series at all.
		wantRecorded *reaper.OpenWispSample
	}{
		{
			name:         "the first whole-town run only records",
			run:          reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 900, Databases: 2}, WholeTown: true},
			wantRecorded: &reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
		},
		{
			name:         "steady state is silent",
			seed:         &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			run:          reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 138, Databases: 2}, WholeTown: true},
			wantRecorded: &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
		},
		{
			name:         "accumulation warns",
			seed:         &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			run:          reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 900, Databases: 2}, WholeTown: true},
			wantWarn:     true,
			wantRecorded: &reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
		},
		{
			name: "a --db run is not a reading of the town",
			run:  reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 900, Databases: 1}},
		},
		{
			name: "a --json run is not a reading of the town",
			run:  reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 900, Databases: 2}},
		},
		{
			name:         "a run that lost a database keeps the series it found",
			seed:         &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			run:          reaperRunAlert{Sample: reaper.OpenWispSample{OpenWisps: 900, Databases: 1}, WholeTown: true, FailedDatabases: 1},
			wantSkip:     true,
			wantRecorded: &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			path := daemon.WispAlertCLIBaselinePath(townRoot)
			if tt.seed != nil {
				held := reaper.OpenWispAlertState{Baseline: *tt.seed, Held: 2}
				if err := daemon.SaveWispAlertState(path, held); err != nil {
					t.Fatalf("seed baseline: %v", err)
				}
			}

			var out bytes.Buffer
			reportReaperRunOpenWisps(&out, townRoot, tt.run)

			if got := strings.Contains(out.String(), "investigate wisp lifecycle"); got != tt.wantWarn {
				t.Fatalf("warned = %v, want %v; output: %q", got, tt.wantWarn, out.String())
			}
			if got := strings.Contains(out.String(), "open-wisp alert skipped"); got != tt.wantSkip {
				t.Fatalf("skip notice = %v, want %v; output: %q", got, tt.wantSkip, out.String())
			}

			state, err := daemon.LoadWispAlertState(path)
			if err != nil {
				t.Fatalf("load baseline: %v", err)
			}
			switch {
			case tt.wantRecorded == nil && state != nil:
				t.Fatalf("the run recorded %+v, want no series", state)
			case tt.wantRecorded != nil && state == nil:
				t.Fatalf("the run recorded nothing, want %+v", *tt.wantRecorded)
			case tt.wantRecorded != nil && state.Baseline != *tt.wantRecorded:
				t.Fatalf("recorded baseline is %+v, want %+v", state.Baseline, *tt.wantRecorded)
			}
		})
	}
}

// The patrol samples once a cycle and a hand run samples whenever an operator
// asks, so a hand run must not move the patrol's series: the gap between the
// two would read as growth (gt-11kyy).
func TestReportReaperRunOpenWispsKeepsItsOwnSeries(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	patrolPath := daemon.WispAlertBaselinePath(townRoot)
	patrol := reaper.OpenWispAlertState{
		Baseline: reaper.OpenWispSample{OpenWisps: 1966, Databases: 6},
		Held:     9,
	}
	if err := daemon.SaveWispAlertState(patrolPath, patrol); err != nil {
		t.Fatalf("seed patrol baseline: %v", err)
	}

	// A count this far above the patrol's baseline would alert against it, so a
	// shared series would show up here as a warning.
	var out bytes.Buffer
	reportReaperRunOpenWisps(&out, townRoot, reaperRunAlert{
		Sample:    reaper.OpenWispSample{OpenWisps: 5900, Databases: 6},
		WholeTown: true,
	})
	if strings.Contains(out.String(), "investigate wisp lifecycle") {
		t.Fatalf("the first hand run warned against the patrol's series: %q", out.String())
	}

	got, err := daemon.LoadWispAlertState(patrolPath)
	if err != nil {
		t.Fatalf("load patrol baseline: %v", err)
	}
	if got == nil || *got != patrol {
		t.Fatalf("the hand run moved the patrol's series to %+v, want %+v", got, patrol)
	}
}

// A town with no root to keep a series in reports nothing rather than guessing
// at one.
func TestReportReaperRunOpenWispsWithoutATownRoot(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reportReaperRunOpenWisps(&out, "", reaperRunAlert{
		Sample:    reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
		WholeTown: true,
	})
	if out.Len() != 0 {
		t.Fatalf("output = %q, want nothing", out.String())
	}
}

// TestReaperHelpNamesTheDaemonPatrolConfig: gt config migrate retires
// mayor/daemon.json, so the reaper's help must not send the operator to it.
func TestReaperHelpNamesTheDaemonPatrolConfig(t *testing.T) {
	t.Parallel()
	for _, cmd := range []*cobra.Command{reaperAutoCloseCmd, reaperRunCmd} {
		text := cmd.Long
		cmd.Flags().VisitAll(func(f *pflag.Flag) { text += "\n" + f.Usage })
		if strings.Contains(text, "daemon.json") {
			t.Errorf("gt reaper %s help names the retired daemon.json:\n%s", cmd.Name(), text)
		}
	}
	if !strings.Contains(reaperAutoCloseCmd.Long, "daemon patrol config") {
		t.Errorf("auto-close help must name the daemon patrol config:\n%s", reaperAutoCloseCmd.Long)
	}
}

// With no town the reaper has no Dolt config to read; it connects as the
// default user instead of loading the config of an empty town root (gt-sktin).
func TestReaperUserInfoForNoTownIsDefaultUser(t *testing.T) {
	t.Parallel()
	if got := reaperUserInfoFor(""); got != doltserver.DefaultUser {
		t.Errorf("reaperUserInfoFor(\"\") = %q, want %q", got, doltserver.DefaultUser)
	}
}

// Outside a town the cwd names no Dolt config: reaperUserInfo connects as the
// default user rather than read ./settings/config.json from whatever directory
// the command ran in, which would send that file's password to the --host the
// reaper is pointed at (gt-1daus).
//
//testpolicy:allow parallel — reaperUserInfo reads the process working directory, which t.Chdir sets for the whole process
func TestReaperUserInfoOutsideTownIgnoresCwdSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"operational":{"dolt":{"user":"admin","password":"secret"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings", "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: reaperUserInfo takes no directory argument
	t.Chdir(dir)
	if got := reaperUserInfo(); got != doltserver.DefaultUser {
		t.Errorf("reaperUserInfo() outside a town = %q, want %q", got, doltserver.DefaultUser)
	}
}

// TestReaperAutoCloseWholeTownPreviewHash: PreviewHash binds the database
// name, so a whole-town dry run produces one hash per database. The live run
// hands each database the hash of its own set from the list, rather than
// passing one database's hash to all of them — which refused every database
// but the one it was made for.
//
// The live-time read is the operator's dry run with only the write defeated:
// same StaleAge, MaxCloses and Force. Rebuilding its options here instead
// would let the two hashes diverge on an option one call got and the other
// did not, so the sweep asserts the options are the live run's. The other half
// — that the real AutoClose hashes the same set whatever DryRun, MaxCloses and
// Force say — is pinned by TestAutoCloseDryRunHashIgnoresTheWriteOptions in
// internal/reaper, where the fake issues table lives.
func TestReaperAutoCloseWholeTownPreviewHash(t *testing.T) {
	t.Parallel()
	live := reaper.AutoCloseOptions{StaleAge: time.Hour, MaxCloses: 3, Force: true}
	sweep := func(_ *beadsql.DB, _ reaper.Writer, dbName string, opts reaper.AutoCloseOptions) (*reaper.AutoCloseResult, error) {
		if !opts.DryRun {
			t.Errorf("%s: preview sweep was not a dry run", dbName)
		}
		if opts.PreviewHash != "" {
			t.Errorf("%s: preview sweep carried preview hash %q", dbName, opts.PreviewHash)
		}
		opts.DryRun, opts.PreviewHash = false, ""
		if opts != live {
			t.Errorf("%s: preview sweep options = %+v, want the live run's %+v", dbName, opts, live)
		}
		return &reaper.AutoCloseResult{Database: dbName, PreviewHash: "hash-" + dbName}, nil
	}

	hashes := parseReaperPreviewHashes(" hash-gastown , hash-beads ,,")
	if want := []string{"hash-gastown", "hash-beads"}; !reflect.DeepEqual(hashes, want) {
		t.Fatalf("parseReaperPreviewHashes = %#v, want %#v", hashes, want)
	}

	for _, dbName := range []string{"gastown", "beads"} {
		got, err := autoClosePreviewHash(sweep, nil, dbName, live, hashes, true)
		if err != nil {
			t.Fatalf("autoClosePreviewHash(%s): %v", dbName, err)
		}
		if want := "hash-" + dbName; got != want {
			t.Errorf("autoClosePreviewHash(%s) = %q, want %q", dbName, got, want)
		}
	}

	// A database the dry run did not show stays unauthorized, so its sweep
	// reports the hash to re-run for rather than tripping over a sibling's.
	got, err := autoClosePreviewHash(sweep, nil, "hq", live, hashes, true)
	if err != nil {
		t.Fatalf("autoClosePreviewHash(hq): %v", err)
	}
	if got != "" {
		t.Errorf("autoClosePreviewHash(hq) = %q, want no hash", got)
	}

	// A single-database run passes the operator's hash through untouched; the
	// sweep compares it and reports a mismatch.
	got, err = autoClosePreviewHash(sweep, nil, "gastown", live, parseReaperPreviewHashes("stale"), false)
	if err != nil || got != "stale" {
		t.Errorf("single-db autoClosePreviewHash = %q, %v; want the operator's hash", got, err)
	}
}

// TestReaperDisarmFromConfig pins the hand-run reaper's auto-close refusal
// (gt-ptslm). The old path read the config through daemon.LoadPatrolConfig,
// which answers nil both for an absent file and for one that does not parse, so
// a settings/config.json that said auto_close=false but could not be read let a
// hand-run sweep write the closes that setting exists to stop. Only an error
// disarms; an absent file keeps today's behavior.
func TestReaperDisarmFromConfig(t *testing.T) {
	t.Parallel()
	source := "/town/settings/config.json (section \"daemon\")"
	off, on := false, true
	withKnob := func(knob *bool) *daemon.DaemonPatrolConfig {
		return &daemon.DaemonPatrolConfig{
			Patrols: &daemon.PatrolsConfig{WispReaper: &daemon.WispReaperConfig{AutoClose: knob}},
		}
	}

	tests := []struct {
		name     string
		cfg      *daemon.DaemonPatrolConfig
		readErr  error
		want     bool
		wantIn   []string
		wantOnce string
	}{
		{
			name: "absent config does not disarm",
		},
		{
			name:   "auto_close=false disarms",
			cfg:    withKnob(&off),
			want:   true,
			wantIn: []string{source, "patrols.wisp_reaper.auto_close=false"},
		},
		{
			name: "auto_close=true does not disarm",
			cfg:  withKnob(&on),
		},
		{
			name: "auto_close unset does not disarm",
			cfg:  withKnob(nil),
		},
		{
			name: "an unparseable config disarms",
			readErr: &config.ParseError{
				Path: "/town/settings/config.json", Offset: 1, Line: 1, Column: 1,
				Err: errors.New("invalid character 'n' looking for beginning of object key string"),
			},
			want:   true,
			wantIn: []string{source, "/town/settings/config.json", "invalid character 'n'"},
		},
		{
			name:    "a read error that names no file still names the source",
			readErr: errors.New("permission denied"),
			want:    true,
			wantIn:  []string{source, "permission denied"},
		},
		{
			name:     "an error that already names the source is not doubled",
			readErr:  errors.New(source + ": read: connection reset"),
			want:     true,
			wantIn:   []string{source, "connection reset"},
			wantOnce: source,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := reaperDisarmFromConfig(tt.cfg, tt.readErr, source)
			if got != tt.want {
				t.Fatalf("reaperDisarmFromConfig = %v (%q), want %v", got, reason, tt.want)
			}
			if !tt.want && reason != "" {
				t.Errorf("reason = %q for a sweep that is not disarmed, want empty", reason)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(reason, want) {
					t.Errorf("reason = %q, want it to contain %q", reason, want)
				}
			}
			if tt.wantOnce != "" {
				if n := strings.Count(reason, tt.wantOnce); n != 1 {
					t.Errorf("reason = %q names %q %d times, want once", reason, tt.wantOnce, n)
				}
			}
		})
	}
}

// TestReaperAutoCloseDisarmedAt is the bug gt-ptslm names, driven through the
// production readers on a temp town: a settings/config.json that does not parse
// reads to daemon.ReadPatrolConfig as absent (the layout follows
// mayor/daemon.json into its host file and defers a broken host to its own
// loader), so a resolution that asks only the patrol config would auto-close on
// defaults. The town with the file absent is not disarmed.
func TestReaperAutoCloseDisarmedAt(t *testing.T) {
	t.Parallel()

	t.Run("unparseable settings/config.json disarms", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := filepath.Join(town, "settings", "config.json")
		writeTownFile(t, path, `{not json`)
		disarmed, reason := reaperAutoCloseDisarmedAt(town)
		if !disarmed {
			t.Fatalf("reaperAutoCloseDisarmedAt = false (%q), want disarmed: a broken config must not read as absent", reason)
		}
		if !strings.Contains(reason, path) {
			t.Errorf("reason = %q, want it to name %s", reason, path)
		}
	})

	t.Run("absent settings/config.json does not disarm", func(t *testing.T) {
		t.Parallel()
		if disarmed, reason := reaperAutoCloseDisarmedAt(t.TempDir()); disarmed {
			t.Fatalf("reaperAutoCloseDisarmedAt = true (%q) for a town with no config, want not disarmed", reason)
		}
	})

	t.Run("auto_close=false in settings/config.json disarms", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownFile(t, filepath.Join(town, "settings", "config.json"),
			`{"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"wisp_reaper":{"enabled":true,"auto_close":false}}}}`)
		disarmed, reason := reaperAutoCloseDisarmedAt(town)
		if !disarmed {
			t.Fatalf("reaperAutoCloseDisarmedAt = false (%q), want disarmed by auto_close=false", reason)
		}
		if !strings.Contains(reason, "auto_close=false") {
			t.Errorf("reason = %q, want the knob it read", reason)
		}
	})
}
