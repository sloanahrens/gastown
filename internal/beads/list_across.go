package beads

import (
	"errors"
	"fmt"
	"sync"
)

// AcrossResult is the answer to a cross-rig list: the issues of each rig that
// answered, and why each rig that did not is missing. A rig is in one map or
// the other and never both, so a failed read cannot arrive as an empty rig.
type AcrossResult struct {
	Rigs   map[string][]*Issue
	Errors map[string]error
}

// Lister is the one read a cross-rig list asks of one rig's store.
type Lister interface {
	List(opts ListOptions) ([]*Issue, error)
}

// AcrossStore is the store of a rig whose database holds several rigs: it
// answers one list for all of them at once. The tracker holds every rig in one
// database and implements it (trackerclient.Client); a bd store does not,
// because a bd database is one rig.
type AcrossStore interface {
	// ListAcross returns the issues matching opts for each rig in rigs, keyed
	// by the rig named, a rig that matched nothing present and empty. A rig
	// the store does not hold refuses the call rather than answering with no
	// issues.
	ListAcross(rigs []string, opts ListOptions) (AcrossResult, error)
}

// RigsOpen opens the store that reads rig's issues. Opening asks the store for
// nothing, so a caller pays for it once per rig and each rig's read still
// starts on the first List.
type RigsOpen func(rig string) (Lister, error)

// AcrossRead is a caller's own read of one rig's store, for the rigs no
// AcrossStore answers. A caller whose reads must be attributed to itself passes
// one (the census in census_budgets.go is keyed by the caller's name). It asks
// for opts' list. nil reads l.List(opts).
type AcrossRead func(l Lister) ([]*Issue, error)

// errNoRigAnswer is a cross-rig store that left a rig out of its answer: a
// rig nobody read is not a rig with no issues.
var errNoRigAnswer = errors.New("beads: the cross-rig store did not answer for a rig it was asked for")

// ListAcross reads one list in every rig of rigs and keys the answers by rig.
//
// The rigs a store holding several of them answers (AcrossStore) cost one
// request between them; every other rig is read from its own store, parallel
// rigs at a time (at least one), so a hung store costs its rig and the others
// still answer. A rig whose open or read fails is in Errors with why and leaves
// the other rigs' answers intact. err is non-nil only when the call itself
// cannot be made.
func ListAcross(rigs []string, open RigsOpen, opts ListOptions, read AcrossRead, parallel int) (AcrossResult, error) {
	res := AcrossResult{Rigs: map[string][]*Issue{}, Errors: map[string]error{}}
	if open == nil {
		return res, fmt.Errorf("beads: ListAcross needs a store opener")
	}
	if opts.Rig != "" {
		return res, fmt.Errorf("beads: ListAcross scopes by the rig it lists in; ListOptions.Rig is a merge-request filter and is refused")
	}
	for _, rig := range rigs {
		if rig == "" {
			return res, fmt.Errorf("beads: ListAcross was given an empty rig name")
		}
	}
	var cross AcrossStore
	var crossRigs []string
	var own []ownStore
	seen := make(map[string]bool, len(rigs))
	for _, rig := range rigs {
		if seen[rig] {
			continue
		}
		seen[rig] = true
		l, err := open(rig)
		if err == nil && l == nil {
			err = fmt.Errorf("beads: the opener returned no store for %s", rig)
		}
		if err != nil {
			res.Errors[rig] = err
			continue
		}
		if a, ok := l.(AcrossStore); ok {
			// One process reaches one tracker (backend.Config comes from the
			// environment), so the first store that answers several rigs
			// answers for all of them. A rig the wrong server does not hold
			// comes back an error, not an empty answer.
			if cross == nil {
				cross = a
			}
			crossRigs = append(crossRigs, rig)
			continue
		}
		own = append(own, ownStore{rig: rig, l: l})
	}
	if cross != nil {
		listAcrossStore(res, cross, crossRigs, opts)
	}
	readOwn(res, own, opts, read, parallel)
	return res, nil
}

// listAcrossStore asks one store for every rig it answers and files the answer
// per rig. A rig the store left out is an error for that rig.
func listAcrossStore(res AcrossResult, cross AcrossStore, rigs []string, opts ListOptions) {
	got, err := cross.ListAcross(rigs, opts)
	if err != nil {
		for _, rig := range rigs {
			res.Errors[rig] = err
		}
		return
	}
	for _, rig := range rigs {
		if issues, ok := got.Rigs[rig]; ok {
			res.Rigs[rig] = issues
			continue
		}
		if e := got.Errors[rig]; e != nil {
			res.Errors[rig] = e
			continue
		}
		res.Errors[rig] = fmt.Errorf("%w: %s", errNoRigAnswer, rig)
	}
}

// ownStore is one rig that reads its own store.
type ownStore struct {
	rig string
	l   Lister
}

// readOwn reads each rig from its own store, parallel rigs at a time.
func readOwn(res AcrossResult, own []ownStore, opts ListOptions, read AcrossRead, parallel int) {
	if read == nil {
		read = func(l Lister) ([]*Issue, error) { return l.List(opts) }
	}
	if parallel < 1 {
		parallel = 1
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for _, s := range own {
		wg.Add(1)
		sem <- struct{}{}
		go func(s ownStore) {
			defer func() { <-sem; wg.Done() }()
			issues, err := read(s.l)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors[s.rig] = err
				return
			}
			res.Rigs[s.rig] = issues
		}(s)
	}
	wg.Wait()
}
