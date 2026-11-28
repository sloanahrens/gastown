package bdgateway

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stalledResultFrames struct {
	resultFrames
	closed <-chan struct{}
	bound  context.Context
}

func (f *stalledResultFrames) Write(p []byte) (int, error) {
	select {
	case <-f.closed:
		return f.resultFrames.Write(p)
	case <-f.bound.Done():
		return 0, f.bound.Err()
	}
}

func checkStdoutBytes(t *testing.T, frames *resultFrames, err error, want *int64) {
	t.Helper()
	if !frames.set || (err == nil) != (want != nil) {
		t.Fatalf("Exec error = %v and result set = %v, want a total of %v", err, frames.set, want != nil)
	}
	got := frames.result.StdoutBytes
	if (got == nil) != (want == nil) {
		t.Fatalf("stdout bytes present = %v, want present = %v", got != nil, want != nil)
	}
	if got != nil && *got != *want {
		t.Fatalf("stdout bytes = %d, want %d", *got, *want)
	}
}

func TestExecStdoutBytesChunks(t *testing.T) {
	t.Parallel()
	proc := newExitedFake(0, "", "an error line\n")
	proc.stdout = io.MultiReader(strings.NewReader("hello "), strings.NewReader("world"))
	var frames resultFrames
	err := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}}).Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &frames)
	eleven := int64(11)
	checkStdoutBytes(t, &frames, err, &eleven)
}

func TestExecStdoutBytesSilentChild(t *testing.T) {
	t.Parallel()
	var frames resultFrames
	err := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{newExitedFake(0, "", "")}}).Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &frames)
	checkStdoutBytes(t, &frames, err, new(int64))
}

func TestExecStdoutBytesNoChild(t *testing.T) {
	t.Parallel()
	var frames resultFrames
	err := newTestExecutor(t, testWorkspace(t), &fakeRunner{err: io.EOF}).Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &frames)
	checkStdoutBytes(t, &frames, err, nil)
}

func TestExecStdoutBytesGiveUpWhileWriting(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{})
	t.Cleanup(func() { close(closed) })
	frames := &stalledResultFrames{closed: closed, bound: waitCtx(t)}
	e := newTestExecutorGrace(t, testWorkspace(t), &fakeRunner{queue: []Process{newBlockingFake("output")}}, 50*time.Millisecond)
	err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", TimeoutMS: 20}, nil, frames)
	checkStdoutBytes(t, &frames.resultFrames, err, nil)
}

func TestExecStdoutBytesGiveUpIdle(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	proc := newBlockingFake("")
	proc.stdout = stubbornReader{release: release, ended: make(chan struct{}), n: new(atomic.Int32)}
	var frames resultFrames
	err := newTestExecutorGrace(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}}, 10*time.Millisecond).Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown", TimeoutMS: 20}, nil, &frames)
	checkStdoutBytes(t, &frames, err, nil)
}
