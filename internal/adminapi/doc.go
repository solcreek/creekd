// Package adminapi exposes creekd's control plane over HTTP/JSON.
//
// Surface (v1):
//
//	POST   /v1/apps                 spawn a new app + register dispatch route
//	GET    /v1/apps                 list registered apps
//	GET    /v1/apps/{id}            single app status
//	DELETE /v1/apps/{id}            graceful stop + de-register
//	POST   /v1/apps/{id}/deploy     blue-green deploy a new version
//	POST   /v1/apps/{id}/reset      clear crash-loop and resume
//
// Authentication is a bearer token in the Authorization header. When
// the server is constructed with an empty token, authentication is
// disabled — appropriate for UNIX-socket listeners or local dev. The
// public data-plane lives in package dispatch; the admin listener is
// always a separate port (or socket) so that operator credentials
// never leak to user traffic.
package adminapi
