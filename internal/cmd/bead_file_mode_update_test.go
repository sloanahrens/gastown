package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/outbox"
)

// A file-mode update writes result.json rather than the bead (gt-pm55bt). These
// tests drive every step from the verb to the file, on seat roots the shared
// helper in bead_file_mode_test.go lays out.

// outboxResult returns the seat's result.json as the host reads it: decoded by
// outbox.Validate against the seat's notes, or nil when the seat wrote none.
func outboxResult(t *testing.T, root, beadID string) *outbox.Result {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, outbox.Dir, outbox.ResultFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	r, err := outbox.Validate(raw, outboxNotes(t, root), beadID)
	if err != nil {
		t.Fatalf("the result.json the seat wrote does not validate: %v\n%s", err, raw)
	}
	return &r
}

// writeOutboxResult writes previous as the seat's result.json, so a test starts
// from an outbox the seat left behind.
func writeOutboxResult(t *testing.T, root, previous string) string {
	t.Helper()
	dir := filepath.Join(root, outbox.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, outbox.ResultFile)
	if err := os.WriteFile(path, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An update in file mode reaches the seat's outbox on every call and never the
// bead store, which file mode forbids (gt-pm55bt).
func TestNoBeadAccessUpdateAppendsToTheOutbox(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	v, store, out := seatVerbs(t, root)
	if err := v.update("gt-own1", beads.UpdateOptions{Acceptance: strPtr("- [ ] the refusal names the verb")}); err != nil {
		t.Fatal(err)
	}
	if err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("DISPROVEN: the retry resets")}); err != nil {
		t.Fatal(err)
	}
	// The host decodes what the seat wrote with outbox.Validate; the file the
	// verb left must already satisfy it, and the second call must keep what the
	// first one wrote.
	r := outboxResult(t, root, "gt-own1")
	if r == nil {
		t.Fatal("an update wrote no result.json")
	}
	if r.Title == nil || *r.Title != "DISPROVEN: the retry resets" {
		t.Errorf("title = %v, want the title the second call set", r.Title)
	}
	if r.Acceptance == nil || *r.Acceptance != "- [ ] the refusal names the verb" {
		t.Errorf("acceptance = %v, want the block the first call set", r.Acceptance)
	}
	// A seat that has not ended has reported no finished run.
	if r.Status != outbox.StatusDeferred {
		t.Errorf("status = %q, want the deferred result an unfinished run writes", r.Status)
	}
	if notes := outboxNotes(t, root); notes != nil {
		t.Errorf("an update wrote notes %q", notes)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written in file mode: %v", store.calls)
	}
	if got, want := out.String(), "✓ Updated gt-own1 in its outbox\n✓ Updated gt-own1 in its outbox\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestNoBeadAccessUpdateRefusesAnotherBead(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"gt-other", "gt-own", "gt-own1 "} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			v, store, _ := seatVerbs(t, root)
			err := v.update(id, beads.UpdateOptions{Title: strPtr("a new title")})
			if err == nil {
				t.Fatalf("updating %q succeeded in a seat that may write gt-own1", id)
			}
			if !strings.Contains(err.Error(), "only its own bead") || !strings.Contains(err.Error(), "gt-own1") {
				t.Errorf("error %q does not name the seat's own bead", err)
			}
			if !strings.Contains(err.Error(), "updating") {
				t.Errorf("error %q reads as another verb", err)
			}
			if r := outboxResult(t, root, "gt-own1"); r != nil {
				t.Errorf("the outbox holds %+v after a refused update", r)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused update: %v", store.calls)
			}
		})
	}
}

// The outbox carries a title and an acceptance block. A field it has no room
// for must refuse rather than report a change the host never makes (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesWhatTheOutboxCannotCarry(t *testing.T) {
	t.Parallel()
	priority := 1
	// A metadata flag alone stops earlier, at update's nothing-to-update check,
	// which predates the outbox and does not read the metadata flags. Paired
	// with a title it reaches the outbox's own refusal, which is the branch
	// that must not let the pair by unreported.
	cases := map[string]beads.UpdateOptions{
		"--priority":       {Priority: &priority},
		"--add-label":      {AddLabels: []string{"needs-review"}},
		"--remove-label":   {RemoveLabels: []string{"needs-review"}},
		"--set-metadata":   {Title: strPtr("a new title"), SetMetadata: []string{"key=value"}},
		"--unset-metadata": {Title: strPtr("a new title"), UnsetMetadata: []string{"key"}},
	}
	for flag, opts := range cases {
		flag, opts := flag, opts
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			v, store, _ := seatVerbs(t, root)
			err := v.update("gt-own1", opts)
			if err == nil {
				t.Fatalf("an update setting %s succeeded in a file-mode seat", flag)
			}
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("error %q does not name %s", err, flag)
			}
			if r := outboxResult(t, root, "gt-own1"); r != nil {
				t.Errorf("the outbox holds %+v after a refused update", r)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused update: %v", store.calls)
			}
		})
	}
}

