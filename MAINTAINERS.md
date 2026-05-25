# Maintainers

This document names current maintainers and describes the
bus-factor mitigation that operates while the project has fewer
than two named maintainers.

## Active maintainers

| Name      | GitHub        | Areas |
|-----------|---------------|-------|
| Lawrence  | @linyiru      | all   |

## Co-maintainer recruitment status

**Open**, blocking `0.1.0` first public alpha. No public alpha
ships until at least one co-maintainer is named. `0.0.x` releases
are explicitly solo-by-design (Lawrence dogfood line, no public
install path); single-maintainer is internally consistent at this
stage.

If you would like to discuss co-maintainership, open an issue
tagged `governance/co-maintainer` or contact Lawrence directly.

## Bus-factor mitigation

While the project has only one named maintainer, the following
three commitments are in force:

### 1. Encrypted recovery escrow

A private GitHub gist at `solcreek/creekd-recovery` is
maintained, accessible to a paper-only off-site escrow. The
escrow contains:

- GitHub organisation admin transfer instructions, in plain
  language for a non-technical executor.
- sigstore signing-key recovery procedure for the release
  pipeline.
- npm namespace transfer procedure for any companion CLI
  packages.

The paper escrow is kept at off-site secure storage. The exact
location is held by the escrow holder, not in this document.
The escrow is verified annually and re-issued whenever any
recovery procedure changes.

### 2. Apache 2.0 public pledge

> If the named maintainer becomes unreachable for ≥90 days, the
> project is freely forkable under Apache 2.0; the "creek"
> trademark (if any) is released to public domain at that point.
> Any subsequent fork that publishes a release using "creek" in
> the name is granted the same trademark release.

This commitment is irrevocable while the project has one
maintainer. It cannot be retroactively narrowed by future
maintainers.

### 3. Co-maintainer recruitment as `0.1.0` blocker

The `0.1.0` first public alpha will not ship until at least one
co-maintainer is added to this file. This makes "find another
maintainer" a release blocker, not an aspirational goal.

## Decision-making

Decisions follow **lazy consensus** (Apache Software Foundation
convention):

- A PR or ADR with no objections after 72 hours is approved.
- Any explicit objection blocks merge until the objection is
  resolved (addressed in the proposal, or the objection
  withdrawn).
- This applies equally to maintainer-authored and external PRs.

When the project has multiple maintainers, the same rule applies
— there is no separate maintainer-vote process. Lazy consensus
scales to the team size without changing.

## PR review SLA

- **First triage**: within 7 calendar days of PR open. "Triage"
  means a maintainer has read the PR description, classified the
  change (additive / behavioural / breaking), and either left
  initial feedback or labelled `ready-for-deep-review`.
- **Full review**: within 14 calendar days of PR open.
- PRs that miss either deadline auto-label `stale-triage`, and
  the named maintainer is paged.

The SLA is best-effort during normal periods and explicitly
relaxed during published vacation windows (announced via
`MAINTAINER_AVAILABILITY` GitHub Discussion thread).

## Trademark

There is no registered "creek" trademark at present. If one is
filed in the future, the pledge in section 2 of bus-factor
mitigation governs its disposition while the project has fewer
than two maintainers.
