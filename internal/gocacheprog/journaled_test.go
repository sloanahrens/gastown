package gocacheprog

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

var errOrderJournal = errors.New("journal full")

type orderLog struct{ calls []string }

func (l *orderLog) add(call string) { l.calls = append(l.calls, call) }

type orderJournal struct {
	*orderLog
	err error
}

func (f orderJournal) Use(id []byte) error { f.add("journal use " + string(id)); return f.err }

func (f orderJournal) Put(id, out []byte, size int64) error {
	f.add(fmt.Sprintf("journal put %s %s %d", id, out, size))
	return f.err
}

// orderStore misses every get, so a get's use is the only thing it can record.
type orderStore struct{ *orderLog }

func (f orderStore) Get(id []byte) (Entry, error) {
	f.add("store get " + string(id))
	return Entry{}, ErrMiss
}

func (f orderStore) Put(id, out, body []byte) (string, error) {
	f.add(fmt.Sprintf("store put %s %s %d", id, out, len(body)))
	return "/store/" + string(out), nil
}

func checkCalls(t *testing.T, log *orderLog, want ...string) {
	if !reflect.DeepEqual(log.calls, want) {
		t.Errorf("calls = %v, want %v", log.calls, want)
	}
}

func TestJournaledGetRecordsUseBeforeRead(t *testing.T) {
	t.Parallel()
	log := &orderLog{}
	if _, err := NewJournaled(orderStore{log}, orderJournal{log, nil}).Get([]byte("a1")); !errors.Is(err, ErrMiss) {
		t.Fatalf("Get err = %v, want ErrMiss", err)
	}
	checkCalls(t, log, "journal use a1", "store get a1")
}

func TestJournaledGetUseFailureIsMissWithError(t *testing.T) {
	t.Parallel()
	log := &orderLog{}
	if _, err := NewJournaled(orderStore{log}, orderJournal{log, errOrderJournal}).Get([]byte("a1")); !errors.Is(err, ErrMiss) || !errors.Is(err, errOrderJournal) {
		t.Fatalf("Get err = %v, want a miss carrying %v", err, errOrderJournal)
	}
	checkCalls(t, log, "journal use a1") // the store is not read
}

func TestJournaledPutRecordsBeforeWrite(t *testing.T) {
	t.Parallel()
	log := &orderLog{}
	if _, err := NewJournaled(orderStore{log}, orderJournal{log, nil}).Put([]byte("a1"), []byte("beef"), []byte("hello")); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, log, "journal put a1 beef 5", "store put a1 beef 5")
}

func TestJournaledPutRecordFailureWritesNoFile(t *testing.T) {
	t.Parallel()
	log := &orderLog{}
	if path, err := NewJournaled(orderStore{log}, orderJournal{log, errOrderJournal}).Put([]byte("a1"), []byte("beef"), []byte("hello")); !errors.Is(err, errOrderJournal) || path != "" {
		t.Fatalf("Put = %q, %v; want a refused put carrying %v", path, err, errOrderJournal)
	}
	checkCalls(t, log, "journal put a1 beef 5") // the store, which writes the files, is not called
}