// An outbox the host would refuse is not an outbox to write over: the update
// reports it and leaves the file as it stands (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesAnOutboxThatDoesNotValidate(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	const previous = `{"bead_id":"gt-other","status":"done"}`
	resultPath := writeOutboxResult(t, root, previous)
	v, store, _ := seatVerbs(t, root)
	err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("a new title")})
	if err == nil {
		t.Fatal("an update was accepted over a result.json the host would refuse")
	}
	if !strings.Contains(err.Error(), "does not validate") {
		t.Errorf("error %q does not say the outbox does not validate", err)
	}
	if got, readErr := os.ReadFile(resultPath); readErr != nil || string(got) != previous {
		t.Errorf("result.json = %q after a refused update (read error %v), want %q", got, readErr, previous)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written over a refused outbox: %v", store.calls)
	}
}

// Ticked boxes are indexes into the acceptance block the seat last had, so
// replacing that block would leave them naming other lines (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesAnAcceptanceOverTickedBoxes(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	const previous = `{"bead_id":"gt-own1","status":"deferred","ticked_boxes":[0,2]}`
	resultPath := writeOutboxResult(t, root, previous)
	v, store, _ := seatVerbs(t, root)
	err := v.update("gt-own1", beads.UpdateOptions{Acceptance: strPtr("- [ ] a different block")})
	if err == nil {
		t.Fatal("an acceptance replaced a block the outbox reports ticks into")
	}
	if !strings.Contains(err.Error(), "ticked boxes") {
		t.Errorf("error %q does not name the ticked boxes", err)
	}
	if got, readErr := os.ReadFile(resultPath); readErr != nil || string(got) != previous {
		t.Errorf("result.json = %q after a refused update (read error %v), want %q", got, readErr, previous)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written for a refused update: %v", store.calls)
	}
}

// An outbox file that cannot be read is not an empty one: the update composes
// nothing over it and says which file it could not read (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesAnUnreadableOutbox(t *testing.T) {
	t.Parallel()
	const previous = `{"bead_id":"gt-own1","status":"deferred"}`
	cases := map[string]func(t *testing.T, root string){
		"notes": func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, outbox.Dir, outbox.NotesFile), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"result": func(t *testing.T, root string) {
			writeOutboxResult(t, root, previous)
			if err := os.Remove(filepath.Join(root, outbox.Dir, outbox.ResultFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, outbox.Dir, outbox.ResultFile), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, arrange := range cases {
		name, arrange := name, arrange
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			arrange(t, root)
			v, store, _ := seatVerbs(t, root)
			err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("a new title")})
			if err == nil {
				t.Fatal("an update was accepted over a file that cannot be read")
			}
			if !strings.Contains(err.Error(), "cannot be read") || !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the %s file it could not read", err, name)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused update: %v", store.calls)
			}
		})
	}
}

// The update validates the bytes it is about to write, so a block over the
// outbox's cap is refused while the seat can still report it rather than
// written for the host to drop (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesAnAcceptanceOverTheCap(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	v, store, _ := seatVerbs(t, root)
	err := v.update("gt-own1", beads.UpdateOptions{Acceptance: strPtr(strings.Repeat("x", 64<<10))})
	if err == nil {
		t.Fatal("an acceptance over the outbox's cap was accepted")
	}
	if !strings.Contains(err.Error(), "more than the") {
		t.Errorf("error %q does not name the cap the outbox holds", err)
	}
	if r := outboxResult(t, root, "gt-own1"); r != nil {
		t.Errorf("the outbox holds %+v after a refused update", r)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written for a refused update: %v", store.calls)
	}
}

