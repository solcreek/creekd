package adminapi

import (
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/supervisor"
)

// findCondition is a tiny test helper — the conditions slice is
// short (4 items, fixed order) so linear scan beats anything more
// clever and keeps assertions readable.
func findCondition(conds []apitypes.Condition, typ apitypes.ConditionType) apitypes.Condition {
	for _, c := range conds {
		if c.Type == typ {
			return c
		}
	}
	return apitypes.Condition{}
}

// TestConditions_ReturnsAllFourTypesInCanonicalOrder covers the
// wire contract: every GET emits exactly four conditions, in the
// design's documented order. Clients are entitled to depend on
// conds[0]=Ready, conds[1]=Progressing, etc. — that's part of the
// stable v1alpha1 shape.
func TestConditions_ReturnsAllFourTypesInCanonicalOrder(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, time.Now())
	if len(conds) != 4 {
		t.Fatalf("conditions count = %d, want 4", len(conds))
	}
	want := []apitypes.ConditionType{
		apitypes.ConditionTypeReady,
		apitypes.ConditionTypeProgressing,
		apitypes.ConditionTypeDegraded,
		apitypes.ConditionTypeBackupReady,
	}
	for i, w := range want {
		if conds[i].Type != w {
			t.Errorf("conds[%d].Type = %q, want %q", i, conds[i].Type, w)
		}
	}
}

// TestConditions_RunningProducesReadyTrue covers the happy-path
// mapping. supervisor.StatusRunning → Ready=True, Progressing=False,
// Degraded=False. BackupReady is always Unknown in 0.0.x.
func TestConditions_RunningProducesReadyTrue(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, time.Now())

	if c := findCondition(conds, apitypes.ConditionTypeReady); c.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Ready.Status = %q, want True (app is Running)", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeProgressing); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Progressing.Status = %q, want False", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeDegraded); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Degraded.Status = %q, want False", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeBackupReady); c.Status != apitypes.ConditionStatusUnknown {
		t.Errorf("BackupReady.Status = %q, want Unknown (Tier 1/drill ships in 0.1.0)", c.Status)
	}
}

// TestConditions_StartingFlipsProgressing covers the deploy-in-flight
// signal at the supervisor layer (Status=Starting).
func TestConditions_StartingFlipsProgressing(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusStarting, 1, 1, time.Now())
	if c := findCondition(conds, apitypes.ConditionTypeProgressing); c.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Progressing.Status = %q, want True (app is Starting)", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeReady); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Ready.Status = %q, want False during Starting", c.Status)
	}
}

// TestConditions_ObservedGenLagAlsoFlipsProgressing covers the K8s
// convention: when spec mutation has landed but the deploy flow
// hasn't converged on the new revision (observedGen < gen),
// Progressing flips True even if the supervisor reports Running.
// This is the door for #10's async observedGeneration writer.
func TestConditions_ObservedGenLagAlsoFlipsProgressing(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusRunning, 5, 4, time.Now())
	prog := findCondition(conds, apitypes.ConditionTypeProgressing)
	if prog.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Progressing.Status = %q, want True (observedGen 4 < gen 5)", prog.Status)
	}
	if prog.Reason != "DeployInFlight" {
		t.Errorf("Progressing.Reason = %q, want DeployInFlight", prog.Reason)
	}
}

func TestConditions_CrashedFlipsDegraded(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusCrashed, 1, 1, time.Now())
	if c := findCondition(conds, apitypes.ConditionTypeDegraded); c.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Degraded.Status = %q, want True for Crashed", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeReady); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Ready.Status = %q, want False for Crashed", c.Status)
	}
}

// TestConditions_CrashLoopingFlipsDegradedWithReason proves that a
// machine-readable Reason distinguishes CrashLooping from one-off
// Crashed — clients filtering "is this app permanently broken vs
// just bouncing" can branch on reason.
func TestConditions_CrashLoopingFlipsDegradedWithReason(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusCrashLooping, 1, 1, time.Now())
	deg := findCondition(conds, apitypes.ConditionTypeDegraded)
	if deg.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Degraded.Status = %q, want True", deg.Status)
	}
	if deg.Reason != "CrashLooping" {
		t.Errorf("Degraded.Reason = %q, want CrashLooping (distinguishes from Crashed)", deg.Reason)
	}
}

func TestConditions_UnhealthyFlipsDegraded(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusUnhealthy, 1, 1, time.Now())
	if c := findCondition(conds, apitypes.ConditionTypeDegraded); c.Status != apitypes.ConditionStatusTrue {
		t.Errorf("Degraded.Status = %q, want True for Unhealthy", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeReady); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Ready.Status = %q, want False for Unhealthy", c.Status)
	}
}

