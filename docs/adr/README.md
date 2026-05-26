# Architecture Decision Records (ADR)

ADRs document architecture / scope decisions that need to outlive a
single PR description. They're referenced from:

- **`.github/PULL_REQUEST_TEMPLATE.md`** — API-breaking PRs must
  link an accepted ADR before they can merge.
- **`CONTRIBUTING.md`** — proposals that contradict a documented
  non-goal must arrive as an ADR, not an issue.
- **`NON-GOALS.md`** — reopening a non-goal requires an ADR
  demonstrating the reasoning has materially changed.

## When you need one

You probably need an ADR if any of these hold:

- You're changing the wire-format contract (`api/openapi.yaml`)
  in a breaking way.
- You're revisiting a row in `NON-GOALS.md`.
- The decision will be cited months later by someone asking "why
  is it like this?"

You probably don't need one for ordinary feature work, refactors,
or bug fixes — the commit message + PR description carry that
context.

## Format

Save the file as `NNNN-short-kebab-title.md` where `NNNN` is the
next monotonic integer, zero-padded to 4 digits. Inside:

```markdown
# ADR-NNNN: Short title

- Status: proposed | accepted | superseded by ADR-MMMM
- Date: YYYY-MM-DD
- Authors: @github-handle

## Context

The problem and the constraints. Why is a decision needed?

## Decision

What did we decide? State it directly.

## Consequences

What does this enable, preclude, or require? Note the trade-offs
and any follow-up work.
```

Status transitions are recorded by appending a status line (don't
edit the original Status field, leave a trail).

## Current ADRs

(None yet — Phase 1 architectural decisions live in
[`DESIGN.md`](../DESIGN.md), [`ROADMAP.md`](../ROADMAP.md), and
[`NON-GOALS.md`](../../NON-GOALS.md). The ADR process kicks in
when those documents become too dense for inline reasoning.)