// An update sets the fields it names and keeps every other field the outbox
// already carries, so a later call never wipes an earlier seat's record
// (gt-pm55bt).
func TestNoBeadAccessUpdateKeepsWhatTheOutboxAlreadyCarries(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	const previous = `{"bead_id":"gt-own1","status":"deferred","ticked_boxes":[0,2],` +
		`"findings":["the retry never resets its counter"],` +
		`"escalation":{"text":"HELP: the third criterion needs a decision"}}`
	writeOutboxResult(t, root, previous)
	v, store, _ := seatVerbs(t, root)
	if err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("DISPROVEN: the retry resets")}); err != nil {
		t.Fatal(err)
	}
	r := outboxResult(t, root, "gt-own1")
	if r == nil {
		t.Fatal("the update wrote no result.json")
	}
	if r.Title == nil || *r.Title != "DISPROVEN: the retry resets" {
		t.Errorf("title = %v, want the title the call set", r.Title)
	}
	if want := []int{0, 2}; !reflect.DeepEqual(r.Ticked, want) {
		t.Errorf("ticked boxes = %v, want the %v the outbox already carried", r.Ticked, want)
	}
	if want := []string{"the retry never resets its counter"}; !reflect.DeepEqual(r.Findings, want) {
		t.Errorf("findings = %v, want the %v the outbox already carried", r.Findings, want)
	}
	if r.Escalation == nil || r.Escalation.Text != "HELP: the third criterion needs a decision" {
		t.Errorf("escalation = %+v, want the request the outbox already carried", r.Escalation)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written in file mode: %v", store.calls)
	}
}

// An update whose write fails reports the failure and leaves the result.json
// the seat already had, so a killed write never loses it (gt-pm55bt).
func TestNoBeadAccessUpdateFailedWriteKeepsThePreviousResult(t *testing.T) {
	t.Parallel()
	boom := errors.New("the seat was killed mid-write")
	const previous = `{"bead_id":"gt-own1","status":"deferred","title":"the title before"}`
	for name, fsys := range map[string]failingOutboxFS{
		"write":  {writeErr: boom},
		"rename": {renameErr: boom},
	} {
		name, fsys := name, fsys
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			resultPath := writeOutboxResult(t, root, previous)
			v, store, _ := seatVerbs(t, root)
			v.outboxFS = fsys
			err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("a new title")})
			if err == nil {
				t.Fatal("an update whose write failed reported success")
			}
			if !strings.Contains(err.Error(), "updating") || !strings.Contains(err.Error(), outbox.ResultFile) {
				t.Errorf("error %q does not read as a failed update of %s", err, outbox.ResultFile)
			}
			if got, readErr := os.ReadFile(resultPath); readErr != nil || string(got) != previous {
				t.Errorf("result.json = %q after a failed write (read error %v), want the previous %q", got, readErr, previous)
			}
			// The host's copy refuses a name in the outbox directory that is
			// not one of its two files, so a killed write must leave none there.
			entries, readErr := os.ReadDir(filepath.Join(root, outbox.Dir))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != outbox.ResultFile {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("the outbox directory holds %v after a failed write, want only %s", names, outbox.ResultFile)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written after a failed outbox write: %v", store.calls)
			}
		})
	}
}

// A seat whose own bead cannot be read cannot tell whether the update names it,
// so the update refuses rather than falling through to a bead write
// (gt-pm55bt).
func TestNoBeadAccessUpdateRefusesAnUnreadableSeat(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	if err := os.Remove(filepath.Join(root, seatTaskFilePath)); err != nil {
		t.Fatal(err)
	}
	v, store, _ := seatVerbs(t, root)
	err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("a new title")})
	if err == nil {
		t.Fatal("an update was accepted while TASK.md could not be read")
	}
	if !strings.Contains(err.Error(), "TASK.md") {
		t.Errorf("error %q does not say TASK.md cannot be read", err)
	}
	if r := outboxResult(t, root, "gt-own1"); r != nil {
		t.Errorf("the outbox holds %+v after a refused update", r)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written when TASK.md could not be read: %v", store.calls)
	}
}

func TestNoBeadAccessUpdateLeavesDoltModeAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // a seat root with no file-mode marker
	v, store, _ := seatVerbs(t, root)
	if err := v.update("gt-own1", beads.UpdateOptions{Title: strPtr("a new title")}); err != nil {
		t.Fatal(err)
	}
	const want = "Update gt-own1"
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Errorf("dolt-mode update calls = %v, want %q", store.calls, want)
	}
	if _, err := os.Stat(filepath.Join(root, outbox.Dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dolt-mode update wrote an outbox: %v", err)
	}
}
