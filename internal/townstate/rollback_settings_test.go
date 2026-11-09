package townstate

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// rollbackRegistry registers two rigs, each with a settings file of its own.
const rollbackRegistry = `{"registry":{"version":1,"rigs":{"gastown":{},"tools":{}}}}`

// gastownRegistry registers the one rig a Mac tree knows about.
const gastownRegistry = `{"registry":{"version":1,"rigs":{"gastown":{}}}}`

// cutover is when the pod took the settings over. --since is that time, so a
// settings file the pod wrote after it is carried and one from before it is not.
var cutover = time.Date(2026, 11, 9, 6, 40, 0, 0, time.UTC)

// setModTime sets the modification time of the file at p.
func setModTime(t *testing.T, p string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// settingsEntry is a regular archive entry whose mtime is at, so the replay
// reads it as newer or older than the Mac file it meets.
func settingsEntry(name, content string, at time.Time) archiveEntry {
	return settingsEntryMode(name, content, at, 0o644)
}

// settingsEntryMode is settingsEntry with the mode the archive entry carries.
func settingsEntryMode(name, content string, at time.Time, mode int64) archiveEntry {
	return archiveEntry{
		hdr:  &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(content)), ModTime: at},
		data: []byte(content),
	}
}

// decodeDoc reads one settings document as a generic object for comparison.
func decodeDoc(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the settings file is not JSON: %v\n%s", err, data)
	}
	return doc
}

// rollbackExport runs the rollback export of volume, --since at the cut-over.
func rollbackExport(t *testing.T, volume string) ([]byte, ExportSummary) {
	t.Helper()
	var stream bytes.Buffer
	sum, err := Export(&stream, volume, &Manifest{Entries: Default().Rollback()}, ExportOptions{Since: cutover, RollbackSettings: true})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return stream.Bytes(), sum
}

// TestRollbackSettingsExportCarriesTownAndRigFiles drives the rollback export
// over a volume whose town and two rigs have settings files, one rig older
// than --since: the newer files are carried and the older one is left out.
func TestRollbackSettingsExportCarriesTownAndRigFiles(t *testing.T) {
	t.Parallel()
	volume := t.TempDir()
	writeStateFile(t, volume, "mayor/town.json", rollbackRegistry, 0o644)
	town := writeStateFile(t, volume, "settings/config.json", `{"name":"town"}`, 0o644)
	gastown := writeStateFile(t, volume, "gastown/settings/config.json", `{"name":"gastown"}`, 0o644)
	tools := writeStateFile(t, volume, "tools/settings/config.json", `{"name":"tools"}`, 0o644)
	setModTime(t, town, cutover.Add(time.Hour))
	setModTime(t, gastown, cutover.Add(time.Hour))
	setModTime(t, tools, cutover.Add(-time.Hour))

	stream, sum := rollbackExport(t, volume)
	names := archiveNames(t, stream)
	for _, want := range []string{"settings/config.json", "gastown/settings/config.json"} {
		if !slices.Contains(names, want) {
			t.Errorf("--rollback dropped %q, which is newer than --since: %v", want, names)
		}
	}
	if slices.Contains(names, "tools/settings/config.json") {
		t.Errorf("--rollback carries tools/settings/config.json, which is older than --since: %v", names)
	}
	if sum.Skipped[SkipOlderThan] != 1 {
		t.Errorf("summary skipped %d files older than --since, want 1 (the tools file)", sum.Skipped[SkipOlderThan])
	}
}

// TestRollbackSettingsEditMadeInThePodSurvives drives the whole rollback: the
// pod edits its settings after the cut-over, the rollback export carries them,
// and the replay onto a Mac tree that holds older copies leaves the edits in place.
func TestRollbackSettingsEditMadeInThePodSurvives(t *testing.T) {
	t.Parallel()
	volume := t.TempDir()
	writeStateFile(t, volume, "mayor/town.json", rollbackRegistry, 0o644)
	const podRig = `{"name":"gastown","edited":"in the pod"}`
	podTown := writeStateFile(t, volume, "settings/config.json", `{"name":"town","edited":"in the pod"}`, 0o644)
	podRigFile := writeStateFile(t, volume, "gastown/settings/config.json", podRig, 0o644)
	setModTime(t, podTown, cutover.Add(time.Hour))
	setModTime(t, podRigFile, cutover.Add(time.Hour))

	mac := t.TempDir()
	writeStateFile(t, mac, "mayor/town.json", rollbackRegistry, 0o644)
	macTown := writeStateFile(t, mac, "settings/config.json", `{"name":"town","daemon":{"supervisor":{"restart":"always"}}}`, 0o644)
	macRig := writeStateFile(t, mac, "gastown/settings/config.json", `{"name":"gastown"}`, 0o644)
	setModTime(t, macTown, cutover.Add(-time.Hour))
	setModTime(t, macRig, cutover.Add(-time.Hour))

	stream, _ := rollbackExport(t, volume)
	if _, err := Import(bytes.NewReader(stream), mac, Default(), ImportOptions{MergeNewer: true}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	gotTown, err := os.ReadFile(macTown)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeDoc(t, gotTown)["edited"]; got != "in the pod" {
		t.Errorf("the replay lost the pod's edit to the town settings: edited = %v, want %q", got, "in the pod")
	}
	gotRig, err := os.ReadFile(macRig)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRig) != podRig {
		t.Errorf("the rig settings did not replay as they are: %q, want %q", gotRig, podRig)
	}
}

