package deepseekbalance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apiURL is the base URL the fake API answers under. No socket is opened: the
// client's transport is in-process.
const apiURL = "http://deepseek.test"

// balanceJSON is a GET /user/balance body with two entries: Fetch must take the
// first and ignore the second.
const balanceJSON = `{"is_available":true,"balance_infos":[` +
	`{"currency":"USD","total_balance":"19.85","granted_balance":"0.25","topped_up_balance":"19.60"},` +
	`{"currency":"CNY","total_balance":"999.00","granted_balance":"0.00","topped_up_balance":"999.00"}]}`

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeAPI is an http.Client whose transport answers every request with handler,
// in process, through a recorder rather than a listening server.
func fakeAPI(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Result(), nil
	})}
}

// balanceAPI answers GET /user/balance with status and body when the bearer
// header is wantAuth, and 401 otherwise.
func balanceAPI(wantAuth string, status int, body string) *http.Client {
	return fakeAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" || r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

// unreachableAPI is a client whose every request fails at the transport, as a
// refused connection does.
func unreachableAPI() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}
}

func TestFetchParsesTheFirstBalanceInfo(t *testing.T) {
	t.Parallel()
	const key = "sk-fetch-test-key"

	t.Run("parses the first entry and sends the bearer header", func(t *testing.T) {
		t.Parallel()
		got, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, balanceJSON), apiURL, key)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		want := Sample{Total: 19.85, Granted: 0.25, ToppedUp: 19.60}
		if got != want {
			t.Errorf("Fetch = %+v; want %+v", got, want)
		}
	})

	t.Run("trailing slash on the base URL is not doubled", func(t *testing.T) {
		t.Parallel()
		if _, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, balanceJSON), apiURL+"/", key); err != nil {
			t.Fatalf("Fetch with trailing slash: %v", err)
		}
	})

	t.Run("non-200 status names the status", func(t *testing.T) {
		t.Parallel()
		_, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusInternalServerError, ""), apiURL, key)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("Fetch error = %v; want one naming HTTP 500", err)
		}
	})

	t.Run("bad JSON names the parse step", func(t *testing.T) {
		t.Parallel()
		_, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, `{"balance_infos": [`), apiURL, key)
		if err == nil || !strings.Contains(err.Error(), "parse balance response") {
			t.Errorf("Fetch error = %v; want one naming the parse step", err)
		}
	})

	t.Run("empty balance_infos is an error", func(t *testing.T) {
		t.Parallel()
		_, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, `{"balance_infos":[]}`), apiURL, key)
		if err == nil || !strings.Contains(err.Error(), "no balance_infos entry") {
			t.Errorf("Fetch error = %v; want one naming the missing entry", err)
		}
	})

	// Each amount field has its own refusal: a non-decimal value in any of them
	// fails the read and names that field.
	for _, field := range []string{"total_balance", "granted_balance", "topped_up_balance"} {
		t.Run("a non-decimal "+field+" names the field", func(t *testing.T) {
			t.Parallel()
			amounts := map[string]string{"total_balance": "1.00", "granted_balance": "0.00", "topped_up_balance": "1.00"}
			amounts[field] = "ten"
			body := fmt.Sprintf(`{"balance_infos":[{"total_balance":%q,"granted_balance":%q,"topped_up_balance":%q}]}`,
				amounts["total_balance"], amounts["granted_balance"], amounts["topped_up_balance"])
			_, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, body), apiURL, key)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("Fetch error = %v; want one naming %s", err, field)
			}
		})
	}

	t.Run("a nil client is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := Fetch(context.Background(), nil, apiURL, key); err == nil {
			t.Error("Fetch with nil client: want an error")
		}
	})
}

func TestEvaluateRules(t *testing.T) {
	t.Parallel()
	sample := func(total float64) Sample { return Sample{Total: total} }
	prev := func(total float64) *Sample { s := sample(total); return &s }

	cases := []struct {
		name     string
		prev     *Sample
		cur      Sample
		failures int
		// wantAlerts is the number of alert lines expected.
		wantAlerts int
		// wantHas is a substring every alert must carry, when non-empty.
		wantHas string
	}{
		{name: "spend above 1.00 alerts with the pace", prev: prev(50.00), cur: sample(48.99), wantAlerts: 1, wantHas: "$6.06/h"},
		{name: "spend of exactly 1.00 does not alert", prev: prev(50.00), cur: sample(49.00), wantAlerts: 0},
		{name: "spend of exactly 1.00 on cents that do not add exactly", prev: prev(32.02), cur: sample(31.02), wantAlerts: 0},
		{name: "a rise is a top-up, not spend", prev: prev(30.00), cur: sample(50.00), wantAlerts: 0},
		{name: "first sample never alerts on spend", prev: nil, cur: sample(100.00), wantAlerts: 0},
		{name: "total below 20 alerts", prev: nil, cur: sample(19.99), wantAlerts: 1, wantHas: "below $20.00"},
		{name: "total of exactly 20 does not alert", prev: nil, cur: sample(20.00), wantAlerts: 0},
		{name: "failure 1 raises nothing", prev: prev(50.00), failures: 1, wantAlerts: 0},
		{name: "failure 2 raises nothing", prev: prev(50.00), failures: 2, wantAlerts: 0},
		{name: "failure 3 raises unreachable only", prev: prev(50.00), failures: 3, wantAlerts: 1, wantHas: "unreachable"},
		{name: "failure 4 does not repeat unreachable", prev: prev(50.00), failures: 4, wantAlerts: 0},
		{name: "a failed read is not judged as a low balance", prev: prev(50.00), failures: 3, wantAlerts: 1, wantHas: "unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(tc.prev, tc.cur, tc.failures)
			if len(got) != tc.wantAlerts {
				t.Fatalf("Evaluate = %q; want %d alert(s)", got, tc.wantAlerts)
			}
			for _, line := range got {
				if tc.wantHas != "" && !strings.Contains(line, tc.wantHas) {
					t.Errorf("alert %q lacks %q", line, tc.wantHas)
				}
			}
		})
	}
}

func TestKeyNeverAppears(t *testing.T) {
	t.Parallel()
	const key = "sk-SECRET-canary-7f3a"
	ctx := context.Background()

	var texts []string
	for _, tc := range []struct {
		name   string
		client *http.Client
		base   string
	}{
		{name: "HTTP 500", client: balanceAPI("Bearer "+key, http.StatusInternalServerError, ""), base: apiURL},
		{name: "bad JSON", client: balanceAPI("Bearer "+key, http.StatusOK, `{"balance_infos": [`), base: apiURL},
		{name: "rejected token", client: balanceAPI("Bearer sk-other", http.StatusOK, balanceJSON), base: apiURL},
		{name: "unreachable", client: unreachableAPI(), base: apiURL},
		{name: "unparseable URL", client: balanceAPI("Bearer "+key, http.StatusOK, balanceJSON), base: "http://[::1"},
	} {
		_, err := Fetch(ctx, tc.client, tc.base, key)
		if err == nil {
			t.Fatalf("Fetch (%s): want an error", tc.name)
		}
		texts = append(texts, err.Error())
	}
	texts = append(texts, Evaluate(nil, Sample{Total: 5}, 3)...)
	texts = append(texts, Evaluate(&Sample{Total: 50}, Sample{Total: 40}, 0)...)

	for _, text := range texts {
		if strings.Contains(text, key) {
			t.Errorf("key appears in %q", text)
		}
	}
}
