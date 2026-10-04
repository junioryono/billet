# Measured facts

Part of the `billet-github-protocol` skill: what was measured, dated where it was recorded.

- Session conflict: `409 Conflict`, `RunnerScaleSetSessionConflictException`, for the same owner and for a different one (2026-09-04).
- An abandoned session: still refused at 60 seconds on eight runs, successor open at 91 on three and 92 on five (2026-09-04, 30-second polls, so the expiry is bracketed rather than named).
- Cross-session redelivery: the successor was handed the abandoned session's exact message id back (2026-09-04).
- The first real long poll ran about 88 seconds against a 90-second lease TTL.
- JIT config: `Ephemeral = True`, `DisableUpdate = True`.
- The worst defect in the project called `AcquireJobs` with ids from `JobAssigned` instead of `JobAvailable`, self-consistent on billet's side and wrong on the wire.
- Runner-group characters that do not survive the client: `&#;%+`; organization, and each segment of a repository: `#%/?`.
- Repository scope (2026-09-04, `TestLiveRepositoryScope` against a private repository under a personal account): `_apis/runtime/runnergroups/?groupName=default` answered one group, id 1, `Default`, `isDefaultGroup: true`; create, describe, session and delete all behaved as at organization scope; the installation reported account type `User` with exactly `administration: write` and `metadata: read`; the manifest flow landed on `/settings/apps/manifest`.
