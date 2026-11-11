package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// guardedStore runs bd's --if-acceptance guard over the fake, the part of bd
// the tick's retry depends on. Before each guarded write, amend (when set)
// changes the field, as an owner's write landing between the tick's read and
// its write would. The write then goes through only while the field still
// hashes to the guard; otherwise it returns beads.ErrGuardNotHeld and writes
// nothing, as bd does on exit 13. reject, when set, is returned instead of
// any write. guards records the guard of each guarded write, in order.
type guardedStore struct {
	beads.Client
	t      *testing.T
	amend  func(write int)
	reject error
	guards []string
}

func (s *guardedStore) Update(id string, opts beads.UpdateOptions) error {
	if opts.Acceptance != nil && opts.IfAcceptance == "" {
		s.t.Errorf("criteria of %s written without a guard", id)
	}
	if opts.IfAcceptance == "" {
		return s.Client.Update(id, opts)
	}
	s.guards = append(s.guards, opts.IfAcceptance)
	if len(s.guards) > tickAttempts {
		s.t.Fatalf("tick made %d guarded writes, past its bound of %d", len(s.guards), tickAttempts)
	}
	if s.reject != nil {
		return s.reject
	}
	if s.amend != nil {
		s.amend(len(s.guards))
	}
	current, err := s.Client.Show(id)
	if err != nil {
		return err
	}
	if shaHex(current.AcceptanceCriteria) != opts.IfAcceptance {
		return fmt.Errorf("bd update %s: guard no longer held: %w", id, beads.ErrGuardNotHeld)
	}
	opts.IfAcceptance = ""
	return s.Client.Update(id, opts)
}

// shaHex is the guard value for a criteria block: the hex sha256 of its text.
func shaHex(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}

