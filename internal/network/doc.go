// Package network builds the Linux primitives that let each supervised
// app live in its own network namespace with reachable connectivity.
//
// Phase 1 architecture:
//
//	host (creekd) ── creekd0 bridge ── veth-h0 ─┐
//	                                            │
//	                                            └── eth0 in netns(app-0)
//	                                                10.42.0.10/24
//	                                                default via 10.42.0.1
//
// Each app gets:
//   - a persistent netns at /var/run/netns/<appID>
//   - a veth pair: host side joins creekd0; container side enters the
//     netns and is renamed to "eth0"
//   - a unique /32 inside the bridge's /24 subnet
//   - a default route via the bridge's gateway IP
//
// Outbound: a single iptables MASQUERADE rule on the subnet covers
// every app. Inbound: dispatch.Router proxies on the host, hopping the
// bridge to reach the app's container IP.
//
// Implementation: shell out to iproute2's `ip` and to `iptables`. The
// kernel does the heavy lifting; this package just orchestrates the
// commands and tracks state (IP allocation, netns lifetime).
//
// Linux only. Non-Linux builds compile against a shim that returns
// ErrUnsupported on use, so the supervisor can depend on the package
// unconditionally on macOS dev hosts.
package network