// TestConditions_StoppedDoesNotMarkDegraded covers the semantic
// distinction: a stopped app is not READY, but it's not DEGRADED
// either — it's at rest by design.
func TestConditions_StoppedDoesNotMarkDegraded(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusStopped, 1, 1, time.Now())
	if c := findCondition(conds, apitypes.ConditionTypeReady); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Ready.Status = %q, want False (stopped)", c.Status)
	}
	if c := findCondition(conds, apitypes.ConditionTypeDegraded); c.Status != apitypes.ConditionStatusFalse {
		t.Errorf("Degraded.Status = %q, want False (intentional stop, not failure)", c.Status)
	}
}

// TestConditions_UnknownStatusYieldsUnknown covers the
// defensive default: supervisor.StatusUnknown (zero value) must
// not surface as Ready=False — it's not a confirmed failure, it's
// a missing observation.
func TestConditions_UnknownStatusYieldsUnknown(t *testing.T) {
	tr := newConditionTracker()
	conds := tr.computeAppConditions("a", supervisor.StatusUnknown, 1, 1, time.Now())
	for _, typ := range []apitypes.ConditionType{
		apitypes.ConditionTypeReady,
		apitypes.ConditionTypeProgressing,
		apitypes.ConditionTypeDegraded,
	} {
		c := findCondition(conds, typ)
		if c.Status != apitypes.ConditionStatusUnknown {
			t.Errorf("%s.Status = %q, want Unknown for StatusUnknown", typ, c.Status)
		}
	}
}

// TestConditions_LTTStableWhenStateUnchanged covers the LTT
// semantic: lastTransitionTime moves ONLY when the (type, status)
// pair flips. Repeated GETs against an unchanging supervisor must
// return the SAME LTT value, even though the call happens later.
func TestConditions_LTTStableWhenStateUnchanged(t *testing.T) {
	tr := newConditionTracker()
	t1 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)

	c1 := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t1)
	c2 := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t2)

	r1 := findCondition(c1, apitypes.ConditionTypeReady)
	r2 := findCondition(c2, apitypes.ConditionTypeReady)
	if !r1.LastTransitionTime.Equal(r2.LastTransitionTime) {
		t.Errorf("LTT moved on unchanged state: r1=%v r2=%v (want equal)", r1.LastTransitionTime, r2.LastTransitionTime)
	}
	if !r1.LastTransitionTime.Equal(t1) {
		t.Errorf("first-observation LTT = %v, want %v (the time of first call)", r1.LastTransitionTime, t1)
	}
}

// TestConditions_LTTAdvancesOnFlip covers the inverse: when Status
// actually flips, LTT must advance to the moment of flip.
func TestConditions_LTTAdvancesOnFlip(t *testing.T) {
	tr := newConditionTracker()
	t1 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)

	tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t1)
	// Now flip to Crashed at t2.
	c2 := tr.computeAppConditions("a", supervisor.StatusCrashed, 1, 1, t2)

	deg := findCondition(c2, apitypes.ConditionTypeDegraded)
	if deg.Status != apitypes.ConditionStatusTrue {
		t.Fatalf("Degraded.Status = %q, want True after flip to Crashed", deg.Status)
	}
	if !deg.LastTransitionTime.Equal(t2) {
		t.Errorf("Degraded.LTT = %v, want %v (the moment of flip)", deg.LastTransitionTime, t2)
	}

	// Ready also flipped True→False; its LTT must also advance.
	r := findCondition(c2, apitypes.ConditionTypeReady)
	if !r.LastTransitionTime.Equal(t2) {
		t.Errorf("Ready.LTT = %v, want %v (flipped at t2)", r.LastTransitionTime, t2)
	}
}

// TestConditions_ForgetResetsLTT covers the lifecycle hook:
// StopApp calls forget(id), which means a re-spawn with the same
// name starts with fresh LTT (it's a new resource lineage from the
// tracker's perspective).
func TestConditions_ForgetResetsLTT(t *testing.T) {
	tr := newConditionTracker()
	t1 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)

	tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t1)
	tr.forget("a")
	c2 := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t2)
	r := findCondition(c2, apitypes.ConditionTypeReady)
	if !r.LastTransitionTime.Equal(t2) {
		t.Errorf("Ready.LTT after forget+re-observe = %v, want %v (fresh first observation)", r.LastTransitionTime, t2)
	}
}

// TestConditions_PerAppLTTIsolation covers tracking isolation: two
// apps with independent state mustn't share LTT — a flip on app B
// doesn't disturb app A's recorded LTT.
func TestConditions_PerAppLTTIsolation(t *testing.T) {
	tr := newConditionTracker()
	t1 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)

	tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t1)
	tr.computeAppConditions("b", supervisor.StatusCrashed, 1, 1, t2)
	// Re-observe A at t2; nothing about B should leak in.
	cA := tr.computeAppConditions("a", supervisor.StatusRunning, 1, 1, t2)
	rA := findCondition(cA, apitypes.ConditionTypeReady)
	if !rA.LastTransitionTime.Equal(t1) {
		t.Errorf("app a's Ready LTT = %v, want %v (app b's flip must not bleed in)", rA.LastTransitionTime, t1)
	}
}
