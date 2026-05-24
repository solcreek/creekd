package adminapi

import (
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/supervisor"
)

// TestConditions_ProgressingTimeoutFlipsDegraded covers the
// DESIGN's "progressing_timeout" semantic: once Progressing has
// been True for longer than the configured timeout, the next
// computation MUST flip Progressing → False AND Degraded → True,
// both with reason=DeployTimeout. The CLI uses this to surface
// deploy_stuck on --watch polls.
func TestConditions_ProgressingTimeoutFlipsDegraded(t *testing.T) {
	tr := newConditionTracker()
	tr.progressingTimeout = 100 * time.Millisecond

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	// Generation moved to 2 but observedGen still 1 → Progressing=True.
	c1 := tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t0)
	if findCondition(c1, apitypes.ConditionTypeProgressing).Status != apitypes.ConditionStatusTrue {
		t.Fatal("initial state should have Progressing=True (observedGen lag)")
	}

	// Same observation 1 second later — well past the 100ms timeout.
	t1 := t0.Add(time.Second)
	c2 := tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t1)

	prog := findCondition(c2, apitypes.ConditionTypeProgressing)
	deg := findCondition(c2, apitypes.ConditionTypeDegraded)
	if prog.Status != apitypes.ConditionStatusFalse {
		t.Errorf("after timeout: Progressing.Status = %q, want False", prog.Status)
	}
	if prog.Reason != "DeployTimeout" {
		t.Errorf("after timeout: Progressing.Reason = %q, want DeployTimeout", prog.Reason)
	}
	if deg.Status != apitypes.ConditionStatusTrue {
		t.Errorf("after timeout: Degraded.Status = %q, want True", deg.Status)
	}
	if deg.Reason != "DeployTimeout" {
		t.Errorf("after timeout: Degraded.Reason = %q, want DeployTimeout", deg.Reason)
	}
}

// TestConditions_ProgressingWithinTimeoutStaysTrue covers the
// negative case: while Progressing is fresh (< timeout), it
// remains True and Degraded does NOT flip. Prevents false
// positives.
func TestConditions_ProgressingWithinTimeoutStaysTrue(t *testing.T) {
	tr := newConditionTracker()
	tr.progressingTimeout = 5 * time.Minute

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t0)
	// Two minutes later — well within the 5-minute window.
	t1 := t0.Add(2 * time.Minute)
	c2 := tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t1)

	if findCondition(c2, apitypes.ConditionTypeProgressing).Status != apitypes.ConditionStatusTrue {
		t.Error("within timeout: Progressing should remain True")
	}
	if findCondition(c2, apitypes.ConditionTypeDegraded).Status != apitypes.ConditionStatusFalse {
		t.Error("within timeout: Degraded should remain False")
	}
}

// TestConditions_ProgressingFalseClearsMonotonicAnchor covers the
// cycle-restart semantic: Progressing True → False → True must
// measure the second True window from its OWN start, not the
// first. Otherwise a fast deploy that immediately follows a slow
// one would be marked stuck despite barely beginning.
func TestConditions_ProgressingFalseClearsMonotonicAnchor(t *testing.T) {
	tr := newConditionTracker()
	tr.progressingTimeout = 100 * time.Millisecond

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t0) // Progressing=True at t0

	// 50ms later, deploy converges: observedGen=2, gen=2 →
	// Progressing=False.
	t1 := t0.Add(50 * time.Millisecond)
	tr.computeAppConditions("a", supervisor.StatusRunning, 2, 2, t1)

	// 200ms later (so total 250ms since t0, but only 200ms into
	// the SECOND True window), a new deploy starts: gen=3,
	// observedGen=2 → Progressing=True. The anchor must restart
	// here, not retain t0.
	t2 := t1.Add(200 * time.Millisecond)
	tr.computeAppConditions("a", supervisor.StatusRunning, 3, 2, t2)

	// 50ms later, still within the 100ms timeout from t2, the
	// anchor (if correctly cleared at t1) means we're 50ms in —
	// NOT timed out.
	t3 := t2.Add(50 * time.Millisecond)
	c := tr.computeAppConditions("a", supervisor.StatusRunning, 3, 2, t3)
	if findCondition(c, apitypes.ConditionTypeProgressing).Status != apitypes.ConditionStatusTrue {
		t.Error("second Progressing window: Progressing should still be True (anchor was cleared in between)")
	}
}

// TestConditions_ProgressingTimeoutPerApp covers tracker
// isolation: app A's stuck deploy must not poison app B's timer.
func TestConditions_ProgressingTimeoutPerApp(t *testing.T) {
	tr := newConditionTracker()
	tr.progressingTimeout = 100 * time.Millisecond

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t0)
	// 1 second later — a is timed out. Now b enters Progressing=True
	// for the first time.
	t1 := t0.Add(time.Second)
	c := tr.computeAppConditions("b", supervisor.StatusRunning, 2, 1, t1)
	if findCondition(c, apitypes.ConditionTypeProgressing).Status != apitypes.ConditionStatusTrue {
		t.Error("app b's first Progressing observation should be True (app a's stuck timer must not leak)")
	}
	if findCondition(c, apitypes.ConditionTypeDegraded).Status == apitypes.ConditionStatusTrue {
		t.Error("app b's Degraded should be False — its timer just started")
	}
}

// TestConditions_ForgetClearsProgressingAnchor covers the
// app-lifecycle hook: StopApp calls forget(id), which must drop
// the Progressing monotonic anchor too. A re-spawn under the same
// name starts with a fresh timer.
func TestConditions_ForgetClearsProgressingAnchor(t *testing.T) {
	tr := newConditionTracker()
	tr.progressingTimeout = 100 * time.Millisecond
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t0)
	tr.forget("a")
	// 1 second later, same app name observed Progressing again.
	t1 := t0.Add(time.Second)
	c := tr.computeAppConditions("a", supervisor.StatusRunning, 2, 1, t1)
	if findCondition(c, apitypes.ConditionTypeProgressing).Status != apitypes.ConditionStatusTrue {
		t.Error("post-forget re-observe: Progressing should be True (fresh timer)")
	}
	if findCondition(c, apitypes.ConditionTypeDegraded).Status == apitypes.ConditionStatusTrue {
		t.Error("post-forget re-observe: Degraded should be False (anchor was cleared)")
	}
}
