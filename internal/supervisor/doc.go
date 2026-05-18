// Package supervisor manages the lifecycle of child application
// processes: spawning, watching, restarting with backoff, health
// probing, and graceful shutdown.
//
// M5.1 — child-process spawn + basic supervision
// M5.2 — restart policy (exponential backoff, crash-loop detection)
// M5.3 — health probe + graceful shutdown
//
// This package is the core of creekd. It is the most critical component
// to get right: a bug here cascades into every other capability.
package supervisor