// setAcceptance rewrites the field through the fake, as another writer would.
func setAcceptance(t *testing.T, fake *beadsfake.Fake, id, block string) {
	t.Helper()
	if err := fake.Update(id, beads.UpdateOptions{Acceptance: &block}); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptanceTickSendsTheGuardOfTheBlockItRead: the write carries
// --if-acceptance as the sha256 of the exact block the tick read.
func TestAcceptanceTickSendsTheGuardOfTheBlockItRead(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	block := "- [ ] first\n- [ ] second\r\n"
	id := seedAcceptance(t, fake, block)
	store := &guardedStore{Client: fake, t: t}
	v := &beadVerbs{client: store, actor: "me", out: &bytes.Buffer{}}

	if err := v.tick(id, "second"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(store.guards) != 1 || store.guards[0] != shaHex(block) {
		t.Errorf("guards = %q, want one guard %s", store.guards, shaHex(block))
	}
}

// TestAcceptanceTickRetriesAGuardMissAndKeepsTheAmendment: the owner amends the
// field after the tick reads it, so bd refuses the first write. The tick
// re-reads, re-ticks the same box by its text in the amended block, and writes
// once more; the owner's boxes survive.
func TestAcceptanceTickRetriesAGuardMissAndKeepsTheAmendment(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	original := "- [ ] first\n- [ ] second"
	id := seedAcceptance(t, fake, original)
	amended := "- [ ] added before\n- [ ] first\n- [ ] second\n- [ ] added after"
	store := &guardedStore{Client: fake, t: t, amend: func(write int) {
		if write == 1 {
			setAcceptance(t, fake, id, amended)
		}
	}}
	var out bytes.Buffer
	v := &beadVerbs{client: store, actor: "me", out: &out}

	if err := v.tick(id, "second"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(store.guards) != 2 || store.guards[0] != shaHex(original) || store.guards[1] != shaHex(amended) {
		t.Errorf("guards = %q, want the first read then the amended block", store.guards)
	}
	want := "- [ ] added before\n- [ ] first\n- [x] second\n- [ ] added after"
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != want {
		t.Errorf("after the retried tick, criteria = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), `Ticked "second"`) {
		t.Errorf("output %q does not confirm the tick", out.String())
	}
}

// TestAcceptanceTickReportsAConflictPastTheBound: the owner amends the field
// before every write, so each guarded write is refused. After tickAttempts
// writes the tick reports the conflict, prints nothing as ticked, and leaves
// the field as the owner last wrote it.
func TestAcceptanceTickReportsAConflictPastTheBound(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first\n- [ ] second")
	store := &guardedStore{Client: fake, t: t, amend: func(write int) {
		setAcceptance(t, fake, id, fmt.Sprintf("- [ ] first\n- [ ] second\n- [ ] amendment %d", write))
	}}
	var out bytes.Buffer
	v := &beadVerbs{client: store, actor: "me", out: &out}

	err := v.tick(id, "second")
	if err == nil || !strings.Contains(err.Error(), "changed between every read and write") {
		t.Fatalf("tick error = %v, want the conflict", err)
	}
	if len(store.guards) != tickAttempts {
		t.Errorf("guarded writes = %d, want %d", len(store.guards), tickAttempts)
	}
	if out.Len() != 0 {
		t.Errorf("conflicted tick printed %q", out.String())
	}
	want := fmt.Sprintf("- [ ] first\n- [ ] second\n- [ ] amendment %d", tickAttempts)
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != want {
		t.Errorf("criteria = %q, want the owner's last write %q", got, want)
	}
}

// TestAcceptanceTickNamesTheFlagWhenBdRejectsIt: a bd without --if-acceptance
// fails the guarded write. The tick returns that failure naming the flag, does
// not retry it, and never writes the field without the guard.
func TestAcceptanceTickNamesTheFlagWhenBdRejectsIt(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first")
	bdErr := errors.New("exit status 1: unknown flag: --if-acceptance")
	store := &guardedStore{Client: fake, t: t, reject: bdErr}
	var out bytes.Buffer
	v := &beadVerbs{client: store, actor: "me", out: &out}

	err := v.tick(id, "first")
	if !errors.Is(err, bdErr) || !strings.Contains(err.Error(), "--if-acceptance") {
		t.Fatalf("tick error = %v, want bd's refusal naming --if-acceptance", err)
	}
	if len(store.guards) != 1 {
		t.Errorf("guarded writes = %d, want 1: a non-guard failure is not retried", len(store.guards))
	}
	if out.Len() != 0 {
		t.Errorf("failed tick printed %q", out.String())
	}
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != "- [ ] first" {
		t.Errorf("criteria = %q, want unchanged", got)
	}
}

// tickFailingStore fails Show or Update with the error it is given, and passes
// every other call to the fake.
type tickFailingStore struct {
	beads.Client
	showErr, updateErr error
}

func (s tickFailingStore) Show(id string) (*beads.Issue, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	return s.Client.Show(id)
}

func (s tickFailingStore) Update(id string, opts beads.UpdateOptions) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Client.Update(id, opts)
}

// TestAcceptanceTickReturnsAStoreFailure: a read or write the store refuses
// is returned to the caller, and nothing is printed as ticked.
func TestAcceptanceTickReturnsAStoreFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("store unreachable")
	for name, store := range map[string]func(beads.Client) tickFailingStore{
		"show":   func(c beads.Client) tickFailingStore { return tickFailingStore{Client: c, showErr: boom} },
		"update": func(c beads.Client) tickFailingStore { return tickFailingStore{Client: c, updateErr: boom} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := beadsfake.New()
			id := seedAcceptance(t, fake, "- [ ] first")
			var out bytes.Buffer
			v := &beadVerbs{client: store(fake), actor: "me", out: &out}
			if err := v.tick(id, "first"); !errors.Is(err, boom) {
				t.Errorf("tick error = %v, want the store failure", err)
			}
			if out.Len() != 0 {
				t.Errorf("failed tick printed %q", out.String())
			}
			if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != "- [ ] first" {
				t.Errorf("criteria = %q, want unchanged", got)
			}
		})
	}
}

// TestAcceptanceTickUpdateFailureDoesNotNameTheFlag: a failed write that is not
// a guard miss is reported as the write that failed. It does not claim bd lacks
// --if-acceptance, which is only what bd's own refusal says.
func TestAcceptanceTickUpdateFailureDoesNotNameTheFlag(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first")
	boom := errors.New("store unreachable")
	v := &beadVerbs{client: tickFailingStore{Client: fake, updateErr: boom}, actor: "me", out: &bytes.Buffer{}}

	err := v.tick(id, "first")
	if !errors.Is(err, boom) {
		t.Fatalf("tick error = %v, want the store failure", err)
	}
	if strings.Contains(err.Error(), "--if-acceptance") {
		t.Errorf("tick error %q names --if-acceptance for a failure bd did not report as the flag", err)
	}
}

// TestAcceptanceTickHelpStatesTheWriteBound: the help names the write bound the
// tick enforces, so the two cannot drift apart.
func TestAcceptanceTickHelpStatesTheWriteBound(t *testing.T) {
	t.Parallel()
	for _, c := range beadCmd.Commands() {
		if c.Name() != "tick" {
			continue
		}
		want := fmt.Sprintf("up to %d writes", tickAttempts)
		if !strings.Contains(c.Long, want) {
			t.Errorf("tick help lacks %q:\n%s", want, c.Long)
		}
		return
	}
	t.Fatal("gt bead has no tick verb")
}

// seedAcceptance creates a bead whose acceptance field is criteria.
func seedAcceptance(t *testing.T, fake *beadsfake.Fake, criteria string) string {
	t.Helper()
	return mustCreateBead(t, fake, beads.CreateOptions{Title: "work", Acceptance: criteria}).ID
}

// TestAcceptanceTickReadsTheFieldAtTickTime: a worker reads the criteria, then
// the owner adds a box before and after the worker's box. The tick must flip
// the worker's box by its text and keep both new boxes; a tick that wrote back
// the earlier read, or flipped by position, would erase or misplace them.
func TestAcceptanceTickReadsTheFieldAtTickTime(t *testing.T) {
	t.Parallel()
	v, fake, out := testBeadVerbs("gastown/polecats/dementus")
	id := seedAcceptance(t, fake, "- [ ] first\n- [ ] second")
	read := mustShowBead(t, fake, id).AcceptanceCriteria

	amended := "- [ ] added before\n- [ ] first\n- [ ] second\n- [ ] added after"
	if err := fake.Update(id, beads.UpdateOptions{Acceptance: &amended}); err != nil {
		t.Fatal(err)
	}
	if read == amended {
		t.Fatal("the owner's amendment did not change the field; the test proves nothing")
	}

	if err := v.tick(id, "second"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := "- [ ] added before\n- [ ] first\n- [x] second\n- [ ] added after"
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != want {
		t.Errorf("after tick, criteria = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), `Ticked "second"`) {
		t.Errorf("output %q does not confirm the tick", out.String())
	}
}

// TestAcceptanceTickRefusesAMissingBox: the owner rewrote the box the worker
// names. The tick refuses, prints the criteria as they now stand, and never
// writes the field.
func TestAcceptanceTickRefusesAMissingBox(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first\n- [ ] second")
	renamed := "- [ ] first\n- [ ] second, reworded by the owner"
	if err := fake.Update(id, beads.UpdateOptions{Acceptance: &renamed}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	v := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &out}

	err := v.tick(id, "second")
	if err == nil {
		t.Fatal("a tick of a box no longer in the field succeeded")
	}
	for _, want := range []string{"not ticking " + id, `"second"`, "- [ ] first", "- [ ] second, reworded by the owner"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("refused tick printed %q", out.String())
	}
}

// TestAcceptanceTickRefusalNamesAnEmptyField: a bead whose field is empty has
// no box to tick. The refusal says so and names the write that sets the field,
// and the store is not written.
func TestAcceptanceTickRefusalNamesAnEmptyField(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "")
	v := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &bytes.Buffer{}}
	err := v.tick(id, "anything")
	for _, want := range []string{"acceptance field is empty", "gt bead update " + id + " --acceptance="} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("tick of an empty field: error %v, want it to contain %q", err, want)
		}
	}
}

