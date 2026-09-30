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
	// Got is the on-disk value when the directive is present.
	// Distinguish "directive is absent" from "directive is present
	// but has an empty value" via Reason — Reason == "missing" means
	// the parser never saw the key, anything else means the value
	// was parsed (possibly to "").
	Got string
	// Reason summarises the failure mode in one line: "missing",
	// "weakened", or a more specific note for the few directives
	// with non-trivial comparison rules.
	Reason string
}

func (d Drift) String() string {
	if d.Reason == "missing" {
		return fmt.Sprintf("%s: %s (want %q)", d.Key, d.Reason, d.Want)
	}
	return fmt.Sprintf("%s: %s (got %q, want %q)", d.Key, d.Reason, d.Got, d.Want)
}

// RequiredDirectives returns the canonical hardening set. This
// function is itself the source-of-truth — callers should not
// reach for an external doc to learn what creekd's privilege model
// looks like, they should read this list. Keys are directive names;
// values are the expected values. Order is the order the validator
// reports drift in.
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
		// Three lines, in this order: allow @system-service, deny
		// @privileged and @resources, re-allow what per-app UIDs need.
		// The old one-line "@system-service ~@privileged ~@resources" is
		// an allow-list whose ~ tokens deny nothing @system-service
		// includes (setresuid, setpriority, chown, setrlimit, ...).
		{"SystemCallFilter", "@system-service\n~@privileged @resources\n@setuid capset", matchSyscallFilter},
		// A denied syscall fails with EPERM instead of killing the
		// process with SIGSYS, so an app that probes one degrades.
		{"SystemCallErrorNumber", "EPERM", matchExact},
		// CAP_SETUID/CAP_SETGID: per-app UIDs (see init/creekd.service).
		// A set, not a string: systemd accepts the names in any order.
		{"CapabilityBoundingSet", "CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID", matchPathSet},
		{"AmbientCapabilities", "CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID", matchPathSet},
		{"LimitCORE", "0", matchExact},
		{"DynamicUser", "no", matchExact},
		// ReadWritePaths is the escape hatch ProtectSystem=strict relies
		// on; both adding extra paths (broadening write surface) and
		// removing required paths (breaking runtime) count as drift, so
		// the matcher demands an exact set match.
		{"ReadWritePaths", "/var/lib/creekd /var/log/creekd", matchPathSet},
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

// matchSyscallFilter compares SystemCallFilter assignments line by
// line (one per "\n"). Line order matters — systemd applies each
// assignment in turn, so allow / deny / re-allow is not the same filter
// in another order — but tokens within a line are order-insensitive.
func matchSyscallFilter(want, got string) bool {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	if len(w) != len(g) {
		return false
	}
	for i := range w {
		wDeny, wList := filterLine(w[i])
		gDeny, gList := filterLine(g[i])
		if wDeny != gDeny || !matchTokenSet(wList, gList) {
			return false
		}
	}
	return true
}

// filterLine splits one SystemCallFilter assignment into its mode and
// its list: a leading "~" makes the whole line a deny-list; it is not
// part of the first token.
func filterLine(line string) (deny bool, list string) {
	line = strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(line, "~"); ok {
		return true, rest
	}
	return false, line
}

// CanonicalServiceSection renders RequiredDirectives as the directive
// lines of a unit's [Service] section, one assignment per line (a
// multi-line want becomes repeated assignments of the same key).
func CanonicalServiceSection() string {
	var b strings.Builder
	for _, r := range RequiredDirectives() {
		for _, v := range strings.Split(r.Want, "\n") {
			b.WriteString(r.Key + "=" + v + "\n")
		}
	}
	return b.String()
}

// matchTokenSet reports whether two space-separated lists hold the same
// tokens, in any order.
func matchTokenSet(want, got string) bool {
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

// multiAssign lists the directives systemd accumulates across repeated
// assignments (an empty assignment resets the list). Every other key
// keeps its last assignment.
var multiAssign = map[string]bool{"SystemCallFilter": true}

// matchPathSet is a strict set comparison: same paths, no extras,
// order-insensitive. Used for ReadWritePaths where extra entries
// silently widen the write surface and missing entries break runtime
// writes — both are drift the operator needs to see.
func matchPathSet(want, got string) bool {
	w := strings.Fields(want)
	g := strings.Fields(got)
	if len(w) != len(g) {
		return false
	}
	seen := make(map[string]bool, len(g))
	for _, p := range g {
		seen[p] = true
	}
	for _, p := range w {
		if !seen[p] {
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
func Validate(unitContent string) ([]Drift, error) {
	parsed, err := parseServiceSection(unitContent)
	if err != nil {
		return nil, err
	}
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
	return out, nil
}

// parseServiceSection walks the unit content line by line, tracks
// the active section header, and accumulates directive=value pairs
// from [Service]. systemd allows repeated keys (later wins for
// scalar directives); we follow that rule. Comments and blanks
// are skipped.
func parseServiceSection(unitContent string) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(unitContent))
	// systemd directive values can be long (e.g. SystemCallFilter with
	// many tokens) — bump the buffer past the default 64KB so the
	// scanner doesn't silently truncate a directive and make the
	// validator report bogus drift.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		switch {
		case !multiAssign[key]:
			out[key] = val
		case val == "":
			delete(out, key) // systemd: an empty assignment resets the list
		case out[key] == "":
			out[key] = val
		default:
			out[key] += "\n" + val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("hardening: parse unit: %w", err)
	}
	return out, nil
}
