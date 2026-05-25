package state

import (
	"sort"
	"sync"
)

// LockManager hands out per-app RWMutexes keyed by app ID. Mutations
// touching a single app serialize against the same lock; mutations
// touching different apps proceed in parallel. Cross-app operations
// (rename collision check, list-then-mutate, restore) call LockMany
// which acquires the locks in lex-sorted order to prevent deadlock.
//
// Per DESIGN-self-host-state.md §"Mutex granularity". The previous
// single global admin-API mutex is intended to disappear once every
// caller is converted.
//
// Locks are created on demand and never freed — at the v0.0.x scale
// (≤ 500 apps per host, per AppList cap) cumulative footprint stays
// under ~30KB even with churn. Eviction is a Phase 1.5 concern if
// measurable on real workloads.
type LockManager struct {
	locks sync.Map // string → *sync.RWMutex
}

// NewLockManager returns a ready manager.
func NewLockManager() *LockManager {
	return &LockManager{}
}

// AppLock returns the RWMutex for id. The same id always returns the
// same *RWMutex; concurrent first-time callers race-free via
// sync.Map.LoadOrStore. Caller chooses Lock / RLock based on whether
// they're mutating or reading.
func (m *LockManager) AppLock(id string) *sync.RWMutex {
	if v, ok := m.locks.Load(id); ok {
		return v.(*sync.RWMutex)
	}
	fresh := &sync.RWMutex{}
	actual, _ := m.locks.LoadOrStore(id, fresh)
	return actual.(*sync.RWMutex)
}

// LockMany acquires write locks for every id in ids — in
// lex-sorted, deduplicated order — and returns a release function
// that drops them in reverse order. Used by cross-app operations
// like rename collision check or restore that must hold multiple
// apps' write locks simultaneously.
//
// Lex order is the convention that makes deadlock impossible: two
// goroutines that both need locks for {"a","b"} will both acquire
// "a" first, then "b". No cycle possible.
func (m *LockManager) LockMany(ids ...string) func() {
	if len(ids) == 0 {
		return func() {}
	}
	sorted := dedupSorted(sortedCopy(ids))
	locks := make([]*sync.RWMutex, len(sorted))
	for i, id := range sorted {
		locks[i] = m.AppLock(id)
		locks[i].Lock()
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func dedupSorted(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