// TestAcceptanceTickPreservesOtherLines: with text that is hard on a rewrite
// (backticks, a shell substitution, quotes, a fenced example, a nested and a
// numbered item, trailing spaces, no final newline), the tick changes the one
// "[ ]" it names and no other byte.
func TestAcceptanceTickPreservesOtherLines(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("me")
	block := "Intro with trailing space  \n" +
		"- [ ] `make lint` && $(echo ok) \"quoted\" 'single'   \n" +
		"```\n- [ ] an example inside a fence, not a criterion\n```\n" +
		"  - [x] nested, already done\n" +
		"1. [ ] numbered: \\ backslash\t tab\n" +
		"- [ ] last one with no newline"
	id := seedAcceptance(t, fake, block)

	if err := v.tick(id, "`make lint` && $(echo ok) \"quoted\" 'single'"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := strings.Replace(block, "- [ ] `make lint`", "- [x] `make lint`", 1)
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != want {
		t.Errorf("criteria = %q, want %q", got, want)
	}

	// The fenced example is not a box, so naming its text refuses.
	if err := v.tick(id, "an example inside a fence, not a criterion"); err == nil {
		t.Error("a tick named a box inside a code fence")
	}
	// A numbered item and the last line are boxes like any other.
	for _, box := range []string{"numbered: \\ backslash\t tab", "- [ ] last one with no newline"} {
		if err := v.tick(id, box); err != nil {
			t.Errorf("tick %q: %v", box, err)
		}
	}
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; !strings.HasSuffix(got, "- [x] last one with no newline") ||
		!strings.Contains(got, "1. [x] numbered: \\ backslash\t tab") {
		t.Errorf("after the numbered and last ticks, criteria = %q", got)
	}
}

// TestAcceptanceTickTwiceIsANoOp: a box already ticked is reported, and the
// store is not written again.
func TestAcceptanceTickTwiceIsANoOp(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first")
	v := &beadVerbs{client: fake, actor: "me", out: &bytes.Buffer{}}
	if err := v.tick(id, "first"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	again := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &out}
	if err := again.tick(id, "first"); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if !strings.Contains(out.String(), "already has") {
		t.Errorf("output %q does not say the box is already ticked", out.String())
	}
}

// TestAcceptanceTickRefusesAmbiguousText: two unchecked boxes with the same
// text are not one box, so neither is ticked.
func TestAcceptanceTickRefusesAmbiguousText(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	block := "- [ ] run the gate\n- [ ] run the gate"
	id := seedAcceptance(t, fake, block)
	v := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &bytes.Buffer{}}
	err := v.tick(id, "run the gate")
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("tick of a repeated box: error %v, want it to refuse the ambiguity", err)
	}
	if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != block {
		t.Errorf("criteria = %q, want unchanged", got)
	}
}

