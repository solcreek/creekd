// Package dispatch is the HTTP routing layer that maps incoming
// requests to the correct child process by X-Creek-App header (or
// ?app= query fallback) and forwards via reverse proxy.
//
// The reverse proxy is net/http/httputil.ReverseProxy from the
// standard library — one per backend, constructed at route-add
// time and reused for the route's lifetime. No external dependency
// (no Caddy, no nginx, no envoy).
//
// The dispatch table is updated by deploy events:
//
//   - new app:    Set inserts an entry.
//   - undeploy:   Remove drops an entry.
//   - blue-green: Set on an existing app id atomically swaps the backend.
//     In-flight requests on the old backend drain naturally because
//     each Backend keeps its own *ReverseProxy.
//
// Per-app health is tracked by the supervisor, not dispatch — a
// failing /healthz signals the supervisor to restart the child,
// after which the dispatch route is unchanged (same app id, same
// port). Dispatch does not gate traffic on health.
package dispatch
