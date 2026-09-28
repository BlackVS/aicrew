# Provenance of `examples.json` and `openapi-proposal.json`

Both files are verbatim copies of aimem's coordination.v1 fixtures. They are
used only by tests, to keep aicrew's coordination route (`POST
/v1/crew/coordination`), the facts it serves and the process pin it carries
consistent with the reviewed contract.

- Source repository: <https://github.com/BlackVS/aimem>
- Source paths: `docs/fixtures/coordination-v1/examples.json` and
  `docs/fixtures/coordination-v1/openapi-proposal.json`
- Source commit: `8ac4ef1` (aimem master after C5-w2, #141, which froze the
  process pin on `offer`, `accepted_attempt` and `independent_claim`)
- Git blobs, checked when copied:
  - `examples.json`: `829f5293b3407c45acb3b39b356da915e2b1eca1`
  - `openapi-proposal.json`: `77022a8f6d1590c0263a52cfeee2b7d1ab4067c3`
- Contract: `docs/DESIGN-AIFORGE-COORDINATION-WIRE.md` at the same commit
  (C5w, amended by C5c-w and C5-w2)
- License: PolyForm Noncommercial 1.0.0, same licensor as aicrew; aimem's
  `Required Notice:` line is kept in this repository's `LICENSE`.

The files' secrets are placeholders such as `{proof_offer}`; the tests
substitute real values. Do not edit them here. A newer version is copied
again, with this note updated.

aicrew applies the pin's written forms (aimem's `process.Ref` rules), not
only the OpenAPI schema, which cannot express them all. It deliberately
refuses a manifest of exactly `..`, which aimem's `checkRepoPath` at this
commit accepts although the written rule keeps the path inside the
repository.
