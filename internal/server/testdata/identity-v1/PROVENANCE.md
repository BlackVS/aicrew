# Provenance of `examples.json`

`examples.json` is a verbatim copy of aimem's identity.v1 example matrix. It
is used only by tests, to keep aicrew consistent with the reviewed contract:
the session introspection route (`POST /v1/crew/introspect`, identity.v1 §3)
that aimem's introspection client consumes, and the receipt redemption
client in `internal/verifier` (identity.v1 §2), whose tests replay its
redemption exchanges, refusals and request-key vectors. It was rechecked
unchanged at aimem master `f50fd68`.

- Source repository: <https://github.com/BlackVS/aimem>
- Source path: `docs/fixtures/identity-v1/examples.json`
- Source commit: `68fcf3acfa0ea7b2e4f4b2992f9cb0cc566de344` (aimem master
  after E4b; the file is unchanged since `230cc9b`, E4a)
- Git blob: `91c54c5b5c0e34692f3b4cb3e7398b869c8da11a` (checked when copied)
- Contract: `docs/DESIGN-AIFORGE-IDENTITY-WIRE.md` §3 at the same commit;
  reference consumer `internal/introspect/introspect.go`
- License: PolyForm Noncommercial 1.0.0, same licensor as aicrew; aimem's
  `Required Notice:` line is kept in this repository's `LICENSE`.

The file's secrets are placeholders such as `{aimem_handle}`; the tests
substitute real values. Do not edit it here. A newer version is copied
again, with this note updated.
