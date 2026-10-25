# Tiers and capacity

## A tier is a label, and a label is a scale set

You define tiers; billet does not ship a catalogue.

```yaml
tiers:
  - label: billet-8vcpu-ubuntu-2404
    providers: [firecracker, ec2]     # preference order
    trust: untrusted
    vcpu: 8
    memory: 32GiB
    disk: 160GiB
    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      ec2:
        image: ami-0123456789abcdef0
        command: [/usr/local/bin/billet-runner]
```

Each tier becomes one GitHub runner scale set whose name is the label, so `runs-on: billet-8vcpu-ubuntu-2404` is the whole contract between a workflow and billet. A tier can instead name its scale set with `runs_on`, which is how tiers on several organizations answer to one `runs-on` value while their labels, billet's own identity for each, stay distinct. A tier names one provider or an ordered list of them; with a list, the control plane fills the first provider's machines before the second's, which is how `[firecracker, ec2]` means "the box at home before the cloud". Each backend gets its own `launch` entry because a Firecracker generation, an EC2 AMI and their runner commands are backend-specific.

`sizes` with `memory_per_vcpu` expands one template into several real tiers. `disk` is real root-volume capacity on Firecracker and EC2 (the guest's copy-on-write clone is grown before boot; the golden image stays small and shared) and is ignored by Docker. `max_concurrent` is a ceiling and `reserved` is a floor held against machines that could keep it. Tiers are read at startup, so adding or changing one restarts the control plane.

## Capacity is a vector, and it is bought when a runner starts

The deployment has a ceiling (`server.max_vcpu`, `server.max_memory`) and every node contributes a budget (`node.max_vcpu`, `node.max_memory`; detected on a host that runs work, required on a cloud orchestrator). Capacity is counted in vCPU, memory, per-tier concurrency and macOS licence slots at once, never as one number.

Every tier advertises to GitHub at once, and steadily: its committed work, raised to the most of its shape the configured ceiling could ever run (`max_concurrent`, and `server.max_vcpu` and `server.max_memory` divided by the tier's shape). Nothing is reserved for an idle tier. When several tiers are waiting for the same room, `server.admission_order` decides who takes it: `fair` (the default) holds it for the tier that has waited longest until that tier's shape fits, as long as that tier's listener is still making progress, and `fill` gives it to anything that fits. The order decides purchases only — every tier goes on advertising throughout, because a tier that advertises nothing is assigned nothing. When GitHub assigns a job, the listener buys the lease for its runner at that moment, and the purchase is the ledger's one atomic check, so the ceiling, a target's share (`max_vcpu`/`max_memory` on its block), each node's budget, `max_concurrent`, `reserved` floors and macOS slots still decide it. Because the advertisements together can promise more than the fleet can run at once, GitHub can assign more work than there is room for; the jobs that do not fit wait, still assigned, and start as running work finishes. No job ever runs on capacity another holds. A sealed deployment and a draining control plane buy nothing, so an assignment that arrives then waits until admission reopens or another control plane takes it. The catalogue can contain overlapping shapes because each definition needs to fit individually, while actual leases share the bounded pool.

## Every runner is preceded by a lease

A lease exists from the moment capacity is escrowed, before anything boots, and its phases are written down: `capacity → assigned → launching → online → busy`, with `custody`, `teardown` and `quarantine` as holding states and `done` or `failed` at the end. A lease that released its capacity never moves backwards. Every write to a lease presents an epoch and a stale one is refused, so a holder that was declared dead and replaced cannot keep writing to a slot somebody else now owns.

## Placement

The control plane chooses the machine when the job is admitted, not when it launches. It walks the tier's providers most-preferred first, then packs (`server.placement: pack`, the default) or spreads, then picks by name. A tier may pin a `node` or a `site`; a macOS tier pins a host because Apple's two-guests-per-Mac limit is counted per machine. A cloud node registers its ordered purchasable shapes and placement charges the first shape that fits rather than the smaller tier request, authorising any fallback against both the node's budget and the deployment ceiling before a request reaches AWS. A fallback to a larger shape also asks the waiting order (`server.admission_order`) as a purchase would, so under `fair` it is refused when it would take room an older waiting tier is accumulating; the launch then fails and GitHub assigns the job again.

`nodes:` is policy about hosts, not a roster of them: a per-host guest-OS allowlist and macOS limit. Registration is dynamic; adding a machine needs no control-plane restart.

## Capacity comes back only on proof

A completion asks the node to destroy the compute; capacity is released when the node confirms it is gone. A lease whose holder stops heartbeating is not freed: if nothing had launched behind it, it fails; if something had, it is **quarantined**, still charged to its host, until the host reports what it is actually running or an operator asserts the compute is gone (`billet leases release --force`). Compute that survived a control-plane restart is **adopted** and left to finish, because GitHub does not requeue a job whose runner vanished after it started. Freeing a slot early would let two jobs land on a machine sized for one; capacity reclaimed late is recoverable and a failed build is not.

A stop never destroys a running job, and nothing times one out. `billet drain` seals admission and waits for what is running, for as long as it runs; `drain_timeout` only decides when billet starts saying the drain is long. The one operation that ends running work is `billet force-destroy`, which names every affected job before it acts.

## Size a tier by what it measured, not by its label

A vCPU is a share of whatever core it runs on, so an 8-vCPU tier on your hardware is not a commercial runner's 8 vCPU. On the reference deployment's node (AMD EPYC 7763, 2026-09-21), the same probe ran on a billet 8-vCPU Firecracker guest and on a commercial 8-vCPU runner:

| | Commercial 8 vCPU | billet 8 vCPU |
|---|---|---|
| sha256 of 2 GiB, one thread | 0.82 s | 1.42 s (the node itself: 1.24 s) |
| pgbench tpcb over the socket (scale 32, 16 clients) | 59,600 tps | 17,000 tps |

Most of that gap is per-core speed. The node's own cores are 1.5 times slower than the commercial runner's on this hash, and a guest adds about 15% on top. The CPU frequency governor is not the lever: busy vCPUs already ran at the node's all-core boost under `schedutil`.

Two consequences follow:
- **For a CPU-bound suite, a 16-vCPU tier roughly matched a commercial 8-vCPU runner here.** Size a tier by timing your own suite on both, not by matching the number in the label.
- **Pinning guests to host CPUs bought no speed.** It was measured in [CPU placement](../reference/records/cpu-placement.md): `compact` was slower than `spread` on every load `spread` could hold, and no different from unpinned guests at three quarters of the threads.

A service container published on a port is slower to reach than one reached by its container address, because Docker relays the port through a userland proxy. Turning that proxy off made the published path nearly as fast, but broke `localhost` for Node 18 and `[::1]` for every client. So billet's images keep the proxy, as GitHub's do ([Docker's published ports](../reference/records/docker-published-ports.md)).

## Cost

Declared prices on cloud shapes report exposure; they do not gate admission. The enforceable policy is the shape allowlist and the resource ceilings; `billet check` reports one node's conservative peak and `billet status` the deployment-wide peak. An AWS budget is the account-wide backstop.

[Status and leases](../operating/status-and-leases.md) shows all of this from the operator's side.
