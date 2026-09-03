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

Each tier becomes one GitHub runner scale set whose name is the label, so `runs-on: billet-8vcpu-ubuntu-2404` is the whole contract between a workflow and billet. A tier names one provider or an ordered list of them; with a list, the control plane fills the first provider's machines before the second's, which is how `[firecracker, ec2]` means "the box at home before the cloud". Each backend gets its own `launch` entry because a Firecracker generation, an EC2 AMI and their runner commands are backend-specific.

`sizes` with `memory_per_vcpu` expands one template into several real tiers. `disk` is real root-volume capacity on Firecracker and EC2 (the guest's copy-on-write clone is grown before boot; the golden image stays small and shared) and is ignored by Docker. `max_concurrent` is a ceiling and `reserved` is a floor held against machines that could keep it. Tiers are read at startup, so adding or changing one restarts the control plane.

## Capacity is a vector, and it is escrowed before it is advertised

The deployment has a ceiling (`server.max_vcpu`, `server.max_memory`) and every node contributes a budget (`node.max_vcpu`, `node.max_memory`; detected on a host that runs work, required on a cloud orchestrator). Capacity is counted in vCPU, memory, per-tier concurrency and macOS licence slots at once, never as one number.

Each tier's listener reserves capacity in the ledger first and advertises to GitHub second. If listeners advertised independently, GitHub could fill all of them at once and the host would be overcommitted with nothing to stop it. A tier advertises the smaller of the deployment ceiling and what its eligible machines can hold, and it keeps exactly one discovery slot beyond the work GitHub reports assigned, climbing with demand, so an idle tier does not hold capacity its peers could use.

## Every runner is preceded by a lease

A lease exists from the moment capacity is escrowed, before anything boots, and its phases are written down: `capacity → assigned → launching → online → busy`, with `custody`, `teardown` and `quarantine` as holding states and `done` or `failed` at the end. A lease that released its capacity never moves backwards. Every write to a lease presents an epoch and a stale one is refused, so a holder that was declared dead and replaced cannot keep writing to a slot somebody else now owns.

## Placement

The control plane chooses the machine when the job is admitted, not when it launches. It walks the tier's providers most-preferred first, then packs (`server.placement: pack`, the default) or spreads, then picks by name. A tier may pin a `node` or a `site`; a macOS tier pins a host because Apple's two-guests-per-Mac limit is counted per machine. A cloud node registers its ordered purchasable shapes and placement charges the first shape that fits rather than the smaller tier request, authorising any fallback against both the node's budget and the deployment ceiling before a request reaches AWS.

`nodes:` is policy about hosts, not a roster of them: a per-host guest-OS allowlist and macOS limit. Registration is dynamic; adding a machine needs no control-plane restart.

## Capacity comes back only on proof

A completion asks the node to destroy the compute; capacity is released when the node confirms it is gone. A lease whose holder stops heartbeating is not freed: if nothing had launched behind it, it fails; if something had, it is **quarantined**, still charged to its host, until the host reports what it is actually running or an operator asserts the compute is gone (`billet leases release --force`). Compute that survived a control-plane restart is **adopted** and left to finish, because GitHub does not requeue a job whose runner vanished after it started. Freeing a slot early would let two jobs land on a machine sized for one; capacity reclaimed late is recoverable and a failed build is not.

A stop never destroys a running job, and nothing times one out. `billet drain` seals admission and waits for what is running, for as long as it runs; `drain_timeout` only decides when billet starts saying the drain is long. The one operation that ends running work is `billet force-destroy`, which names every affected job before it acts.

## Cost

Declared prices on cloud shapes report exposure; they do not gate admission. The enforceable policy is the shape allowlist and the resource ceilings; `billet check` reports one node's conservative peak and `billet status` the deployment-wide peak. An AWS budget is the account-wide backstop.

[Status and leases](../operating/status-and-leases.md) shows all of this from the operator's side.
