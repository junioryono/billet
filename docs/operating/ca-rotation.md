# CA rotation

The control plane is its own certificate authority: a per-deployment authority with a ten-year life whose private key never leaves that machine, issuing each node a one-year certificate. Nodes renew themselves; the authority is what you rotate.

## Why it is an overlap, not a switch

A node trusts the authority it was given. The moment the control plane presents a certificate from a new one, every node that has not heard about it fails to verify the server and drops out, over the wire it would need in order to recover. There is no way back from that remotely. So a rotation has two phases and an operator runs the second when they can see the fleet has moved.

```bash
billet ca rotate     # phase one
# ... wait for the fleet to renew ...
billet ca retire     # phase two; --force to skip the wait
```

**Phase one.** The new authority issues node certificates; the old one still signs what the control plane presents; both are trusted. Nodes adopt the new authority through ordinary renewal, which carries the trust bundle alongside the certificate. Restart the control plane to pick it up.

**Phase two.** Once every node has renewed, drop the old authority and present a certificate from the new one. A node that missed the whole overlap has to be re-enrolled, which is why this is a command rather than a timer. A second rotation while one is running is refused: there is one previous authority, and starting another would drop the one the un-renewed fleet still trusts.

## When to rotate

- **The authority is approaching expiry.** A leaf may not outlive its authority, so once the CA has less than a leaf's lifetime left, every certificate it issues is quietly shorter than the last, renewals come round faster, and then every node expires on the day the authority does. `billet ca show` warns once that starts; rotate then rather than waiting.
- **A host may have been compromised and you cannot enumerate what it holds.** Revocation is by serial; a legacy certificate whose serial was never recorded is caught by a clock-based cutoff instead, and a certificate minted by an authority running ahead of the control plane can survive it. Rotating the authority is the answer that needs no clock.

## What billet checks before it acts

`ca rotate` refuses if a previous authority is already present, with a different message for a leftover certificate (finish with `ca retire`) and a leftover key (billet says whether it is a copy of the installed authority, and leaves it for you). `ca retire` refuses unless the current pair is conclusively whole: `ca.crt` and `ca.key` read, parse, match and name this deployment. It removes a previous pair only when it can prove that pair is this deployment's own; the new authority records the fingerprint of the one it replaced inside its own certificate, so a second authority somebody assembled around the same deployment id cannot be retired as the predecessor. A crashed rotation leaves a previous certificate with no key; that is public, the ordinary leftover, and exactly what `ca retire` is for.

A `billet local backup` taken during a rotation carries the previous key too, because it signs what the control plane presents until every node has renewed; a backup missing half of either pair is refused rather than shortened ([Backup, restore and recover](backup-restore-recover.md)).

## Two controllers

With `server.identity.backend: aws-ssm` the authority is replicated through Parameter Store rather than copied by hand. `ca rotate` and `ca retire` publish what they produced; the other controller takes it with `billet ca sync`. A host holding a different authority is refused rather than overwritten, naming both fingerprints; `billet ca sync --force` moves the old directory aside rather than deleting a private key. Two controllers do not converge on their own. See [PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md).

## Revoking a single machine

`billet nodes revoke <node>` withdraws every credential a machine holds; `billet ca revoke <node> --cert <path>` withdraws one; `billet ca revocations` lists them. Revocation is checked on every authenticated request, not only at registration, so a revoked host stops on its next call. See [Adding and removing nodes](nodes.md).
