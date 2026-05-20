// Package deploy implements zero-downtime blue-green deploys.
//
// # M5.7 — zero-downtime blue-green deploy
//
// Sequence:
//  1. Spawn v2 child on new port (e.g., v1 on 9001 → v2 on 9011)
//  2. Wait for v2 health probe to pass (timeout 30s)
//  3. Flip dispatch table: appID → v2 port
//  4. Send SIGTERM to v1
//  5. v1 graceful shutdown (drain) → exit
//  6. On v2 failure: rollback (dispatch stays on v1, kill v2)
//
// Acceptance: 100 concurrent requests during deploy → 0 dropped.
package deploy
