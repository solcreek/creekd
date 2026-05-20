// Package runtime selects and configures the underlying JS/TS runtime
// (Bun, Node, or Deno) for each child application.
//
// # M5.4 — multi-runtime dispatch
//
// Auto-detection precedence (highest first):
//
//  1. deno.json or deno.jsonc   → Deno
//  2. bun.lockb                 → Bun
//  3. package.json with "bun"
//     in dev/dependencies       → Bun
//  4. package.json              → Node
//  5. nothing recognised        → ErrNoSignal
//
// Detect inspects file fingerprints only — it does not scan source
// for runtime-specific imports (e.g. "bun:sqlite"). Manifest files
// are sufficient signal for the deploy flow and avoid parsing
// TypeScript.
//
// Explicit selection via app config overrides detection.
package runtime
