<!--
Thanks for the PR! Please fill in the sections below. The
checklist isn't ceremonial — the API impact box and the DCO
sign-off are both reviewer-enforced gates today; a dedicated
api-diff workflow is planned for 0.1.0.
-->

## Summary

<!-- 1-3 sentences: what changed and why. The "what" is in the
diff; emphasise the why. -->

## API impact

Pick exactly one:

- [ ] This PR does NOT change any wire format (no spec/status
      field, no endpoint, no header, no error code).
- [ ] This PR is API-additive: append a row to
      [`docs/api/breaking-changes.yaml`](docs/api/breaking-changes.yaml)
      with `kind: additive`.
- [ ] This PR is API-breaking and references **ADR-NNNN** (must
      exist at [`docs/adr/NNNN-*.md`](docs/adr/) with
      `Status: accepted` BEFORE this PR can merge). See
      [`docs/adr/README.md`](docs/adr/README.md) for the
      template.

## Non-goals check

- [ ] I have read [`NON-GOALS.md`](NON-GOALS.md) and confirm
      this PR does not implement a documented non-goal. If it
      does, link the ADR that revises the non-goal.

## Tests

<!-- For Go changes:
  - [ ] New tests cover the new code path.
  - [ ] `go test ./... -count=1` passes locally.
For infra / config changes, state how the change is exercised
(e.g. "validated by next tagged release"). -->

## DCO

By signing off on each commit (`git commit --signoff`), I certify
that I wrote the change OR have the right to submit it under the
project's Apache 2.0 license, per the [Developer Certificate of
Origin](https://developercertificate.org). The sign-off line
reads `Signed-off-by: Name <email>` at the end of each commit
message.
