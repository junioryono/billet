# Adding and removing nodes

A control plane bound to a network address requires client certificates and is its own certificate authority. There is no CA to run and nothing to install. A machine joins by asking and having its fingerprint approved, or by being handed a certificate you issued.

## One command on each machine

```bash
# on the control plane: a token, and the whole command to run on the new machine
billet ca token

# on the new machine, as that command printed it
billet join --server billet.example:7717 --ca-fingerprint SHA256:... --token h7q2... \
  --bootstrap-addr billet.example:7718 --node-name mac-mini-1 --org acme

# back on the control plane, with the two lines `billet join` printed
billet join accept billet-join-1.H4sI...
billet nodes approve mac-mini-1 --fingerprint SHA256:...
```

`billet join` first fetches the control plane's authority, holds it to `--ca-fingerprint`, and confirms over a handshake verified against it that the answer came from the control plane, so a wrong fingerprint or address, or something replaying the public certificate, changes nothing on the machine. Then it sees to the prerequisites. On a Mac it installs tart and softnet with Homebrew if they are missing, then asks for `sudo` once and runs the system's own binaries for what needs root: it keeps the Mac awake, gives it its node name, hands the `/usr/local` directories to the account, and makes softnet setuid-root for untrusted work. Each step is read first and done only where it differs, checked before the password is asked (a directory under a link or under a directory another account can change is refused, and so is a softnet outside Homebrew's Cellar or with a second name), and read back afterwards. billet itself never runs as root on a Mac, because the account owns the binary in `/usr/local/bin`. `--harden-ssh` also installs the key-only sshd drop-in from [Headless operation](../reference/reference-hardware.md#headless-operation) and removes it again if `sshd -t` refuses it; it is never the default, because `AllowUsers` shuts every other account out and no command can prove you still have a key login, and it refuses over a drop-in already there. On Linux the prerequisites are verified, never changed, and a missing one is refused with the fix: the package for the `billet` account, Docker for a Docker node, and the host role ([Linux Firecracker host](../deploying/linux-firecracker-host.md)) for a Firecracker one. Then it generates the node's config with `billet init --join` (flags after `--` go to `init` as they are), enrolls, pulls a Mac's tier images and runs `billet local up`. Run it as the node account on a Mac, never under sudo, and as root on Linux. Each step is reported and a re-run resumes: a node that already holds a valid bundle for its name and deployment is not enrolled again, and a bundle for anything else is refused rather than enrolled over. If `billet init` leaves an existing config alone and writes `billet.yaml.new` beside it, the join stops there. `--dry-run` fetches the authority, lists what it would do and changes nothing.

## The new machine's config

`billet init --join <control plane>:7717` generates a config the way a single-machine `init` does, from the same flags, and then turns it into a node that joins the control plane at that address: no `server:` and no `github:`, `node.server_addr` set, `node.tls` naming the bundle under the config directory's `tls/`, and the ceiling it measured moved onto `node.max_vcpu` and `node.max_memory`. Left unset there, a node contributes its whole machine rather than what it was measured to spare. It then prints the control plane's half: the deployment ceiling has to cover this node's contribution, because it caps every node and room it does not count is room nothing hands out, and the control plane needs this node's `tiers:` and `nodes:` entries. Joined with `billet join` (below), that half is one command to run on the control plane, `billet join accept <token>`, bound to the deployment the join verified. It merges the tiers by label and the `nodes:` entry by name, keeping an identical entry and refusing a different one under the same label or name with both shown, so a tier pinned to this node gets a label of its own. It raises `server.max_vcpu` and `server.max_memory` to what every registered host contributes plus this node, never lowering them, so accepting twice or after the node registered changes nothing. It keeps the file's comments, mode and owner, edits a symlinked config at its target, and refuses a proposal bound to another deployment, an entry for any host but the joining one, YAML anchors or merge keys, and a merge that would change anything but the tiers, the `nodes:` entries and the ceiling. `--dry-run` says what it would change. A config the collection renders from an inventory is not edited, because the next converge would undo it; the command prints the same half as inventory changes instead. After a plain `billet init --join`, merge the printed entries by hand under the same rules and raise the ceiling by the node's contribution. The control plane reads tiers at startup, so restart it afterwards; the restart is a handoff, not a drain. `billet check` on the control plane says when the ceiling is below what the registered hosts contribute. A join never replaces a config whose control plane holds a live deployment identity, even with `--force`; retire that deployment first, or write the joined config somewhere else with `--config`. The certificate bundle comes from either way of joining below.

## Enroll: the machine asks, you approve a fingerprint

Two ends display the same number and you check they match. That comparison is the trust decision; everything else is transport. `billet join` does the node's half of this; a machine whose config is already written does it with `billet node --enroll`, which `billet ca token` also prints.

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

## Keeping a node when its controller retires

A host in a PostgreSQL active-passive pair can retire its controller and keep its installed node, and the rules that decide whether it may are the retirement's, not this page's. What a node operator has to know before planning one: a node still dialling the controller being removed cannot be retained until its endpoint moves to the survivor in an ordinary converge, and retention is not a promise of uninterrupted node uptime, because the transition stops and starts the node on the serverless configuration before it completes. [Retiring a controller that keeps its node](upgrades.md) states both rules and the refusals that enforce them; do not restate them here.

## Decommissioning

A node row is durable, so a retired machine would block every future drain from proving the fleet clear. `billet nodes decommission <node>` stops expecting the host to answer for its compute. It requires a current proof that the host holds nothing (the drain's compute barrier), refuses a host the deployment is still talking to, and refuses a host still holding a lease. `--force` overrides only the reachability refusal and records the exclusion as durably **unproven**, which `billet status` and every later drain name rather than reporting the fleet clear. The ordinary replacement of a machine needs none of this: reuse the node name.

`billet status` reports each host's negotiated protocol version and release under `protocol`, and a host the deployment cannot reach still counts, because its compute may be running and it will come back speaking whatever it spoke before; decommissioning is what closes that window when a release stops carrying an old protocol ([Upgrades](upgrades.md)).
