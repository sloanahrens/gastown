package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func stubCrewMailCounter(t *testing.T, fn func(address string) (int, int, error)) {
	t.Helper()
	orig := crewMailCounter
	crewMailCounter = fn
	t.Cleanup(func() { crewMailCounter = orig })
}

func buildOneCrewStatusItem(t *testing.T) CrewStatusItem {
	t.Helper()
	total, unread, errText := crewMailCounts("gastown", "emma")
	return CrewStatusItem{Name: "emma", Rig: "gastown", MailTotal: total, MailUnread: unread, MailError: errText}
}

func TestCrewStatusCountsBeadsMail(t *testing.T) {
	var gotAddress string
	stubCrewMailCounter(t, func(address string) (int, int, error) {
		gotAddress = address
		return 5, 3, nil
	})

	item := buildOneCrewStatusItem(t)

	if gotAddress != "gastown/crew/emma" {
		t.Errorf("mailbox address = %q, want gastown/crew/emma", gotAddress)
	}
	if item.MailTotal == nil || *item.MailTotal != 5 || item.MailUnread == nil || *item.MailUnread != 3 {
		t.Fatalf("mail = %v/%v, want 3 unread / 5 total", item.MailUnread, item.MailTotal)
	}
	if got := crewMailSummary(item); got != "3 unread / 5 total" {
		t.Errorf("text Mail line = %q", got)
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["mail_total"] != float64(5) || out["mail_unread"] != float64(3) {
		t.Errorf("json mail_total/mail_unread = %v/%v, want 5/3", out["mail_total"], out["mail_unread"])
	}
	if _, ok := out["mail_error"]; ok {
		t.Errorf("json carries mail_error on success: %v", out["mail_error"])
	}
}

func TestCrewStatusMailErrorIsUnknownNotZero(t *testing.T) {
	stubCrewMailCounter(t, func(string) (int, int, error) {
		return 0, 0, errors.New("dolt unreachable")
	})

	item := buildOneCrewStatusItem(t)

	if item.MailTotal != nil || item.MailUnread != nil {
		t.Fatalf("mail counts = %v/%v on error, want nil (unknown)", item.MailUnread, item.MailTotal)
	}
	if line := crewMailSummary(item); !strings.Contains(line, "unknown") || !strings.Contains(line, "dolt unreachable") {
		t.Errorf("text Mail line = %q, want unknown with the error", line)
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["mail_total"]; !ok || v != nil {
		t.Errorf("json mail_total = %v (present=%v), want null", v, ok)
	}
	if v, ok := out["mail_unread"]; !ok || v != nil {
		t.Errorf("json mail_unread = %v (present=%v), want null", v, ok)
	}
	if out["mail_error"] != "dolt unreachable" {
		t.Errorf("json mail_error = %v", out["mail_error"])
	}
}
