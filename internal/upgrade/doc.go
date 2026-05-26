// Package upgrade verifies a release artifact's integrity before
// `creekctl self-upgrade` swaps it into place. Two-layer check:
//
//  1. cosign keyless verification of checksums.txt against the
//     pinned release-pipeline identity (Fulcio + Rekor). Proves
//     the checksums file came from this repo's release.yml on a
//     v* tag — a fork or hijacked workflow cannot satisfy.
//  2. SHA256 of the downloaded tarball matches the checksums.txt
//     entry. Proves the bytes on disk match what cosign signed.
//
// A rejection from EITHER layer surfaces ErrSignatureInvalid; the
// admin API's upgrade_signature_invalid error code is the wire-
// format equivalent. Setup / availability errors (cosign not
// installed or non-executable, network timeout, missing checksums
// entry) do NOT surface as ErrSignatureInvalid — they're returned
// verbatim so the caller can tell "untrusted bytes" from "couldn't
// even attempt verification" and react accordingly.
//
// Verification shells out to the cosign binary (matching install.sh's
// pattern) rather than embedding sigstore-go. The dep cost of
// sigstore-go is significant and 0.0.x users running self-upgrade
// already have cosign on PATH from the install.sh paranoid mode.
// 0.1.0 may revisit if shipping creekctl without the cosign
// dependency becomes a goal.
package upgrade
