package dispatch

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
)

// HeaderAppID is the request header creekd reads to decide which
// supervised app the request is for. ?app=<id> is accepted as a
// fallback for cases where the caller cannot set headers (curl
// shortcuts, browser address bar).
const HeaderAppID = "X-Creek-App"

// QueryAppID is the query-string alias for HeaderAppID.
const QueryAppID = "app"

// DefaultHost is used by Set when the caller doesn't supply a host
// explicitly. Matches the historical behaviour of the router (apps
// listening on the host's loopback interface).
const DefaultHost = "127.0.0.1"

// Backend is one routable destination: the target URL of an app's
// process at <Host>:<Port>, plus a memoised reverse proxy. Backends
// are immutable once created; route updates produce a new Backend.
type Backend struct {
	AppID string
	Host  string
	Port  int
	URL   *url.URL
	proxy *httputil.ReverseProxy
}

// newBackend constructs a Backend for appID listening on host:port.
// host == "" defaults to DefaultHost.
func newBackend(appID, host string, port int) (*Backend, error) {
	if appID == "" {
		return nil, errors.New("dispatch: empty appID")
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("dispatch: invalid port %d", port)
	}
	if host == "" {
		host = DefaultHost
	}
	u, err := url.Parse(fmt.Sprintf("http://%s:%d", host, port))
	if err != nil {
		return nil, fmt.Errorf("dispatch: parse backend url: %w", err)
	}
	b := &Backend{
		AppID: appID,
		Host:  host,
		Port:  port,
		URL:   u,
		proxy: httputil.NewSingleHostReverseProxy(u),
	}
	// Set the ErrorHandler exactly once at construction. Mutating it
	// per request (the prior approach) is a data race when multiple
	// goroutines proxy through the same backend — even if the closure
	// content is identical, the Go memory model treats concurrent
	// writes to a shared field as a race.
	id := appID
	b.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "dispatch: upstream unavailable for app "+id, http.StatusBadGateway)
	}
	return b, nil
}

// Router holds the appID → Backend table. Set/Remove are atomic; Get
// is lock-free for the common read path via RWMutex. The same Router
// can be safely shared by many concurrent goroutines.
type Router struct {
	mu     sync.RWMutex
	routes map[string]*Backend
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{routes: make(map[string]*Backend)}
}

// Set installs (or atomically replaces) the route for appID using
// DefaultHost (127.0.0.1) as the backend host. Shorthand for
// SetAddr(appID, "", port).
//
// Set is the blue-green flip primitive: once the v2 process is
// healthy, the caller invokes Set(appID, v2Port) and the swap is live.
func (r *Router) Set(appID string, port int) error {
	return r.SetAddr(appID, "", port)
}

// SetAddr installs (or atomically replaces) the route for appID
// pointing at host:port. Used by callers (notably the supervisor)
// that need to route to a non-loopback address — e.g. an app's
// container IP inside a network namespace. host == "" falls back to
// DefaultHost.
func (r *Router) SetAddr(appID, host string, port int) error {
	b, err := newBackend(appID, host, port)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.routes[appID] = b
	r.mu.Unlock()
	return nil
}

// Remove drops the route for appID. Subsequent requests for this app
// receive 503. Returns true if a route existed.
func (r *Router) Remove(appID string) bool {
	r.mu.Lock()
	_, existed := r.routes[appID]
	delete(r.routes, appID)
	r.mu.Unlock()
	return existed
}

// Get returns the current backend for appID or nil.
func (r *Router) Get(appID string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.routes[appID]
}

// Snapshot returns a copy of the appID → port mapping. Order is stable
// (alphabetical) so callers can render it deterministically.
func (r *Router) Snapshot() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.routes))
	for id, b := range r.routes {
		out[id] = b.Port
	}
	return out
}

// IDs returns the registered appIDs in sorted order.
func (r *Router) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.routes))
	for id := range r.routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AppIDFromRequest extracts the target app ID from req using the
// X-Creek-App header first, then the ?app= query fallback. Returns
// "" if neither is present.
func AppIDFromRequest(req *http.Request) string {
	if v := req.Header.Get(HeaderAppID); v != "" {
		return v
	}
	return req.URL.Query().Get(QueryAppID)
}

// ServeHTTP implements http.Handler: pick the backend by header/query
// and proxy. Errors are surfaced with descriptive HTTP status codes:
//
//   - 400 if no app id is supplied
//   - 503 if the app id is not registered
//   - 502 if the upstream proxy reports an error (handled by
//     httputil.ReverseProxy's ErrorHandler, set per-Backend)
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id := AppIDFromRequest(req)
	if id == "" {
		http.Error(w, "dispatch: missing "+HeaderAppID+" header or ?app= query", http.StatusBadRequest)
		return
	}
	b := r.Get(id)
	if b == nil {
		http.Error(w, "dispatch: no route for app "+id, http.StatusServiceUnavailable)
		return
	}
	// ErrorHandler is set once in newBackend; safe to read here.
	b.proxy.ServeHTTP(w, req)
}

// Handler returns r as an http.Handler. Useful when callers want the
// Router-as-handler explicit at the call site.
func (r *Router) Handler() http.Handler { return r }
