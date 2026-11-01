package gsm

import (
	"context"
	"sync"
)

// fakeClient is Client in memory. It records every call's secret id, so a test
// can prove a refused call never reached it, and it can be told to fail.
type fakeClient struct {
	mu       sync.Mutex
	secrets  map[string]*fakeSecret
	calls    []string // "method secretID"
	failWith error    // returned by every call when set
	failOnly string   // when set with failWith, only this method fails
	// beforeAccess, when set, runs at the start of AccessVersion with the lock
	// released, so a test can change a version between the list and the read.
	beforeAccess func()
}

type fakeSecret struct {
	next     int64
	payloads map[int64][]byte
	states   map[int64]State
}

func newFakeClient() *fakeClient { return &fakeClient{secrets: map[string]*fakeSecret{}} }

func (f *fakeClient) record(method, id string) error {
	f.calls = append(f.calls, method+" "+id)
	if f.failOnly != "" && f.failOnly != method {
		return nil
	}
	return f.failWith
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeClient) AddVersion(_ context.Context, id string, payload []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("add", id); err != nil {
		return 0, err
	}
	s := f.secrets[id]
	if s == nil {
		s = &fakeSecret{payloads: map[int64][]byte{}, states: map[int64]State{}}
		f.secrets[id] = s
	}
	s.next++
	s.payloads[s.next] = append([]byte(nil), payload...)
	s.states[s.next] = Enabled
	return s.next, nil
}

func (f *fakeClient) version(id string, n int64) (*fakeSecret, error) {
	s := f.secrets[id]
	if s == nil || s.states[n] == 0 || s.states[n] == Destroyed {
		return nil, ErrClientNotFound
	}
	return s, nil
}

func (f *fakeClient) AccessVersion(_ context.Context, id string, n int64) ([]byte, error) {
	if f.beforeAccess != nil {
		f.beforeAccess()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("access", id); err != nil {
		return nil, err
	}
	s, err := f.version(id, n)
	if err != nil {
		return nil, err
	}
	if s.states[n] != Enabled {
		return nil, ErrClientNotFound
	}
	return append([]byte(nil), s.payloads[n]...), nil
}

func (f *fakeClient) DisableVersion(_ context.Context, id string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("disable", id); err != nil {
		return err
	}
	s, err := f.version(id, n)
	if err != nil {
		return err
	}
	s.states[n] = Disabled
	return nil
}

func (f *fakeClient) DestroyVersion(_ context.Context, id string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("destroy", id); err != nil {
		return err
	}
	s, err := f.version(id, n)
	if err != nil {
		return err
	}
	clear(s.payloads[n])
	delete(s.payloads, n)
	s.states[n] = Destroyed
	return nil
}

func (f *fakeClient) ListVersions(_ context.Context, id string) ([]Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("list", id); err != nil {
		return nil, err
	}
	s := f.secrets[id]
	if s == nil {
		return []Version{}, nil
	}
	out := make([]Version, 0, len(s.states))
	for n, st := range s.states {
		out = append(out, Version{Number: n, State: st})
	}
	return out, nil
}
