package adminapi

import (
	"sync"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/supervisor"
)

// conditionTracker holds the per-app observed Condition tuple so
// the GET handler can report LastTransitionTime that reflects the
// real moment of the last status flip, not the moment of the
// request.
//
// The map is keyed by appID then ConditionType. It is process-
// local (NOT persisted into state.json in 0.0.x) — after restart,
// LTT values reset to the moment of the first GET per app. The
// DESIGN reserves the right to persist these into state.json
// in 0.1.0; until then the wire is correct but ephemeral.
type conditionTracker struct {
	mu sync.Mutex
	m  map[string]map[apitypes.ConditionType]apitypes.Condition
}

func newConditionTracker() *conditionTracker {
	return &conditionTracker{m: map[string]map[apitypes.ConditionType]apitypes.Condition{}}
}

// computeAppConditions returns the four-condition slice for one
// app at the current moment. The returned slice is sorted in the
// design's canonical order (Ready, Progressing, Degraded,
// BackupReady) so wire output is stable.
//
// The supervisor status is mapped to conditions as follows:
//
//	supervisor.StatusRunning      → Ready=True
//	supervisor.StatusStarting     → Progressing=True
//	supervisor.StatusCrashed      → Degraded=True reason=Crashed
//	supervisor.StatusCrashLooping → Degraded=True reason=CrashLooping
//	supervisor.StatusUnhealthy    → Degraded=True reason=Unhealthy
//	supervisor.StatusStopped      → Ready=False reason=Stopped
//
// BackupReady is always Unknown in 0.0.x — the Tier 1 restore drill
// machinery that decides True/False ships in 0.1.0. Reporting it
// as Unknown keeps the wire shape stable; CLIs MAY surface the
// "not yet configured" message via `reason=NotConfigured`.
//
// observedGeneration < generation flips Progressing→True even when
// the supervisor reports Running, mirroring the K8s convention
// "spec changed, haven't observed convergence yet". This pathway is
// in the design but is dormant in 0.0.x until #10 wires up the
// async observedGeneration writer.
func (t *conditionTracker) computeAppConditions(appID string, status supervisor.Status, generation, observedGeneration int64, now time.Time) []apitypes.Condition {
	ready := apitypes.Condition{Type: apitypes.ConditionTypeReady}
	prog := apitypes.Condition{Type: apitypes.ConditionTypeProgressing}
	deg := apitypes.Condition{Type: apitypes.ConditionTypeDegraded}
	backup := apitypes.Condition{Type: apitypes.ConditionTypeBackupReady}

	switch status {
	case supervisor.StatusRunning:
		ready.Status = apitypes.ConditionStatusTrue
		ready.Reason = "Healthy"
		prog.Status = apitypes.ConditionStatusFalse
		prog.Reason = "AtRest"
		deg.Status = apitypes.ConditionStatusFalse
		deg.Reason = "Stable"
	case supervisor.StatusStarting:
		ready.Status = apitypes.ConditionStatusFalse
		ready.Reason = "Starting"
		prog.Status = apitypes.ConditionStatusTrue
		prog.Reason = "Starting"
		deg.Status = apitypes.ConditionStatusFalse
		deg.Reason = "Stable"
	case supervisor.StatusCrashed:
		ready.Status = apitypes.ConditionStatusFalse
		ready.Reason = "Crashed"
		prog.Status = apitypes.ConditionStatusFalse
		prog.Reason = "AtRest"
		deg.Status = apitypes.ConditionStatusTrue
		deg.Reason = "Crashed"
	case supervisor.StatusCrashLooping:
		ready.Status = apitypes.ConditionStatusFalse
		ready.Reason = "CrashLooping"
		prog.Status = apitypes.ConditionStatusFalse
		prog.Reason = "AtRest"
		deg.Status = apitypes.ConditionStatusTrue
		deg.Reason = "CrashLooping"
	case supervisor.StatusUnhealthy:
		ready.Status = apitypes.ConditionStatusFalse
		ready.Reason = "ProbeFailing"
		prog.Status = apitypes.ConditionStatusFalse
		prog.Reason = "AtRest"
		deg.Status = apitypes.ConditionStatusTrue
		deg.Reason = "Unhealthy"
	case supervisor.StatusStopped:
		ready.Status = apitypes.ConditionStatusFalse
		ready.Reason = "Stopped"
		prog.Status = apitypes.ConditionStatusFalse
		prog.Reason = "AtRest"
		deg.Status = apitypes.ConditionStatusFalse
		deg.Reason = "Stable"
	default:
		ready.Status = apitypes.ConditionStatusUnknown
		ready.Reason = "Unknown"
		prog.Status = apitypes.ConditionStatusUnknown
		prog.Reason = "Unknown"
		deg.Status = apitypes.ConditionStatusUnknown
		deg.Reason = "Unknown"
	}

	// observedGeneration < generation forces Progressing=True even
	// when supervisor says Running: spec mutation has landed but
	// the deploy flow hasn't converged on the new revision yet.
	if observedGeneration < generation {
		prog.Status = apitypes.ConditionStatusTrue
		prog.Reason = "DeployInFlight"
	}

	// BackupReady — Tier 0 ships but the drill machinery + Tier 1
	// don't exist yet in 0.0.x. We can't truthfully say "True"
	// (no drill has run) or "False" (Tier 0 backups may be fine);
	// Unknown is the honest answer until 0.1.0.
	backup.Status = apitypes.ConditionStatusUnknown
	backup.Reason = "NotConfigured"

	out := []apitypes.Condition{ready, prog, deg, backup}
	t.applyTransitionTimes(appID, out, now)
	return out
}

// applyTransitionTimes overwrites c.LastTransitionTime on each
// returned Condition based on whether (type, status) flipped from
// the last observation. First observation = `now`; unchanged
// status = the previously recorded LTT; flipped status = `now`
// (a fresh transition).
func (t *conditionTracker) applyTransitionTimes(appID string, conds []apitypes.Condition, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cur, ok := t.m[appID]
	if !ok {
		cur = map[apitypes.ConditionType]apitypes.Condition{}
		t.m[appID] = cur
	}
	for i := range conds {
		c := &conds[i]
		prev, seen := cur[c.Type]
		if !seen || prev.Status != c.Status {
			c.LastTransitionTime = now
		} else {
			c.LastTransitionTime = prev.LastTransitionTime
		}
		// Always store the current observation so the next call's
		// "flipped?" check uses the latest snapshot.
		cur[c.Type] = *c
	}
}

// forget drops the per-app tracker entries on app delete so a
// future create with the same name starts fresh. Idempotent.
func (t *conditionTracker) forget(appID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, appID)
}