func TestAcceptanceTickRefusesABlankBoxText(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	id := seedAcceptance(t, fake, "- [ ] first")
	for _, blank := range []string{"", "   ", "- [ ] "} {
		var out bytes.Buffer
		v := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &out}
		err := v.tick(id, blank)
		for _, want := range []string{"not ticking " + id, "the acceptance box text is empty"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("tick with box text %q: error %v, want it to contain %q", blank, err, want)
			}
		}
		if out.Len() != 0 {
			t.Errorf("refused tick with box text %q printed %q", blank, out.String())
		}
		if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != "- [ ] first" {
			t.Errorf("criteria after blank tick %q = %q, want unchanged", blank, got)
		}
	}
}

// TestAcceptanceTickIsAWriteVerbOfBead: the verb is reachable as gt bead tick
// with its two arguments, so the help and the done refusal name a command that
// runs.
func TestAcceptanceTickIsAWriteVerbOfBead(t *testing.T) {
	t.Parallel()
	for _, c := range beadCmd.Commands() {
		if c.Name() != "tick" {
			continue
		}
		if err := c.Args(c, []string{"gt-abc"}); err == nil {
			t.Error("tick accepted one argument; it needs the id and the box text")
		}
		if err := c.Args(c, []string{"gt-abc", "box"}); err != nil {
			t.Errorf("tick refused two arguments: %v", err)
		}
		return
	}
	t.Fatal("gt bead has no tick verb")
}
