# Adding and removing nodes

A control plane bound to a network address requires client certificates and is its own certificate authority. There is no CA to run and nothing to install. A machine joins by asking and having its fingerprint approved, or by being handed a certificate you issued.

## Enroll: the machine asks, you approve a fingerprint

Two ends display the same number and you check they match. That comparison is the trust decision; everything else is transport.

```bash
# on the control plane
billet ca show                      # the authority's fingerprint
billet ca token                     # a short-lived join token, and the whole command below

# on the new machine
billet node --enroll \
  --ca-fingerprint SHA256:...  \
  --join-token h7q2... \
  --bootstrap-addr billet.example:7718   # prints THIS machine's fingerprint, then waits

# back on the control plane
billet nodes pending                # shows the same fingerprint
billet nodes approve mac-mini-1 --fingerprint SHA256:...
```

Neither side accepts on faith. The node refuses to enroll without the authority's fingerprint, because its first connection has nothing to verify against and accepting whatever answered would let anyone who replies first own every job it runs. Approval refuses without the node's fingerprint, because approving by name alone approves whatever currently holds the name. A name is claimed by the first key to ask; a second key wanting it is refused.

The join token is what stops a stranger who can reach the port from filling the pending list or taking a name before the machine that should have it. It is short-lived (`--ttl`, default an hour), counted (`--uses`, default one) and stored as a hash.

**Enrollment is served on its own port, and its absence is a refusal.** Reading the authority and asking to join cannot require a certificate, so those two routes are not on the node wire; they are on `server.bootstrap_listen`, which is unset by default. Set it when you want a machine to be able to ask, and close the port afterwards: nothing a running fleet does goes through it. `billet ca token` prints the whole command including `--bootstrap-addr`, and `node.bootstrap_addr` is the same thing in the config (unset, it falls back to `node.server_addr`, which is right only for a control plane with no separate enrollment address).

## Or issue a certificate directly

Right for a machine you are provisioning anyway: cloud-init can drop a bundle on it, and no human is standing there to compare a fingerprint.

```bash
# on the control plane
billet ca issue mac-mini-1          # writes ./mac-mini-1-billet-tls/
scp -r mac-mini-1-billet-tls mac-mini-1:/etc/billet/tls

# in that host's billet.yaml; node.name comes from the certificate
node:
  server_addr: billet.example:7717
  tls:
    cert: /etc/billet/tls/node.crt
    key:  /etc/billet/tls/node.key
    ca:   /etc/billet/tls/ca.crt
```

The bundle carries the trust bundle, not only the issuing authority, so a node added during a CA rotation can verify the certificate the control plane presents. Both paths are recorded, so `billet nodes pending --all` is the single answer to what has been admitted and when.

## What a node registers

Registration is dynamic and never asks whether a host was declared anywhere. It negotiates a mutually supported protocol version, then checks a non-empty name, the deployment identity, a non-zero contribution, and that the site is one this deployment declares (a typo would otherwise become a place of its own with a cache that is always empty). The allocator then requires a provider and refuses to move a host to a different provider or site while leases are outstanding against it. `nodes:` in the config is policy about hosts (a guest-OS allowlist, a macOS limit), not a roster of them.

| Action | Control-plane restart? |
|---|---|
| a registered machine reconnecting | no; it re-registers itself |
| admitting a new machine | no; enroll, approve, and it joins |
| reclaiming stranded capacity | no; `billet leases release --force` works on a running deployment |
| adding or changing a tier | yes; tiers are read at startup and each becomes one scale set |
| changing the `nodes:` policy block | yes; it is snapshotted into the allocator at startup |

A node's identity is its **name**, so two hosts configured with the same one are one host to the control plane; a per-process incarnation routes new commands to the newest registration and lets a superseded process keep maintaining the work it already holds, but after a restart the plane cannot say which of two machines sharing a name physically holds a container. Give each host its own name.

## Certificates renew themselves

A node replaces its certificate over the wire when less than a third of its life remains, with the private key never leaving the node. A certificate that has already expired cannot renew, because renewal is authenticated by the certificate being renewed; that machine has to be re-enrolled. For a full-life certificate the window is months, and the usual way to miss it is a host powered off throughout. The authority is the cliff, not any single certificate: once the CA has less than a leaf's lifetime left, every certificate it issues is quietly shorter than the last, and `billet ca show` warns once that starts ([CA rotation](ca-rotation.md)).

## Taking one back

`billet nodes revoke <node>` withdraws every credential that machine currently holds, renewals included, and each is refused on the very next request it makes. Revoke the node, not a file: a node renews itself, so the bundle you issued names a serial it stopped presenting months ago. A certificate issued in a later second is unaffected, so a rebuilt machine can keep its name; mint the replacement a second later rather than instantly. `billet ca revoke <node> --cert <path>` withdraws one specific credential, and `billet ca revocations` lists what has been withdrawn. If you cannot enumerate what a compromised host holds, or you do not trust the clocks, rotate the authority instead.

## Stopping a node

A node that stops cleanly (`systemctl stop billet-node`, `billet local down`, SIGTERM) drains: it stops taking work, waits for the jobs already running for as long as they run, and then tells the control plane it is leaving, so placement moves to other hosts at once. A node that dies is forgotten only by silence, about four and a half minutes, because from the control plane a crash and a partition look the same and the drain's compute barrier depends on that caution. Its compute keeps running and is re-adopted when the node comes back.

## Decommissioning

A node row is durable, so a retired machine would block every future drain from proving the fleet clear. `billet nodes decommission <node>` stops expecting the host to answer for its compute. It requires a current proof that the host holds nothing (the drain's compute barrier), refuses a host the deployment is still talking to, and refuses a host still holding a lease. `--force` overrides only the reachability refusal and records the exclusion as durably **unproven**, which `billet status` and every later drain name rather than reporting the fleet clear. The ordinary replacement of a machine needs none of this: reuse the node name.

`billet status` reports each host's negotiated protocol version and release under `protocol`, and a host the deployment cannot reach still counts, because its compute may be running and it will come back speaking whatever it spoke before; decommissioning is what closes that window when a release stops carrying an old protocol ([Upgrades](upgrades.md)).
