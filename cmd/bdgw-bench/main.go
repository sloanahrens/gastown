// Command bdgw-bench measures the bd gateway's latency for the bd container
// load test (docs/design/bd-container.md, "Latency budget"). It sends 200 show
// and 50 list --json calls, one at a time, through the gateway's POST /v1/exec,
// and prints p50, p95 and p99 with PASS or OVER BUDGET. It needs a running
// gateway, so it is a measurement, not a test-tier test.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// The command's exit statuses: 0 for PASS, 1 for OVER BUDGET or a failed call,
// 2 for a usage problem the run refuses before it sends anything.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

const (
	// execPath is the gateway route one bd call goes to.
	execPath = "/v1/exec"
	// callerName is the X-Gt-Caller value the gateway logs for these calls.
	callerName = "bdgw-bench"
	// callTimeoutMS is the limit the head asks the gateway for; the gateway caps
	// it by class.
	callTimeoutMS = 30000
	// clientTimeout bounds one call's wait, so a gateway that stops answering
	// ends the run rather than hanging it.
	clientTimeout = 60 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

// run is main without the process: now is the clock the latencies are read from,
// so a test can drive the run with a fake one. It returns the exit status.
func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("bdgw-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gatewayURL := fs.String("url", "", "the gateway's base URL, http or https")
	tokenFile := fs.String("token-file", "", "the file holding the gateway's read-class bearer token")
	rig := fs.String("rig", "", "the rig whose bd answers the calls; empty is the town")
	bead := fs.String("bead", "", "a bead id the show calls read")
	inPodP50 := fs.Duration("in-pod-p50", 40*time.Millisecond, "the in-pod bd p50 the overhead is measured from")
	inPodP95 := fs.Duration("in-pod-p95", 40*time.Millisecond, "the in-pod bd p95 the overhead is measured from")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bdgw-bench: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := checkFlags(*gatewayURL, *tokenFile, *bead); err != nil {
		fmt.Fprintf(stderr, "bdgw-bench: %v\n", err)
		return exitUsage
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "bdgw-bench: %v\n", err)
		return exitUsage
	}

	client := &http.Client{Timeout: clientTimeout}
	do := func(argv []string) error {
		return gatewayCall(client, *gatewayURL, token, *rig, argv)
	}
	latencies, err := measure(workload(*bead), do, now)
	if err != nil {
		fmt.Fprintf(stderr, "bdgw-bench: %v\n", err)
		return exitFailed
	}
	sum, err := Summarize(latencies)
	if err != nil {
		fmt.Fprintf(stderr, "bdgw-bench: %v\n", err)
		return exitFailed
	}
	base := Baseline{P50: *inPodP50, P95: *inPodP95}
	pass := Verdict(sum, base)
	report(stdout, sum, base, pass)
	if !pass {
		return exitFailed
	}
	return exitOK
}

// checkFlags refuses a run that names no gateway, no token file or no bead, or
// a gateway URL that is not an http or https address with a host.
func checkFlags(gatewayURL, tokenFile, bead string) error {
	if gatewayURL == "" {
		return errors.New("-url is required")
	}
	u, err := neturl.Parse(gatewayURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("-url must be an http or https URL with a host")
	}
	if tokenFile == "" {
		return errors.New("-token-file is required")
	}
	if bead == "" {
		return errors.New("-bead is required")
	}
	return nil
}

// readToken reads the bearer token from path, without its surrounding space. An
// empty file is refused: an empty bearer header would fail every call.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty", path)
	}
	return token, nil
}

// gatewayCall posts one bd call to the gateway and reads its answer. The error
// names the HTTP status or the gateway's error code and the exit status, never
// the token or the call's output.
func gatewayCall(client *http.Client, gatewayURL, token, rig string, argv []string) error {
	head, err := json.Marshal(bdgateway.Request{
		Argv:      argv,
		Rig:       rig,
		TimeoutMS: callTimeoutMS,
	})
	if err != nil {
		return fmt.Errorf("encode the call: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(gatewayURL, "/")+execPath, bytes.NewReader(append(head, '\n')))
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gt-Caller", callerName)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the gateway did not answer: %w", err)
	}
	// The answer is read to its exit frame or refused by status, so a close error
	// cannot change the verdict.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the gateway answered HTTP %d", resp.StatusCode)
	}
	return readAnswer(resp.Body)
}

// readAnswer reads the frames of one call and returns nil when bd exited 0. Stdout
// and stderr frames are read past: the bench measures latency, not output.
func readAnswer(r io.Reader) error {
	for {
		frame, err := bdgateway.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return errors.New("the answer ended before an exit status")
		}
		if err != nil {
			return fmt.Errorf("read the answer: %w", err)
		}
		switch frame.Kind {
		case bdgateway.FrameError:
			payload, err := frame.GatewayError()
			if err != nil {
				return errors.New("the gateway refused the call with an unreadable error")
			}
			return fmt.Errorf("the gateway refused the call (%s)", payload.Code)
		case bdgateway.FrameExit:
			status, err := frame.ExitStatus()
			if err != nil {
				return fmt.Errorf("the exit frame is unreadable: %w", err)
			}
			if status != 0 {
				return fmt.Errorf("bd exited %d", status)
			}
			return nil
		}
	}
}

// report prints the percentiles, the in-pod baseline they are read against, and
// the verdict.
func report(w io.Writer, s Summary, base Baseline, pass bool) {
	fmt.Fprintf(w, "calls: %d show, %d list --json\n", showCalls, listCalls)
	fmt.Fprintf(w, "p50: %s\n", s.P50)
	fmt.Fprintf(w, "p95: %s\n", s.P95)
	fmt.Fprintf(w, "p99: %s\n", s.P99)
	fmt.Fprintf(w, "max: %s\n", s.Max)
	fmt.Fprintf(w, "in-pod baseline: p50 %s, p95 %s\n", base.P50, base.P95)
	if pass {
		fmt.Fprintln(w, "PASS")
		return
	}
	fmt.Fprintln(w, "OVER BUDGET")
}
