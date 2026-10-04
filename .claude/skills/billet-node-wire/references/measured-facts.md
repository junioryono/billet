# Measured facts

Part of the `billet-node-wire` skill: what was measured, dated where it was recorded.

- Idle anonymous connections on the shared listener: about 4.3 requests a second emptied a budget of 512; silent sockets: about 52 a second.
- Dropping the `charged` guard on permit release did not fail a test; it hung the closing goroutine.
- GitHub's session conflict: `409 Conflict … RunnerScaleSetSessionConflictException`.
- A new node against an old plane fails as `json: unknown field "min_version"` before any version check.
- `tls.AlertError` cannot be matched with `errors.As` (unexported type); the refusal is asserted as `*net.OpError`.
