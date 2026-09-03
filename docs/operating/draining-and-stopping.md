# Draining and stopping

Nothing billet does on its own ever destroys a running job. A drain seals admission and waits, for as long as the work takes; a stop leaves running jobs running; the one operation that ends work is `billet force-destroy`, and it names every job before it acts.

## Drain

```bash
billet drain --reason "kernel update"        # seal admission; returns at once
billet drain --wait --timeout 2h             # seal, then wait for the deployment to hold nothing
billet resume                                # open admission again
```

A seal stops new work being admitted. It takes effect at each listener's next poll and says nothing about what is already running, so `--wait` is what asks the real question, in two stages. First the ledger: no lease is holding anything. Then the machines: the control plane asks every node what it is actually running, because the ledger cannot see compute whose lease has already gone, and an empty answer counts only after five minutes of continuous emptiness with no launch dispatched in between. `--wait` exits 2 while still draining or when interrupted, and the seal remains; treat that as *not drained*, because reading it as permission to continue is what destroys running jobs. `--without-compute-proof` skips the second stage and prints a different conclusion.

A host that is off, or negotiated a protocol too old to be asked, never answers and blocks the proof; `billet nodes decommission` is the escape ([Adding and removing nodes](nodes.md)).

## Stop

A SIGTERM to either process is a drain:

- `billet node` stops taking new work, waits for the compute already running here, destroys it when each job completes, tells the control plane it is leaving, and exits.
- `billet server` stops admitting work and waits for the jobs it is tracking, finishing the teardown it owes (destroys a completion already asked for, the session close, the idle escrow).

Neither has a deadline. `drain_timeout` decides only when billet starts saying the drain is running long, and it repeats every fifteen minutes. The packaged units and plists give the service manager 88200 seconds before it kills a drain, so a reboot mid-drain loses billet's bookkeeping and not the jobs: the guests keep running, their leases stay charged, and the next process re-adopts them.

A second SIGTERM ends the **wait**, not the work:

```bash
sudo systemctl kill --kill-whom=main --signal=SIGTERM billet-server
```

`--kill-whom=main` matters because without it systemd signals every process in the service, including a container CLI billet has in flight. The jobs still running are left running, their capacity stays charged until a host proves the compute gone, and the next control plane re-adopts their leases.

## `billet local down` and `uninstall`

`billet local down` is a drain that then stops, in that order: seal, wait, stop the node, stop the server, disable both. It has no default time limit (`--timeout` bounds the wait; `--reason` is recorded). It re-reads the clearance immediately before the first stop, refuses to stop a service that is still deactivating, and on a node-only host says that it cannot fence anything and names `billet drain` against the control plane. Its seal is cleared by the next successful `billet local up`; an operator's seal from `billet drain` is left alone. `billet local uninstall` is `down` plus forgetting the services, leaving the config, App key, ledger, identity and CA where they are.

On a Mac the same commands drive the launch agents, with one difference: launchd cannot start a disabled service, so `up` enables before it proves, and what protects the host is the unwinding ([Run jobs on a Mac](../deploying/mac-tart.md)).

## Force

```bash
billet force-destroy --tier billet-8vcpu --reason "host being retired" --yes
```

The one operation that fails somebody's build on purpose. It refuses unless admission is already sealed by `billet drain`, because it enumerates a set, shows it to a person and acts on their answer, and a job admitted in between would be destroyed without ever appearing in the list. The target set is fixed when the request is taken; a standing flag would destroy every job launched after it. Every affected job, lease and host is printed before confirmation, forced leases are archived as failed, and it warns that GitHub does not requeue a job whose runner vanished after starting. A lease a node has taken custody of is not touched; `billet leases release --force` is the operation for that, because it goes through the holder rather than underneath it. Exit 2 means refused and nothing destroyed.

## Removing GitHub and AWS resources

`billet teardown --tier <label>` or `--all` deletes the scale sets billet created on GitHub; there is no delete control for them in GitHub's UI. `billet decommission --yes` deletes the EC2 instances and the EBS+S3 cache billet made outside Terraform, scoped by the deployment identity; run it before `terraform destroy`, and pass `--terminate-instances` only if failing any job still running is acceptable. The CodeBuild Terraform module refuses a destroy while a build is running; drain first.

## What stays charged

A stop deliberately leaves capacity charged for compute that is still running. Freeing a slot whose container is live is the overcommit every ordering here exists to prevent; capacity reclaimed late is recoverable, and a failed build is not. [Status and leases](status-and-leases.md) shows what is held and why.
