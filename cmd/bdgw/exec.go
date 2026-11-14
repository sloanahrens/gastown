package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// execConfig is what bdgw exec adds to the shared settings: the head fields an
// HTTP caller sends, named as command-line values instead.
type execConfig struct {
	config
	rig       string
	cwd       string
	timeoutMS int
	caller    string
}

func newExecConfig() *execConfig {
	return &execConfig{config: defaultConfig(), caller: "bdgw-exec"}
}

func runExec(args []string, stdout, stderr io.Writer) int {
	ec := newExecConfig()
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindShared(fs, &ec.config)
	fs.StringVar(&ec.rig, "rig", ec.rig, "the rig whose .beads the call reads, empty for the town's")
	fs.StringVar(&ec.cwd, "cwd", ec.cwd, "the working directory relative to the workspace root")
	fs.IntVar(&ec.timeoutMS, "timeout-ms", ec.timeoutMS, "the call's limit in milliseconds, capped by the verb's class")
	fs.StringVar(&ec.caller, "caller", ec.caller, "the caller name the call log records")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	argv := fs.Args()
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "bdgw exec: no bd arguments after --")
		return exitUsage
	}
	if err := ec.require("verbs", "log-dir", "workspace", "bd", "home"); err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitUsage
	}

	policy, err := bdgateway.LoadPolicyFile(ec.verbsPath)
	if err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitFailed
	}
	log, err := ec.callLog(policy)
	if err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitFailed
	}
	defer func() {
		if err := log.Close(); err != nil {
			fmt.Fprintln(stderr, "bdgw exec: call log:", err)
		}
	}()
	runner, err := ec.executor()
	if err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitFailed
	}

	// A signal cancels the call, which kills bd's whole process group rather
	// than leaving it running after its requester.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return execCall(ctx, policy, log, runner, ec, argv, callerEnv(os.Environ()), os.Stdin, stdout, stderr)
}

// execCall runs one call the way the HTTP handler does: the operator class
// decides what may run (exec is the container's own path, so there is no token),
// the same class cap bounds the timeout, the same executor runs it, and the same
// call log records it. bd's streams go to stdout and stderr and its exit status
// is the status returned.
func execCall(ctx context.Context, policy *bdgateway.Policy, log *bdgateway.CallLog, runner bdgateway.CallRunner, ec *execConfig, argv []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	verb, err := policy.Authorize(bdgateway.TokenOperator, argv)
	if err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitRefused
	}
	filtered, err := bdgateway.FilterEnv(bdgateway.TokenOperator, env)
	if err != nil {
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitRefused
	}
	req := bdgateway.Request{
		Argv:      argv,
		Env:       filtered,
		Rig:       ec.rig,
		Cwd:       ec.cwd,
		TimeoutMS: bdgateway.CapTimeout(ec.timeoutMS, verb),
	}
	start := time.Now()
	sink := &streamSink{stdout: stdout, stderr: stderr}
	if err := runner.Exec(ctx, req, stdin, sink); !sink.Started() {
		if err == nil {
			err = errors.New("the call returned without a frame")
		}
		fmt.Fprintln(stderr, "bdgw exec:", err)
		return exitRefused
	}
	status, seen := sink.Status()
	if !seen {
		// The stream began but no exit frame arrived: the executor's ending is
		// the reason, and a call that could not report bd's status is never a
		// success. The HTTP handler records the same 125 for this ending.
		fmt.Fprintln(stderr, "bdgw exec: the call ended without an exit frame")
		status = exitRefused
	}
	log.Log(bdgateway.CallRecord{
		Class:    string(bdgateway.TokenOperator),
		Caller:   ec.caller,
		Argv:     argv,
		ExitCode: status,
		Duration: time.Since(start),
	})
	return status
}

// callerEnv turns the process environment into the map a request carries. The
// allowlist is applied by selection rather than refusal here: an exec process's
// environment always holds names a request may not carry (HOME, PATH,
// HOSTNAME), so refusing the environment would refuse every call.
func callerEnv(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		env[k] = v
	}
	return env
}

// streamSink writes the executor's frames to bd's own streams and records the
// exit status, so exec looks to its caller like bd itself. A gateway error frame
// is shown on stderr; the exit frame after it carries the status. No lock is
// needed: the executor serializes its frames and waits for its pumps before it
// returns, so a Write and the Status read after it never race.
type streamSink struct {
	stdout  io.Writer
	stderr  io.Writer
	started bool
	status  int
	exited  bool
}

func (s *streamSink) Write(p []byte) (int, error) {
	frame, err := bdgateway.ReadFrame(bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	s.started = true
	switch frame.Kind {
	case bdgateway.FrameStdout:
		if _, err := s.stdout.Write(frame.Payload); err != nil {
			return 0, err
		}
	case bdgateway.FrameStderr:
		if _, err := s.stderr.Write(frame.Payload); err != nil {
			return 0, err
		}
	case bdgateway.FrameError:
		var payload bdgateway.ErrorPayload
		if err := json.Unmarshal(frame.Payload, &payload); err == nil {
			fmt.Fprintf(s.stderr, "bdgw: %s: %s\n", payload.Code, payload.Message)
		}
	case bdgateway.FrameExit:
		// An exit frame whose payload is not a status leaves the call without
		// one, which execCall refuses. The HTTP handler reads a malformed exit
		// frame the same way.
		if status, err := frame.ExitStatus(); err == nil {
			s.status, s.exited = status, true
		}
	}
	return len(p), nil
}

// Started reports whether any frame arrived, which is whether bd started.
func (s *streamSink) Started() bool { return s.started }

// Status returns the status the exit frame carried, and whether one arrived.
// The executor writes one once it has started bd, but a stream the caller
// stopped reading ends before the frame can be written.
func (s *streamSink) Status() (int, bool) { return s.status, s.exited }
