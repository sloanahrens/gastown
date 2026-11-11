// Package deepseekbalance reads the DeepSeek account balance and decides which
// alerts a pair of readings raises. It is the Go port of the balance check the
// Mac launchd spend watcher ran in bash, so the check can run in the daemon pod.
//
// Fetch makes the one HTTP call; Evaluate is pure: it reads no clock and does no
// I/O. The HTTP client and the base URL are parameters, so tests run against an
// httptest server. The key is sent only in the Authorization header and never
// appears in an error or an alert.
package deepseekbalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// spendAlertAbove is the spend between two samples that raises an alert. A spend
// of exactly this amount does not.
const spendAlertAbove = 1.00

// spendTolerance absorbs binary floating-point error in the difference of two
// decimal balances, so that two readings exactly one dollar apart do not read as
// a hair over it. It is not cent rounding: 1.004 still alerts and 0.996 does not.
const spendTolerance = 1e-9

// lowBalanceBelow is the total balance that raises a low-balance alert. A total
// of exactly this amount does not.
const lowBalanceBelow = 20.00

// unreachableAt is the consecutive-failure count that raises the unreachable
// alert. It fires on this failure only, not on the ones after it.
const unreachableAt = 3

// maxBodyBytes caps how much of the balance response Fetch reads.
const maxBodyBytes = 1 << 20

// Sample is one balance reading, in the account currency.
type Sample struct {
	Total    float64
	Granted  float64
	ToppedUp float64
}

// balanceResponse is the part of GET /user/balance the check reads. The API
// returns every amount as a decimal string.
type balanceResponse struct {
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

type balanceInfo struct {
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// Fetch reads the balance at baseURL/user/balance with key as the bearer token
// and returns the first balance_infos entry. A non-200 status, a body that does
// not parse, an empty balance_infos, or a non-finite amount is an error. Fetch
// sets no deadline of its own: the caller's context bounds the call.
func Fetch(ctx context.Context, client *http.Client, baseURL, key string) (Sample, error) {
	if client == nil {
		return Sample{}, errors.New("deepseekbalance: nil http client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/user/balance", nil)
	if err != nil {
		return Sample{}, fmt.Errorf("deepseekbalance: build balance request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return Sample{}, fmt.Errorf("deepseekbalance: fetch balance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Sample{}, fmt.Errorf("deepseekbalance: balance endpoint answered HTTP %d", resp.StatusCode)
	}

	var body balanceResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return Sample{}, fmt.Errorf("deepseekbalance: parse balance response: %w", err)
	}
	if len(body.BalanceInfos) == 0 {
		return Sample{}, errors.New("deepseekbalance: parse balance response: no balance_infos entry")
	}
	info := body.BalanceInfos[0]

	total, err := parseAmount("total_balance", info.TotalBalance)
	if err != nil {
		return Sample{}, err
	}
	granted, err := parseAmount("granted_balance", info.GrantedBalance)
	if err != nil {
		return Sample{}, err
	}
	toppedUp, err := parseAmount("topped_up_balance", info.ToppedUpBalance)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Total: total, Granted: granted, ToppedUp: toppedUp}, nil
}

// parseAmount reads one decimal-string amount, naming the field on failure. A
// value that parses to NaN or an infinity is refused: it would make every
// comparison false and suppress the alerts it should raise.
func parseAmount(field, text string) (float64, error) {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("deepseekbalance: parse balance response: %s: %w", field, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("deepseekbalance: parse balance response: %s: not a finite amount", field)
	}
	return v, nil
}

// Evaluate returns the alert lines for the readings. prev is the last good
// sample, or nil on the first run, which never alerts on spend. interval is the
// spacing between prev and cur; the spend pace is scaled to an hour at it, and
// it must be positive or Evaluate panics, as time.NewTicker does. cur is judged
// only when consecutiveFailures is zero: after a failed fetch cur is not a
// reading, so only the unreachable rule applies, and it fires on failure
// unreachableAt only.
func Evaluate(prev *Sample, cur Sample, consecutiveFailures int, interval time.Duration) []string {
	if interval <= 0 {
		panic("deepseekbalance: Evaluate interval must be positive")
	}
	var alerts []string
	if consecutiveFailures == 0 {
		if prev != nil {
			// The threshold compares the unrounded difference; cents appear only in
			// the message.
			spend := prev.Total - cur.Total
			if spend > spendAlertAbove+spendTolerance {
				perHour := spend * float64(time.Hour) / float64(interval)
				alerts = append(alerts, fmt.Sprintf(
					"DeepSeek spent $%.2f in the last %d min, a pace of $%.2f/h",
					spend, int(interval/time.Minute), perHour))
			}
		}
		if cur.Total < lowBalanceBelow {
			alerts = append(alerts, fmt.Sprintf(
				"DeepSeek balance is $%.2f, below $%.2f", cur.Total, lowBalanceBelow))
		}
	}
	if consecutiveFailures == unreachableAt {
		alerts = append(alerts, fmt.Sprintf(
			"DeepSeek balance endpoint unreachable for %d consecutive checks", unreachableAt))
	}
	return alerts
}
