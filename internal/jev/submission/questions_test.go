package submission

import "testing"

// The battery every submission is asked, the map submission.Pass sends with
// each Ask, names will_pass_review as a noul with both of its outcomes
// described: it was dropped at question version 3, so no submission after that
// got the answer.
func TestSubmissionBatteryAsksWillPassReview(t *testing.T) {
	t.Parallel()
	q, ok := Questions()[QWillPassReview]
	if !ok {
		t.Fatalf("the battery asks no %s question", QWillPassReview)
	}
	if q.Type != "noul" || q.True == "" || q.False == "" {
		t.Errorf("%s = %+v, want a noul describing both outcomes", QWillPassReview, q)
	}
}
