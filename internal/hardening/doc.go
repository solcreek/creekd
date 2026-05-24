// Package hardening validates that the running creekd systemd unit
// carries every hardening directive specified by
// DESIGN-self-host-state.md §"creekd privilege model & systemd
// hardening". `creek host doctor` (lands with #23 CLI) wraps the
// Validate function to surface drift as the `systemd_hardening_drift`
// error code so an operator can re-apply the shipped unit.
//
// The validator parses an INI-style systemd unit (the on-disk
// .service file at /etc/systemd/system/creekd.service) and reports
// each missing or weakened directive individually. It does NOT
// consult systemctl — that would couple test machines to a real
// init system. Operators verifying a LIVE host should pipe
// `systemctl cat creekd.service` into the validator if they want
// effective-config drift; for the simpler on-disk check, point at
// the unit file directly.
package hardening
