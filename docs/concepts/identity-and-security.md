# Identity and security

## A deployment is four things

- **The ledger**: capacity, leases, job history, admission state, node registrations.
- **The deployment identity**: a random 32-character id minted once, which labels every piece of compute billet launches (a container label, an EC2 tag, a jail marker, a VM marker file, a build override) so that destruction is scoped to this deployment and never to a node name. Two billets on one machine share a hostname; they must not see each other's containers.
- **The GitHub App private key**: issued exactly once by GitHub, never reissued. billet never overwrites it and never renders it.
- **The node-wire certificate authority**: the control plane mints one per deployment and issues every node its certificate.

A ledger without its identity is a fresh authority that cannot see the compute the old one launched; an identity without the CA cannot issue a certificate; a CA without the App key cannot get a token. [Backup, restore and recover](../operating/backup-restore-recover.md) captures and restores all four or none.

## The node wire

A control plane bound to a network address requires client certificates and is its own CA; there is no CA to run and nothing to install. `billet ca issue <node>` mints a bundle you copy to a machine, or the machine asks over the enrollment port and you approve its fingerprint ([Adding and removing nodes](../operating/nodes.md)). The name in a node's certificate is the only thing that decides which node a request is from, and the certificate also carries which deployment it belongs to, so a fresh host does not invent an identity the control plane would refuse. Certificates renew themselves over the wire when a third of their life remains, with the private key never leaving the node; revocation is by serial and checked on every request. The authority itself is rotated as an overlap, never a switch ([CA rotation](../operating/ca-rotation.md)).

Two listeners, on purpose. The node wire (`server.listen`) requires a certificate in the TLS handshake and serves nothing to a caller without one, and its connection budget is charged only to callers that verified, so no volume of anonymous traffic can displace an admitted node. The two routes a machine needs before it has a certificate (reading the authority, asking to join) live on `server.bootstrap_listen`, which is unset by default: no address means no network enrollment, and `billet ca issue` is the way in. Open it when you add a machine and close it afterwards. Loopback stays plain HTTP, because there is nothing between two processes on one box to authenticate.

## The GitHub App

billet creates its App through GitHub's manifest flow, so every deployment ends up with the same minimal permissions for its scope. For an organization target: metadata read, organization self-hosted runners read and write. For a repository target: metadata read, repository administration read and write, which is the only permission GitHub offers for registering a repository's runners and is far wider than the organization one; billet uses it for that and never for the repository's settings, collaborators or branch protection, and the installation should be on that one repository rather than every repository the owner has. No Contents permission either way; billet cannot read your code. It is not literally "no access": the App can manage runners on its owner, which is a real capability, and every token billet mints is scoped to the App's installation there.

A deployment serving several targets holds one App per target, each verified against its own scope's permission set, each backed up as part of the one archive, and each installed by the host role at its own path. The credential is per target; the control plane, the fleet, the CA and the deployment identity are one ([ADR-011](../reference/decisions/adr-011-targets-and-repository-scope.md)).

## Secrets, and where they may not appear

| Secret | Held by | Never in |
|---|---|---|
| The App private key | the control plane host | argv, logs, any error message, a second copy nobody knows about |
| A runner registration (JIT config) | one guest, single-use | argv, a log, a disk the host shares, a buildspec command |
| A cache bearer | one guest, for its lifetime | cache keys, logs, anything deployment-wide |
| The node-wire CA key | the control plane | any store that is not the file layout it was written to |
| AWS credentials | a cloud node, from the instance role or the environment | any rendering path: every type that holds one redacts itself |

A launch failure quotes nothing the guest wrote, because a job can choose those bytes. What billet prints about a failed launch is a token from a closed vocabulary its own launcher wrote.

## What runs as what

| Process | Account | Why |
|---|---|---|
| `billet server` (Linux) | `billet`, empty capability set, `ProtectSystem=strict` | it talks to GitHub and a database and needs nothing else |
| `billet node` (Linux) | root | Firecracker needs TAP devices, block devices, cgroups, chroots and verified process signalling; the Docker socket is host root anyway. The jailer drops each VMM to its own uid before it reads guest state |
| `billet node` (macOS) | your login session, as a launch agent | Apple's hypervisor needs an unlocked login keychain and tart's image store is per user |
| every guest | its own kernel (Firecracker, tart, EC2) or its own container (Docker) | never the host's Docker socket |

## Reporting a vulnerability

Do not open a public issue. Email the maintainer. Note that "a job can affect the host it runs on" is documented behaviour on the Docker backend and on any backend without its separate untrusted network, not a vulnerability; [Trust and isolation](trust-and-isolation.md) states the boundary.
