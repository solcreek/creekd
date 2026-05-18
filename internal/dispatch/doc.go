// Package dispatch is the HTTP routing layer that maps incoming
// requests to the correct child process by X-Creek-App header (or
// ?app= query fallback) and forwards via reverse proxy.
//
// Phase 1 implementation likely embeds Caddy v2 as a library for the
// reverse-proxy + TLS layer, with creekd providing the upstream table.
//
// The dispatch table is updated by deploy events:
//   - new app:    add entry
//   - undeploy:   remove entry
//   - blue-green: atomic swap of port for an existing entry
//   - unhealthy:  temporarily withhold from dispatch until healthy again
package dispatch
