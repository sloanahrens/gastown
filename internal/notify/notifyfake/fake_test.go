package notifyfake

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
)

func TestFakeNotifierContract(t *testing.T) {
	t.Parallel()
	RunNotifierContract(t, func(t *testing.T) notify.Notifier {
		r := New()
		r.Missing(MissingTarget)
		return r
	})
}

func TestRecorderRecordsEachKindInOrder(t *testing.T) {
	t.Parallel()
	r := New()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.MailSend(ctx, "mayor/", "s", "b", notify.From("convoy/c1"), notify.NoNotify()))
	must(r.Nudge(ctx, "deacon", "wake"))
	must(r.Escalate(ctx, notify.Escalation{Severity: "high", Description: "d", Fingerprint: "k"}))
	must(r.ClearEscalations(ctx, "ok", "k", " "))

	want := []Call{
		{Kind: KindMail, To: "mayor/", Subject: "s", Body: "b", Mail: notify.MailOptions{From: "convoy/c1", NoNotify: true}},
		{Kind: KindNudge, Target: "deacon", Message: "wake"},
		{Kind: KindEscalate, Escalation: notify.Escalation{Severity: "high", Description: "d", Fingerprint: "k"}},
		{Kind: KindClear, Reason: "ok", Fingerprints: []string{"k"}},
	}
	if got := r.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls() = %+v\nwant      %+v", got, want)
	}
	if got := r.Nudges(); len(got) != 1 || got[0].Target != "deacon" {
		t.Fatalf("Nudges() = %+v", got)
	}
}

func TestRecorderScriptedFailureIsRecorded(t *testing.T) {
	t.Parallel()
	r := New()
	boom := errors.New("boom")
	r.Fail(KindMail, boom)
	if err := r.MailSend(t.Context(), "mayor/", "s", "b"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got := r.Mails(); len(got) != 1 || !errors.Is(got[0].Err, boom) {
		t.Fatalf("Mails() = %+v, want the failed attempt recorded", got)
	}
	r.Fail(KindMail, nil)
	if err := r.MailSend(t.Context(), "mayor/", "s", "b"); err != nil {
		t.Fatalf("after Fail(nil): %v", err)
	}
	if err := r.Nudge(t.Context(), "mayor", "m"); err != nil {
		t.Fatalf("Fail(KindMail) leaked into nudges: %v", err)
	}
}

func TestRecorderDoesNotRecordRefusedRequests(t *testing.T) {
	t.Parallel()
	r := New()
	if err := r.Nudge(t.Context(), "", "m"); !errors.Is(err, notify.ErrInvalid) {
		t.Fatalf("Nudge without target: %v", err)
	}
	if err := r.Escalate(t.Context(), notify.Escalation{}); !errors.Is(err, notify.ErrInvalid) {
		t.Fatalf("Escalate without description: %v", err)
	}
	if got := r.Calls(); len(got) != 0 {
		t.Fatalf("Calls() = %+v, want nothing recorded for invalid requests", got)
	}
}
