// Package adminclient is a typed Go HTTP client for the creekd admin
// API. It re-uses the request / response structs defined in
// internal/adminapi so that schema drift between server and client
// surfaces at compile time rather than at runtime.
//
// The package is internal to keep the surface tight for Phase 1 —
// only cmd/creekctl consumes it. If/when an external consumer needs
// the client (CI tools, dashboards, custom automation), the contents
// move to a /pkg path with backwards-compatibility guarantees.
package adminclient
