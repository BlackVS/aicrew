# Provenance of `examples.json` and `openapi-proposal.json`

Both files are verbatim copies of aimem's coordination.v1 fixtures. They are
used only by tests, to keep aicrew's coordination route (`POST
/v1/crew/coordination`), the facts it serves, and the process pin and
evidence digest they carry consistent with the reviewed contract.

- Source repository: <https://github.com/BlackVS/aimem>
- Source paths: `docs/fixtures/coordination-v1/examples.json` and
  `docs/fixtures/coordination-v1/openapi-proposal.json`
- Source commit: `a9b9b6fcf1963808c4480f7b96838bfd1ea10d56` (aimem master
  after C5-w3, #149, which binds a coordinated finalize to the confirmed
  delivery evidence; `docs/fixtures/coordination-v1/` is unchanged from
  #149's `0dd404a` to this commit). The previous copy was from `8ac4ef1`
  (C5-w2, #141).
- Git blobs, checked when copied:
  - `examples.json`: `26d408a0e04e1bb27a44ff7fe8a754c5214194de`
  - `openapi-proposal.json`: `7a0727856f5c5c209fb82d11b19ebbad4d3ec7f8`
- Contract: `docs/DESIGN-AIFORGE-COORDINATION-WIRE.md` at the same commit
  (C5w, amended by C5c-w, C5-w2 and C5-w3)
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
