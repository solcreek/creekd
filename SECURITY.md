# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a suspected security vulnerability.** Public reports make the bug exploitable before there is a fix.

Report privately via GitHub's Private Vulnerability Reporting:

→ **[Open a private report](https://github.com/solcreek/creekd/security/advisories/new)**

This routes the report directly to the maintainers, keeps the discussion private, and lets us coordinate a fix and a disclosure timeline before the details become public.

If GitHub PVR is unavailable to you for any reason, you can also email the maintainer via the address on the [solcreek GitHub profile](https://github.com/solcreek). Please prefer the PVR flow — email gets lost.

## What to include

A useful report typically has:

- A description of the issue and the impact (what an attacker gets).
- The affected version (`creekd --version`) or commit SHA.
- A minimal reproducer — exact env vars, exact request, exact config that triggers it.
- Your assessment of severity, if you have one.

If you have a proposed patch, attach it; if not, that's fine.

## Scope

In scope:

- The `creekd` daemon and the `creekctl` CLI in this repository.
- The HTTP/JSON admin API surface and the dispatch routing surface.
- The supervisor's process isolation (cgroup, namespace, chroot, NoNewPrivs) — specifically, escapes from the documented sandbox into the host or into another app's sandbox.
- The state persistence layer (`state.json`) — specifically, integrity / injection / TOCTOU on read.

Out of scope:

- Bugs in apps that creekd happens to be supervising. Report those to the app's maintainers.
- Bugs in the underlying Linux kernel, cgroup v2, or Linux namespace primitives. Report those upstream.
- Denial of service that requires already-authenticated admin access — by design, an admin token holder can stop the daemon. The threat model assumes admin tokens are protected.
- Issues that require root on the host to exploit.

## What you can expect

- Acknowledgement within 7 days of the initial private report.
- A first assessment (confirmed / not-reproducible / out-of-scope) within 14 days.
- For confirmed issues: a patch on a target date that we agree on with you. We aim for ≤90 days from confirmation to public disclosure for high-severity issues.
- A credit in the release notes, if you want one. If you prefer to stay anonymous, say so.

## What we won't do

- We will not ask you to sign an NDA before reading your report.
- We will not threaten legal action against good-faith research.
- We will not silently fix and pretend nothing happened. Every confirmed issue gets a CVE-style entry in the release notes.
