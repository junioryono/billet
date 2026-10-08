---
name: billet-identity-and-ca
description: "Load when touching internal/deploymentid, internal/wirecert (rotation, LoadServing, rotate/retire/revoke/renew), internal/wireshare and the aws-ssm identity store, internal/github (App onboarding, the private key, installation tokens), or the `ca *`, `nodes *`, `github-app create` and `store-key` commands; when a rotation, retire, restore or promotion touches ca.crt/ca.key; when an App key is written or published; or when a GitHub failure reads as a bare 404."
---

# Identity and authorities

## What this area is

A deployment is named by a 32-hex identity (`internal/deploymentid`, `deploymentid.Validate`) minted once per state directory and recorded in the ledger's binding. The control plane is its own certificate authority: `internal/wirecert` mints one CA per deployment (`CALifetime` 10 years, `LeafLifetime` 1 year, `ClockSkew` 1 hour, `ExpiryWarning` 30 days), issues node bundles, rotates and retires authorities and serves the TLS configs. `internal/wireshare` carries an authority between controllers through an `awsssm` store. `internal/github` creates the App through the manifest flow, signs App JWTs, resolves the installation and verifies the App. The commands are in `cmd/billet` (`ca.go`'s `ca` dispatch, `enroll.go`, `githubapp.go`, `appkey.go`, `appkeypublish.go`, `casync.go`, `authoritypublish.go`, `githubaccess.go`, and `internal/app/identitystore.go`).

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### The deployment identity and the CA: [references/deployment-and-ca.md](references/deployment-and-ca.md)

- **Destruction is scoped by deployment identity, never by node name.**
- **A node's identity and deployment come from its certificate.**
- **An empty CA directory is ambiguous, so `authority-created` remembers.**
- **A rotation is an overlap, not a switch, and it is published in an order a lock-free reader can trust.**
- **`Rotate` and `Retire` take `LockAuthority` themselves.**
- **Retire proves the current pair took over, and derives permission from what is proved.**
- **Revocation is by serial and renewal by the certificate being replaced.**
- **The CA is a slow cliff.**
- **A shared identity is replication over the file layout; moving the authority into a store is the refused answer.**

### The GitHub App: [references/github-app.md](references/github-app.md)

- **The App private key is issued once, never deleted by pathname, never rendered.**
- **Onboarding refuses everything it can before it touches GitHub.**
- **An App that holds a permission billet never requested is refused, so a job needing a permission billet did not ask for is another App's job.**
- **Every token billet mints is scoped to an installation, so losing the installation looks like nothing.**

### The identity directory's exclusion: [references/identity-exclusion.md](references/identity-exclusion.md)

- **A retirement's own exclusion acquires and never admits.**
- **An observer of the authority takes no lock and creates nothing.**
- **Every writer of the identity directory takes the exclusion before its first access, and borrows it downward.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Anything that writes the identity directory.** Take the exclusion before the first access and borrow it downward; an observer takes no lock and creates nothing ([references/identity-exclusion.md](references/identity-exclusion.md)). Permission to retire or replace an authority comes from what is proved, never from what is present or broken.

## Where the tests are

- `internal/wirecert/*_test.go`: rotation ordering, `LoadServing` tears and re-reads, retire refusals, the `billet-replaces:` claim, one-block PEM proof.
- `internal/wireshare/*_test.go`; `internal/app/proofs_test.go` (the adopt-before-serve order is `app.Controller.ServeWire`'s signature), `casync` tests, `identitystore` tests.
- `internal/github/*_test.go` (manifest permissions, redaction, code handling), `cmd/billet/githubapp_test.go`, `configedit_test.go`, `githubaccess_test.go` (`TestAnUnreachableGitHubIsNotReportedAsABadCredential`, `TestAStartupFailureNamesAnUninstalledApp`).
- `cmd/billet/ca_test.go` (`TestCAIssueDuringARotationWritesABundleThatCanVerifyTheServer`, `TestCAIssueWillNotOverwriteABundle`), `internal/e2e/enroll_test.go`, `revocation_test.go`, `restore_test.go`.

## Related skills

`billet-node-wire` (what the certificates authenticate), `billet-backup-restore` (the authority as part of the four-piece unit), `billet-security` (what may never be rendered), `billet-state` (the deployment binding and the claim).
