package deepseekbalance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apiURL is the base URL the fake API answers under. No socket is opened: the
// client's transport is in-process.
const apiURL = "http://deepseek.test"

// tenMinutes is the sample spacing the rule tables pass to Evaluate.
const tenMinutes = 10 * time.Minute

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

// balanceWith returns a one-entry balance body with field set to value and the
// other two amounts at a valid decimal.
func balanceWith(field, value string) string {
	amounts := map[string]string{"total_balance": "1.00", "granted_balance": "0.00", "topped_up_balance": "1.00"}
	amounts[field] = value
	return fmt.Sprintf(`{"balance_infos":[{"total_balance":%q,"granted_balance":%q,"topped_up_balance":%q}]}`,
		amounts["total_balance"], amounts["granted_balance"], amounts["topped_up_balance"])
}

// amountFields are the three decimal-string amounts a balance entry carries.
var amountFields = []string{"total_balance", "granted_balance", "topped_up_balance"}

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
	for _, field := range amountFields {
		t.Run("a non-decimal "+field+" names the field", func(t *testing.T) {
			t.Parallel()
			_, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, balanceWith(field, "ten")), apiURL, key)
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

func TestFetchRejectsNonFiniteBalance(t *testing.T) {
	t.Parallel()
	const key = "sk-nonfinite-test-key"

	for _, field := range amountFields {
		for _, value := range []string{"NaN", "Inf", "-Inf", "1e999"} {
			t.Run(field+" "+value, func(t *testing.T) {
				t.Parallel()
				got, err := Fetch(context.Background(), balanceAPI("Bearer "+key, http.StatusOK, balanceWith(field, value)), apiURL, key)
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("Fetch error = %v; want one naming %s", err, field)
				}
				if got != (Sample{}) {
					t.Errorf("Fetch returned %+v alongside an error; want no sample", got)
				}
			})
		}
	}
}

func TestFetchHonoursContextDeadline(t *testing.T) {
	t.Parallel()
	// The test sets the deadline; the transport stands in for a hung endpoint and
	// waits on the request's context. A request that carries a different deadline,
	// or none, fails at once instead of waiting, so a regression cannot hang here.
	deadline := time.Unix(0, 0)
	hung := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got, ok := r.Context().Deadline(); !ok || !got.Equal(deadline) {
			return nil, errors.New("request does not carry the caller's deadline")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	_, err := Fetch(ctx, hung, apiURL, "sk-deadline-test-key")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fetch error = %v; want one wrapping context.DeadlineExceeded", err)
	}
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
			got := Evaluate(tc.prev, tc.cur, tc.failures, tenMinutes)
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

func TestEvaluateUsesTheIntervalArgument(t *testing.T) {
	t.Parallel()
	prev, cur := &Sample{Total: 50}, Sample{Total: 45}

	ten := Evaluate(prev, cur, 0, tenMinutes)
	thirty := Evaluate(prev, cur, 0, 30*time.Minute)
	if len(ten) != 1 || len(thirty) != 1 {
		t.Fatalf("Evaluate = %q and %q; want one spend alert each", ten, thirty)
	}
	if !strings.Contains(ten[0], "in the last 10 min") || !strings.Contains(ten[0], "$30.00/h") {
		t.Errorf("10-minute alert = %q; want a 10 min window and a $30.00/h pace", ten[0])
	}
	if !strings.Contains(thirty[0], "in the last 30 min") || !strings.Contains(thirty[0], "$10.00/h") {
		t.Errorf("30-minute alert = %q; want a 30 min window and a $10.00/h pace", thirty[0])
	}
}

func TestEvaluateRefusesANonPositiveInterval(t *testing.T) {
	t.Parallel()
	for _, interval := range []time.Duration{0, -tenMinutes} {
		t.Run(interval.String(), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("Evaluate with interval %v: want a panic", interval)
				}
			}()
			Evaluate(&Sample{Total: 50}, Sample{Total: 40}, 0, interval)
		})
	}
}

func TestEvaluateComparesBeforeRounding(t *testing.T) {
	t.Parallel()
	// 50.000 - 48.996 = 1.004: over the threshold, and the message shows it to the
	// cent as $1.00.
	over := Evaluate(&Sample{Total: 50}, Sample{Total: 48.996}, 0, tenMinutes)
	if len(over) != 1 || !strings.Contains(over[0], "spent $1.00 in the last") {
		t.Errorf("Evaluate(50.000 -> 48.996) = %q; want one alert showing $1.00", over)
	}
	// 50.000 - 49.004 = 0.996: under the threshold, so no alert.
	under := Evaluate(&Sample{Total: 50}, Sample{Total: 49.004}, 0, tenMinutes)
	if len(under) != 0 {
		t.Errorf("Evaluate(50.000 -> 49.004) = %q; want no alert", under)
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
	texts = append(texts, Evaluate(nil, Sample{Total: 5}, 3, tenMinutes)...)
	texts = append(texts, Evaluate(&Sample{Total: 50}, Sample{Total: 40}, 0, tenMinutes)...)

	for _, text := range texts {
		if strings.Contains(text, key) {
			t.Errorf("key appears in %q", text)
		}
	}
}
