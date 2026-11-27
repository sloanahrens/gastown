package done

import (
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/outbox"
)

const untickedBlock = "- [ ] first\n- [x] done\n- [ ] second"

// TestNoBeadAccessUntickedJudges: the result's ticks are read as outbox.Apply
// writes them, so every box left open is returned under its own line, the
// naming refuseUncheckedCriteria gives, and an index outside the block's boxes
// or a replacement of nothing is an error.
func TestNoBeadAccessUntickedJudges(t *testing.T) {
	t.Parallel()
	seatBlock := "- [x] the seat's box\n- [ ] the seat's open box"
	replaced := outbox.Result{Acceptance: &seatBlock}
	replacedTicked := outbox.Result{Acceptance: &seatBlock, Ticked: []int{1}}
	oneBox, empty := "- [ ] only", " \n\t"
	for name, tc := range map[string]struct {
		acceptance string
		r          outbox.Result
		want       []string
		wantErr    string
	}{
		"every unticked box named":      {acceptance: untickedBlock, want: []string{"- [ ] first", "- [ ] second"}},
		"an indented box named trimmed": {acceptance: "- [x] done\n  - [ ] nested", want: []string{"- [ ] nested"}},
		"the result's ticks cover them": {acceptance: untickedBlock, r: outbox.Result{Ticked: []int{0, 2}}},
		"every box ticked in the text":  {acceptance: "- [x] done\n- [X] too"},
		"no acceptance boxes":           {acceptance: "plain prose"},
		"a replaced block is judged":    {acceptance: "- [ ] the bead's box", r: replaced, want: []string{"- [ ] the seat's open box"}},
		"ticks index the replacement":   {acceptance: "- [ ] the bead's box", r: replacedTicked},
		"tick past the end":             {acceptance: untickedBlock, r: outbox.Result{Ticked: []int{3}}, wantErr: "tick index 3"},
		"negative tick":                 {acceptance: untickedBlock, r: outbox.Result{Ticked: []int{-1}}, wantErr: "tick index -1"},
		"tick past the replacement":     {acceptance: untickedBlock, r: outbox.Result{Acceptance: &oneBox, Ticked: []int{1}}, wantErr: "tick index 1"},
		"empty replacement":             {acceptance: untickedBlock, r: outbox.Result{Acceptance: &empty}, wantErr: "acceptance is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := untickedBoxes(tc.acceptance, tc.r)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
					t.Fatalf("untickedBoxes = %q, %v; want no boxes and an error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("untickedBoxes = %q, %v; want %q and no error", got, err, tc.want)
			}
		})
	}
}