// TestRollbackSettingsKeepsTheMacSupervisor replays the pod's town settings onto
// a Mac file in each supervisor state. The Mac's value is set back over the pod's
// copy, a Mac file with none stays without one, and every other key is the pod's.
func TestRollbackSettingsKeepsTheMacSupervisor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// mac is the Mac's settings file; noMacFile leaves it absent.
		mac       string
		noMacFile bool
		archived  string
		// want is the replayed town settings, written out in full.
		want string
	}{
		{
			name:     "the Mac's value is set back over the pod's copy",
			mac:      `{"name":"town","daemon":{"supervisor":{"restart":"always","max":3.0}}}`,
			archived: `{"name":"town","edited":"in the pod","daemon":{"supervisor":"pod copy","patrols":{"refinery":{"enabled":false}}}}`,
			want:     `{"name":"town","edited":"in the pod","daemon":{"supervisor":{"restart":"always","max":3},"patrols":{"refinery":{"enabled":false}}}}`,
		},
		{
			name:     "a Mac file with none stays without one",
			mac:      `{"name":"town","daemon":{"patrols":{}}}`,
			archived: `{"name":"town","daemon":{"supervisor":"pod copy"}}`,
			want:     `{"name":"town","daemon":{}}`,
		},
		{
			name:     "neither side has a supervisor, so nothing changes",
			mac:      `{"name":"town"}`,
			archived: `{"name":"town","edited":"in the pod"}`,
			want:     `{"name":"town","edited":"in the pod"}`,
		},
		{
			name:     "the Mac's value is set back when the archive has no daemon object",
			mac:      `{"name":"town","daemon":{"supervisor":{"restart":"always","max":3.0}}}`,
			archived: `{"name":"town","edited":"in the pod"}`,
			want:     `{"name":"town","edited":"in the pod","daemon":{"supervisor":{"restart":"always","max":3}}}`,
		},
		{
			name:      "no Mac file: the archive's supervisor is not carried",
			noMacFile: true,
			archived:  `{"name":"town","daemon":{"supervisor":"pod copy"}}`,
			want:      `{"name":"town","daemon":{}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mac := t.TempDir()
			writeStateFile(t, mac, "mayor/town.json", rollbackRegistry, 0o644)
			macTown := filepath.Join(mac, "settings", "config.json")
			if !tc.noMacFile {
				writeStateFile(t, mac, "settings/config.json", tc.mac, 0o644)
				setModTime(t, macTown, cutover.Add(-time.Hour))
			}
			archive := writeArchive(t, settingsEntry("settings/config.json", tc.archived, cutover.Add(time.Hour)))
			if _, err := Import(bytes.NewReader(archive), mac, Default(), ImportOptions{MergeNewer: true}); err != nil {
				t.Fatalf("Import: %v", err)
			}

			got := decodeDoc(t, mustRead(t, macTown))
			want := decodeDoc(t, []byte(tc.want))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the replayed town settings = %v, want %v", got, want)
			}
		})
	}
}

// TestRollbackSettingsKeepsTheArchivedMode replays a town and a rig settings file
// whose archive entries are private (0600) onto Mac files at 0644. Each replayed
// file takes the archived mode, not the 0644 a shared row is masked to.
func TestRollbackSettingsKeepsTheArchivedMode(t *testing.T) {
	t.Parallel()
	mac := t.TempDir()
	writeStateFile(t, mac, "mayor/town.json", rollbackRegistry, 0o644)
	macTown := writeStateFile(t, mac, "settings/config.json", `{"name":"town"}`, 0o644)
	macRig := writeStateFile(t, mac, "gastown/settings/config.json", `{"name":"gastown"}`, 0o644)
	setModTime(t, macTown, cutover.Add(-time.Hour))
	setModTime(t, macRig, cutover.Add(-time.Hour))
	archive := writeArchive(t,
		settingsEntryMode("settings/config.json", `{"name":"town"}`, cutover.Add(time.Hour), 0o600),
		settingsEntryMode("gastown/settings/config.json", `{"name":"gastown"}`, cutover.Add(time.Hour), 0o600),
	)
	if _, err := Import(bytes.NewReader(archive), mac, Default(), ImportOptions{MergeNewer: true}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, p := range []string{macTown, macRig} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s replayed at mode %o, want 0600 as archived", p, got)
		}
	}
}

// TestRollbackSettingsRefusesWhatItCannotRead holds each refusal the replay
// makes to its input. Every refusal writes nothing: the Mac file it would have
// replaced is still there, unchanged.
func TestRollbackSettingsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	oversize := strings.Repeat("a", maxSettingsBytes+1)
	for _, tc := range []struct {
		name string
		// macFile is the Mac's town settings file, absent when empty.
		macFile string
		// macRegistry is the Mac's registry; the rigs it names are the ones a replay accepts.
		macRegistry string
		archive     []archiveEntry
		// mergeNewer is the mode the import runs in.
		mergeNewer bool
		want       string
	}{
		{
			name:        "an archived town settings file that is not JSON",
			macFile:     `{"name":"town"}`,
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", "{not json", cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "not valid JSON",
		},
		{
			name:        "an archived town settings file that is null",
			macFile:     `{"name":"town"}`,
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", "null", cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "not one JSON object",
		},
		{
			name:        "a Mac settings file that is not JSON",
			macFile:     "{not json",
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", `{"name":"town"}`, cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "the Mac's settings file",
		},
		{
			name:        "a Mac settings file that is null",
			macFile:     "null",
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", `{"name":"town"}`, cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "the Mac's settings file is not one JSON object",
		},
		{
			name:        "an archived daemon that is not an object",
			macFile:     `{"name":"town","daemon":{"supervisor":{}}}`,
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", `{"daemon":[]}`, cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "daemon is not an object",
		},
		{
			name:        "an archived settings file over the bound",
			macFile:     `{"name":"town"}`,
			macRegistry: rollbackRegistry,
			archive:     []archiveEntry{settingsEntry("settings/config.json", oversize, cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "larger than",
		},
		{
			name:        "a rig settings file for a rig the destination does not register",
			macRegistry: gastownRegistry,
			archive:     []archiveEntry{settingsEntry("tools/settings/config.json", `{"name":"tools"}`, cutover.Add(time.Hour))},
			mergeNewer:  true,
			want:        "tools/settings/config.json",
		},
		{
			name:        "a settings file on an import that is not a replay",
			macRegistry: gastownRegistry,
			archive: []archiveEntry{
				settingsEntry("mayor/town.json", gastownRegistry, cutover.Add(time.Hour)),
				settingsEntry("gastown/settings/config.json", `{"name":"gastown"}`, cutover.Add(time.Hour)),
			},
			mergeNewer: false,
			want:       "gastown/settings/config.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mac := t.TempDir()
			writeStateFile(t, mac, "mayor/town.json", tc.macRegistry, 0o644)
			macTown := filepath.Join(mac, "settings", "config.json")
			if tc.macFile != "" {
				writeStateFile(t, mac, "settings/config.json", tc.macFile, 0o644)
				setModTime(t, macTown, cutover.Add(-time.Hour))
			}
			// A plain import needs an empty root; the replay runs onto the Mac tree.
			dest := mac
			if !tc.mergeNewer {
				dest = t.TempDir()
			}
			_, err := Import(bytes.NewReader(writeArchive(t, tc.archive...)), dest, Default(), ImportOptions{MergeNewer: tc.mergeNewer})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Import = %v, want a refusal holding %q", err, tc.want)
			}
			if tc.macFile != "" && string(mustRead(t, macTown)) != tc.macFile {
				t.Errorf("the refused replay changed the Mac's settings file to %q", mustRead(t, macTown))
			}
		})
	}
}

// TestExportWithoutRollbackCarriesNoSettings holds the normal export's output
// to the same names whether or not the settings files exist: only --rollback
// adds them.
func TestExportWithoutRollbackCarriesNoSettings(t *testing.T) {
	t.Parallel()
	volume := t.TempDir()
	writeStateFile(t, volume, "mayor/town.json", rollbackRegistry, 0o644)
	writeStateFile(t, volume, "daemon/state.json", "{}\n", 0o644)

	var before bytes.Buffer
	if _, err := Export(&before, volume, Default(), ExportOptions{}); err != nil {
		t.Fatalf("Export before the settings files exist: %v", err)
	}
	for _, rel := range []string{"settings/config.json", "gastown/settings/config.json"} {
		setModTime(t, writeStateFile(t, volume, rel, `{"name":"x"}`, 0o644), cutover.Add(time.Hour))
	}
	var after bytes.Buffer
	if _, err := Export(&after, volume, Default(), ExportOptions{}); err != nil {
		t.Fatalf("Export with the settings files: %v", err)
	}
	if got, want := archiveNames(t, after.Bytes()), archiveNames(t, before.Bytes()); !slices.Equal(got, want) {
		t.Errorf("the normal export changed when settings files appeared:\n got %v\nwant %v", got, want)
	}
	var rollback bytes.Buffer
	if _, err := Export(&rollback, volume, &Manifest{Entries: Default().Rollback()}, ExportOptions{Since: cutover}); err != nil {
		t.Fatalf("rollback export without RollbackSettings: %v", err)
	}
	if names := archiveNames(t, rollback.Bytes()); slices.Contains(names, "settings/config.json") {
		t.Errorf("an export without RollbackSettings carries the settings file: %v", names)
	}
}

// failingRead is a reader whose every read fails.
type failingRead struct{}

func (failingRead) Read([]byte) (int, error) { return 0, errors.New("read refused") }

// failingClose is a reader whose close fails after its content is read.
type failingClose struct{ io.Reader }

func (failingClose) Close() error { return errors.New("close refused") }

// TestRollbackSettingsRefusesWhatItCannotReadOnTheMac drives the replay's reads of
// the Mac's settings file and of the archive entry through their failure paths.
// Each refusal leaves the Mac's file as it was.
func TestRollbackSettingsRefusesWhatItCannotReadOnTheMac(t *testing.T) {
	t.Parallel()
	const macContent = `{"name":"town"}`
	goodArchive := func(t *testing.T) []byte {
		return writeArchive(t, settingsEntry("settings/config.json", `{"name":"pod"}`, cutover.Add(time.Hour)))
	}
	for _, tc := range []struct {
		name       string
		macContent string
		ops        func(t *testing.T) townOps
		archive    func(t *testing.T) []byte
		want       string
	}{
		{
			name:       "the Mac's settings file cannot be opened",
			macContent: macContent,
			ops: func(*testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.open = func(string) (io.ReadCloser, error) { return nil, errors.New("open refused") }
				})
			},
			archive: goodArchive,
			want:    "reading the Mac's settings file",
		},
		{
			name:       "the Mac's settings file cannot be read",
			macContent: macContent,
			ops: func(*testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.open = func(string) (io.ReadCloser, error) { return io.NopCloser(failingRead{}), nil }
				})
			},
			archive: goodArchive,
			want:    "reading the Mac's settings file",
		},
		{
			name:       "the Mac's settings file cannot be closed",
			macContent: macContent,
			ops: func(*testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.open = func(string) (io.ReadCloser, error) {
						return failingClose{strings.NewReader(macContent)}, nil
					}
				})
			},
			archive: goodArchive,
			want:    "closing the Mac's settings file",
		},
		{
			name:       "the Mac's daemon is not an object",
			macContent: `{"daemon":[]}`,
			archive:    goodArchive,
			want:       "the Mac's settings file: settings: daemon is not an object",
		},
		{
			name:       "an archive entry cut short",
			macContent: macContent,
			archive: func(t *testing.T) []byte {
				// Keep the header and part of the first data block: the entry
				// promises 100 bytes and the stream ends after 40.
				full := writeArchive(t, settingsEntry("settings/config.json", strings.Repeat("x", 100), cutover.Add(time.Hour)))
				return full[:512+40]
			},
			want: `reading "settings/config.json" from the archive`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mac := t.TempDir()
			writeStateFile(t, mac, "mayor/town.json", rollbackRegistry, 0o644)
			macTown := filepath.Join(mac, "settings", "config.json")
			writeStateFile(t, mac, "settings/config.json", tc.macContent, 0o644)
			setModTime(t, macTown, cutover.Add(-time.Hour))
			opts := ImportOptions{MergeNewer: true}
			if tc.ops != nil {
				opts.ops = tc.ops(t)
			}
			_, err := Import(bytes.NewReader(tc.archive(t)), mac, Default(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Import = %v, want a refusal holding %q", err, tc.want)
			}
			if got := string(mustRead(t, macTown)); got != tc.macContent {
				t.Errorf("the refused replay changed the Mac's settings file to %q", got)
			}
		})
	}
}

// mustRead returns the file at p, failing the test when it cannot be read.
func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
