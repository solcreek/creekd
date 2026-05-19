// Package state persists the supervisor's declared app set so creekd
// can survive its own restart. The Store is a JSON file plus a tiny
// in-memory cache, serialised by a mutex. Every admin mutation goes
// through it; startup reads it back and re-spawns each declared app.
//
// What persists: the configuration each app was spawned with —
// command, args, env, port, runtime, cgroup limits, sandbox spec,
// net isolation flag. What does NOT persist: ephemeral state (PIDs,
// uptime, restart counters, allocated IPs) — those are re-derived
// on restore.
//
// Atomicity: writes go to <path>.tmp, then rename(2) onto <path>.
// Corrupt half-writes therefore can't exist; either the file is the
// previous version or the new one.
//
// Concurrency: every method serialises through Store.mu. Admin-API
// callers don't need their own coordination beyond invoking the
// store from inside their handler.
package state
