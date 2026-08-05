# ABI v2 baseline

Migration C completed on 2026-08-05. Edge main revision
`5727793c79f1ff1fead3c983624773cd23c931ef` is the merged ABI v2 switch
baseline and uses SDK revision
`995c0bd40baa44098de11c137ceba9e8e79fdc41`.

All nine official plugins are ABI v2. Their merged source baseline is
`0e8a1014af138588e26ed2d1b9abfdaf65560bf6`. The release evidence included
reproducible builds for all nine, 92 bundle-gated behavior rows with zero
skips, fresh ordinary and serialized race suites, lint/vet/format/diff checks,
and a static binary check.

The supported guest-authoring path is the Go ABI v2 SDK. The SDK repository's
Rust implementation remains ABI v1 and cannot load in the v2-only Edge host.
