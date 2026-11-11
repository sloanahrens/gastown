package trackerclient

import (
	"strings"
	"testing"
)

// TestUpdateOffersFlagRefusesWithNoBd: the tracker client runs no bd, so it
// cannot say what bd update offers; the answer is an error naming that, never a
// false a caller could read as "the flag is missing".
func TestUpdateOffersFlagRefusesWithNoBd(t *testing.T) {
	t.Parallel()
	offers, err := (&Client{}).UpdateOffersFlag("--if-acceptance")
	if err == nil || !strings.Contains(err.Error(), "runs no bd") || !strings.Contains(err.Error(), "--if-acceptance") {
		t.Fatalf("UpdateOffersFlag = (%v, %v); want an error naming that the tracker client runs no bd and the flag asked for", offers, err)
	}
	if offers {
		t.Errorf("UpdateOffersFlag answered true beside its error")
	}
}
