package hardening

import (
	"bufio"
	"fmt"
	"strings"
)

// Drift describes one directive that's missing or weakened from
// the canonical hardening set.
type Drift struct {
	// Key is the directive name as it appears in the unit file
	// (e.g. "NoNewPrivileges").
	Key string
	// Want is the expected value per DESIGN.
	Want string
	// Got is the on-disk value. Empty string means the directive
	// is absent entirely.
	Got string
	// Reason summarises the failure mode in one line: "missing",
	// "weakened", or a more specific note for the few directives
	// with non-trivial comparison rules.
	Reason string
}

func (d Drift) String() string {
	if d.Got == "" {
		return fmt.Sprintf("%s: %s (want %q)", d.Key, d.Reason, d.Want)
	}
	return fmt.Sprintf("%s: %s (got %q, want %q)", d.Key, d.Reason, d.Got, d.Want)
}

// RequiredDirectives returns the canonical hardening set per
// DESIGN-self-host-state.md §"creekd privilege model & systemd
// hardening". Keys are directive names; values are the expected
// values. Order is the order the validator reports drift in.
//
// Returning a fresh slice (not a global) keeps the canonical set
// immutable from a caller's perspective.
func RequiredDirectives() []Required {
	return []Required{
		{"NoNewPrivileges", "true", matchExact},
		{"ProtectSystem", "strict", matchExact},
		{"ProtectHome", "true", matchExact},
		{"PrivateTmp", "true", matchExact},
		{"PrivateDevices", "true", matchExact},
		{"ProtectKernelTunables", "true", matchExact},
		{"ProtectKernelModules", "true", matchExact},
		{"ProtectKernelLogs", "true", matchExact},
		{"ProtectControlGroups", "true", matchExact},
		{"ProtectClock", "true", matchExact},
		{"ProtectHostname", "true", matchExact},
		{"RestrictNamespaces", "true", matchExact},
		{"RestrictRealtime", "true", matchExact},
		{"RestrictSUIDSGID", "true", matchExact},
		{"LockPersonality", "true", matchExact},
		{"MemoryDenyWriteExecute", "true", matchExact},
		{"SystemCallArchitectures", "native", matchExact},
		{"SystemCallFilter", "@system-service ~@privileged ~@resources", matchSyscallFilter},
		{"CapabilityBoundingSet", "CAP_NET_BIND_SERVICE", matchExact},
		{"AmbientCapabilities", "CAP_NET_BIND_SERVICE", matchExact},
		{"LimitCORE", "0", matchExact},
		{"DynamicUser", "no", matchExact},
	}
}

// Required describes one expected directive with its comparison
// strategy.
type Required struct {
	Key   string
	Want  string
	Match MatchFunc
}

// MatchFunc reports whether got satisfies want. Exposed so a few
// directives that have multiple equivalent spellings (most notably
// SystemCallFilter, where the deny tokens can appear in any order)
// can declare their own comparison.
type MatchFunc func(want, got string) bool

// matchExact compares values byte-for-byte after trimming surrounding
// whitespace. Covers most boolean / single-value directives.
func matchExact(want, got string) bool {
	return strings.TrimSpace(want) == strings.TrimSpace(got)
}

// matchSyscallFilter is order-insensitive over the space-separated
// tokens. systemd accepts the same filter expressed with tokens
// shuffled; the validator should not flag that as drift.
func matchSyscallFilter(want, got string) bool {
	w := strings.Fields(want)
	g := strings.Fields(got)
	if len(w) != len(g) {
		return false
	}
	seen := make(map[string]bool, len(g))
	for _, t := range g {
		seen[t] = true
	}
	for _, t := range w {
		if !seen[t] {
			return false
		}
	}
	return true
}

// Validate parses unitContent (the bytes of a .service file or the
// output of `systemctl cat creekd.service`) and returns one Drift
// per missing or weakened required directive. Nil means the unit
// matches the canonical hardening set.
//
// Directives appearing OUTSIDE the [Service] section are ignored —
// a value of NoNewPrivileges under [Unit] would not be honoured by
// systemd, so the validator should not honour it either.
func Validate(unitContent string) []Drift {
	parsed := parseServiceSection(unitContent)
	var out []Drift
	for _, r := range RequiredDirectives() {
		got, ok := parsed[r.Key]
		if !ok {
			out = append(out, Drift{Key: r.Key, Want: r.Want, Reason: "missing"})
			continue
		}
		if !r.Match(r.Want, got) {
			out = append(out, Drift{Key: r.Key, Want: r.Want, Got: got, Reason: "weakened"})
		}
	}
	return out
}

// parseServiceSection walks the unit content line by line, tracks
// the active section header, and accumulates directive=value pairs
// from [Service]. systemd allows repeated keys (later wins for
// scalar directives); we follow that rule. Comments and blanks
// are skipped.
func parseServiceSection(unitContent string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(unitContent))
	section := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "Service" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		out[key] = val
	}
	return out
}
