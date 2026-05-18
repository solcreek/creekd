// Package runtime selects and configures the underlying JS/TS runtime
// (Bun, Node, or Deno) for each child application.
//
// M5.4 — multi-runtime dispatch
//
// Auto-detection rules:
//   - package.json imports "bun:sqlite" → bun
//   - deno.json exists → deno
//   - otherwise → node
//
// Explicit selection via app config overrides detection.
package runtime
