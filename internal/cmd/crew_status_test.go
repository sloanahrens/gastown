package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func buildOneCrewStatusItem(count crewMailCounter) CrewStatusItem {
	total, unread, errText := crewMailCounts(count, "gastown", "emma")
	return CrewStatusItem{Name: "emma", Rig: "gastown", MailTotal: total, MailUnread: unread, MailError: errText}
}

func crewStatusItemJSON(t *testing.T, item CrewStatusItem) map[string]any {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCrewStatusCountsBeadsMail(t *testing.T) {
	t.Parallel()
	var gotAddress string
	item := buildOneCrewStatusItem(func(address string) (int, int, error) {
		gotAddress = address
		return 5, 3, nil
	})

	if gotAddress != "gastown/crew/emma" {
		t.Errorf("mailbox address = %q, want gastown/crew/emma", gotAddress)
	}
	if item.MailTotal == nil || *item.MailTotal != 5 || item.MailUnread == nil || *item.MailUnread != 3 {
		t.Fatalf("mail = %v/%v, want 3 unread / 5 total", item.MailUnread, item.MailTotal)
	}
	if got := crewMailSummary(item); got != "3 unread / 5 total" {
		t.Errorf("text Mail line = %q", got)
	}

	out := crewStatusItemJSON(t, item)
	if out["mail_total"] != float64(5) || out["mail_unread"] != float64(3) {
		t.Errorf("json mail_total/mail_unread = %v/%v, want 5/3", out["mail_total"], out["mail_unread"])
	}
	if _, ok := out["mail_error"]; ok {
		t.Errorf("json carries mail_error on success: %v", out["mail_error"])
	}
}

func TestCrewStatusMailErrorIsUnknownNotZero(t *testing.T) {
	t.Parallel()
	item := buildOneCrewStatusItem(func(string) (int, int, error) {
		return 0, 0, errors.New("dolt unreachable")
	})

	if item.MailTotal != nil || item.MailUnread != nil {
		t.Fatalf("mail counts = %v/%v on error, want nil (unknown)", item.MailUnread, item.MailTotal)
	}
	if line := crewMailSummary(item); !strings.Contains(line, "unknown") || !strings.Contains(line, "dolt unreachable") {
		t.Errorf("text Mail line = %q, want unknown with the error", line)
	}

	out := crewStatusItemJSON(t, item)
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
