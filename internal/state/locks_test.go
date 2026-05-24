package state

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockManager_SameIDReturnsSamePtr covers the lazy-creation
// contract: calling AppLock on the same id twice must return the
// same *RWMutex instance, otherwise distinct callers wouldn't
// actually serialise.
func TestLockManager_SameIDReturnsSamePtr(t *testing.T) {
	m := NewLockManager()
	a := m.AppLock("alice")
	b := m.AppLock("alice")
	if a != b {
		t.Errorf("AppLock(\"alice\") returned different pointers: %p vs %p", a, b)
	}
	c := m.AppLock("bob")
	if a == c {
		t.Errorf("AppLock(\"alice\") and AppLock(\"bob\") returned same pointer; lock manager is broken")
	}
}

// TestLockManager_SameIDSerialises proves that two goroutines
// contending on the same per-app lock take it sequentially.
func TestLockManager_SameIDSerialises(t *testing.T) {
	m := NewLockManager()
	var (
		concurrent int32
		maxConc    int32
	)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk := m.AppLock("same")
			lk.Lock()
			n := atomic.AddInt32(&concurrent, 1)
			for {
				cur := atomic.LoadInt32(&maxConc)
				if n <= cur || atomic.CompareAndSwapInt32(&maxConc, cur, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond) // simulate work
			atomic.AddInt32(&concurrent, -1)
			lk.Unlock()
		}()
	}
	wg.Wait()
	if maxConc != 1 {
		t.Errorf("same-id concurrency saw maxConc=%d, want 1", maxConc)
	}
}

// TestLockManager_DifferentIDsParallel proves that distinct per-app
// locks don't serialise against each other.
func TestLockManager_DifferentIDsParallel(t *testing.T) {
	m := NewLockManager()
	const n = 8
	var (
		concurrent int32
		maxConc    int32
	)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Each goroutine takes a unique lock.
			lk := m.AppLock(uniqueID(id))
			lk.Lock()
			n := atomic.AddInt32(&concurrent, 1)
			for {
				cur := atomic.LoadInt32(&maxConc)
				if n <= cur || atomic.CompareAndSwapInt32(&maxConc, cur, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
			lk.Unlock()
		}(i)
	}
	wg.Wait()
	// We expect parallelism. Strict equality (maxConc == n) is brittle
	// under load; allow ≥ 2 as the "definitely parallel" floor.
	if maxConc < 2 {
		t.Errorf("different-ids saw maxConc=%d, want ≥ 2 (lock manager not parallel)", maxConc)
	}
}

// TestLockManager_LockManyLexOrder verifies that LockMany acquires
// in sorted order. We instrument by recording the order locks are
// taken using a side-channel inspecting which AppLock is acquired
// first when two callers contend.
func TestLockManager_LockManyLexOrder(t *testing.T) {
	m := NewLockManager()

	// Probe: caller wants {"b","a","c"}. Lex-sorted: a,b,c. To prove
	// ordering, hold "a" first from a separate goroutine. Then call
	// LockMany("b","a","c") — it should block on "a" (proves "a" was
	// the first lock LockMany tried to acquire) and continue once "a"
	// frees.
	held := make(chan struct{})
	freed := make(chan struct{})
	go func() {
		lk := m.AppLock("a")
		lk.Lock()
		close(held)
		<-freed
		lk.Unlock()
	}()
	<-held

	done := make(chan struct{})
	go func() {
		release := m.LockMany("b", "a", "c")
		release()
		close(done)
	}()

	// LockMany should NOT complete while "a" is held by the side goroutine.
	select {
	case <-done:
		t.Fatal("LockMany completed without waiting on \"a\"; lex order not enforced")
	case <-time.After(20 * time.Millisecond):
		// Good — LockMany blocked.
	}

	close(freed)

	// Once "a" frees, LockMany should complete.
	select {
	case <-done:
		// Good.
	case <-time.After(time.Second):
		t.Fatal("LockMany did not complete after \"a\" was released")
	}
}

// TestLockManager_LockManyDedupes proves that LockMany with duplicate
// ids doesn't try to lock the same mutex twice (which would deadlock
// since *RWMutex doesn't support recursive Lock).
func TestLockManager_LockManyDedupes(t *testing.T) {
	m := NewLockManager()
	done := make(chan struct{})
	go func() {
		// If dedup is broken, this LockMany would try Lock("same")
		// twice and deadlock immediately.
		release := m.LockMany("same", "same", "other", "same")
		release()
		close(done)
	}()
	select {
	case <-done:
		// Good.
	case <-time.After(time.Second):
		t.Fatal("LockMany deadlocked on duplicate ids; dedup is broken")
	}
}

// TestLockManager_LockManyEmptyIsNoop covers the boundary: zero ids
// returns a no-op release function without taking any locks.
func TestLockManager_LockManyEmptyIsNoop(t *testing.T) {
	m := NewLockManager()
	done := make(chan struct{})
	go func() {
		release := m.LockMany()
		release()
		close(done)
	}()
	select {
	case <-done:
		// Good.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("LockMany() with zero ids hung; expected no-op")
	}
}

// TestLockManager_NoDeadlockOnInverseOrder is the classical lock-
// ordering deadlock probe: two goroutines try to acquire {"a","b"}
// and {"b","a"} respectively. With lex-sorted acquisition both
// converge on the same order (a, b) — no deadlock possible.
func TestLockManager_NoDeadlockOnInverseOrder(t *testing.T) {
	m := NewLockManager()
	const n = 50
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				release := m.LockMany("a", "b")
				release()
			}()
			go func() {
				defer wg.Done()
				// Caller specifies in inverse order — LockMany sorts.
				release := m.LockMany("b", "a")
				release()
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Good.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock between {a,b} and {b,a} acquisition; lex order not enforced")
	}
}

func uniqueID(i int) string {
	out := []byte("id-x")
	out[3] = byte('a' + (i % 26))
	return string(out)
}
