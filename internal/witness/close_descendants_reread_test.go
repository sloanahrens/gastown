package witness

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestCloseViaCLIUntilNoProgressKeepsATransientFailureOutOfTheRefusalLabel:
// the retry loop's second pass can end on the re-read that checks a batch
// rather than on a refusal. A caller reads errors.Is(err,
// beads.ErrCloseRefused) as "bd refused these steps" — closeStepsThenRoot
// leaves a molecule's root open on it — so a re-read that died must not
// borrow the label: it strands a molecule whose steps all closed (gt-22hdp.36).
func TestCloseViaCLIUntilNoProgressKeepsATransientFailureOutOfTheRefusalLabel(t *testing.T) {
	t.Parallel()
	status := map[string]string{"gt-s1": "open", "gt-s2": "open", "gt-s3": "open", "gt-s4": "open"}
	rereads := 0
	bd := &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			switch {
			case args[0] == "show":
				// The re-read after a batch close. It works while the loop
				// is making progress and dies on the pass that finishes.
				rereads++
				if rereads > 1 {
					return "", errors.New("bd show: dial tcp: connection refused")
				}
				var out []string
				for _, a := range args[1:] {
					if st, ok := status[a]; ok {
						out = append(out, `{"id":"`+a+`","status":"`+st+`"}`)
					}
				}
				return "[" + strings.Join(out, ",") + "]", nil
			}
			return "", nil
		},
		Run: func(_ string, args ...string) error {
			if args[0] != "close" {
				return nil
			}
			// Pass 1 has bd skip gt-s2 and gt-s4; pass 2 closes them.
			skip := map[string]bool{}
			if rereads == 0 {
				skip = map[string]bool{"gt-s2": true, "gt-s4": true}
			}
			closedAny := false
			for _, a := range args[1:] {
				if _, ok := status[a]; !ok || skip[a] {
					continue
				}
				status[a] = "closed"
				closedAny = true
			}
			if !closedAny {
				return errors.New("exit status 1")
			}
			return nil
		},
	}

	err := closeViaCLIUntilNoProgress(bd, t.TempDir(), []string{"gt-s1", "gt-s2", "gt-s3", "gt-s4"})
	if err == nil {
		t.Fatal("closeViaCLIUntilNoProgress = nil, want the re-read failure")
	}
	if errors.Is(err, beads.ErrCloseRefused) {
		t.Errorf("error %v wraps beads.ErrCloseRefused; the pass ended on a re-read failure, not a refusal", err)
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *beads.PartialCloseError; the steps pass 1 closed are lost", err)
	}
	if want := []string{"gt-s1", "gt-s3"}; !reflect.DeepEqual(pe.Closed, want) {
		t.Errorf("Closed = %v, want %v (the steps the first pass closed)", pe.Closed, want)
	}
}
