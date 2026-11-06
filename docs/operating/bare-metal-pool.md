# A pool of rented bare metal

A control plane with `server.pool` in its config orders Firecracker hosts from rented bare metal, keeps a floor of them per architecture, and orders more when a Firecracker tier is short of hosts. [ADR-030](../reference/decisions/adr-030-rented-bare-metal-pool.md) is the design and [the bare-metal hosts record](../reference/records/bare-metal-hosts.md) what it was measured on; the [configuration reference](../reference/configuration.md) has every field.

## What is built, and what is not yet

- **Built:** ordering. The controller keeps each architecture's floor, never past the maximum, and orders one more host for a Firecracker tier whose fresh capacity report says it is short of hosts (it has work the allocator could not buy for, and every reason was a host: none that can place its shape, or none with room; a seal, a tier's cap, a share or the deployment ceiling never count, and a share or ceiling with no room for one more of the tier's shape is named beside full hosts or none, so a spent budget never reads as a shortage; nor does a tier draining or one whose count of waiting work is unknown), while no placed host is still on its way. It orders nothing on an architecture while any order there is unknown, being ordered or uncertain, because that order may hold a host already. It passes over a choice its source refused (no stock, a quota, a refusal an operator acts on) for `refusal_backoff`. A report older than five minutes, or in error, is not read as demand, and the floor is kept regardless. Its sources are AWS bare-metal instances (`aws-metal`) and phoenixNAP Bare Metal Cloud (`phoenixnap`).
- **Demand grows the pool by one host at most, for now.** A placed host counts as on its way until a node is known to have joined from it, and nothing records that yet, so a second host for demand waits for the join.
- **Not yet:** a host joins with `billet join` and its order's single-use token, and its enrollment waits for a person to approve it with `billet nodes approve`, as any node's does; automated admission comes next. A host is never released by the pool yet: releasing needs the host linked to the node that joined from it, and a proof that it holds nothing (ADR-030), which come with the join. Until then an operator drains and releases a pool host by hand.

## An order's life

Every order is a row in the ledger, written before its source is asked, so `billet pool` always shows what was ordered:

- **ordering**: the row is committed and the source has been, or is being, asked.
- **placed**: the source named the host.
- **refused**: the source refused, or the order failed before anything reached the source; the reason is on the order.
- **uncertain**: the answer was lost, the source lists no host for the order, and a resend could place a second one. On AWS, whose launches carry the order as an idempotency token, the pool sends a lost order again for ten minutes from its first attempt before it calls it uncertain; phoenixNAP takes no such token, so a lost answer there is uncertain at once. A resend the source refuses makes the order uncertain too: the refusal says nothing about the first attempt, which may have placed a host.

An order records where its source orders (an AWS account, region, subnet and zone; a phoenixNAP account). If the config or the credentials change so the source orders somewhere else, or the source is no longer configured, an order still being ordered is not read back or sent again; it becomes uncertain, because reading it back anywhere else could miss the host it placed. Look where it was placed, then close it.

An uncertain order holds its place against the maximum and stops all ordering on its architecture, because it may hold a host. Its SCOPE column in `billet pool` says where it was placed; look for it there, in the source's own console: an EC2 instance tagged `sh.billet.pool-order=<order>`, or a phoenixNAP server named `billet-<order>`. If there is one, it is real and joins or can be released by hand; if there is none, close the order:

```bash
billet pool abandon --reason "no instance tagged with the order in us-west-2" p1a2b3c4d5e6
```

## What a host runs on its first boot

The `boot_prelude` script, then `billet join --server <join_server> --ca-fingerprint <the deployment's> --token <the order's> --node-name pool-<order> --provider firecracker`. The prelude installs billet and what a Firecracker node needs (the host role's packages, the kernel and guest image), or is nearly empty on an image that already has them. It runs as a script of its own under `bash -euo pipefail`, so a command that fails, an unset variable or a failed stage of a pipeline stops the boot before the join, and whatever it sets or traces does not reach the join. The whole payload carries a join token, so it is a credential: it reaches the source's API and nothing else, and it must fit EC2's 16384 bytes of user data.

## Credentials

- **AWS:** the control plane's own role, through the usual credential chain. It needs to launch, describe and terminate instances tagged `sh.billet.pool-owner=<deployment>` and read their console; the policy `billet init iam` renders does not cover pool hosts yet.
- **phoenixNAP:** an API client's id and secret, in the environment variables `phoenixnap.client_id_env` and `client_secret_env` name, never in the config. A new account may hold one server, and an API client may be refused orders until the account is cleared to order; the order is then refused with the API's own message.
