# Hybrid: owned hardware with cloud fallback

Your own hardware for the builds, the cloud for when it is not there. This is the shape billet was built for: one `runs-on` label means "the machine at home if it is up, EC2 if it is not", with the control plane somewhere always on.

```text
                               outbound long-poll
                    GitHub <-------------------------+
                                                     |
                                      +--------------+---------------+
                                      | small EC2 control plane      |
                                      | billet server                |
                                      | SQLite on encrypted gp3 EBS  |
                                      +--------------+---------------+
                                                     ^
                                          nodes dial | mTLS over a private path
                                                     |
                          +--------------------------+-------------------------+
                          |                                                    |
               +----------+-----------+                             +----------+-----------+
               | Local Linux host     |                             | EC2 orchestrator node|
               | Firecracker + Ceph   |                             | EC2 instance per job  |
               | preferred capacity   |                             | On-Demand fallback    |
               +----------------------+                             +----------------------+
```

## The controller

A small Graviton EC2 instance with SQLite on a dedicated encrypted gp3 volume and EC2 auto-recovery. Auto-recovery keeps the same instance and attached volume; an Auto Scaling group launches a new instance that does not reattach the ledger, so it is the wrong tool ([ADR-001](../reference/decisions/adr-001-control-plane-hosting.md)). SQLite stays on local EBS, never EFS or NFS. The Terraform root module creates the instance, its security group, the auto-recovery alarm and the retained ledger volume ([AWS with EC2](aws-ec2.md)).

**The state volume must fail closed.** The packaged unit's `StateDirectory=` and billet itself both create a missing `/var/lib/billet/server`, so without a mount dependency a failed EBS mount would let the controller start on the root disk with a new ledger, identity and CA. Set the host role's `billet_ledger_volume_id` to the module's `ledger_volume_id` output and the role mounts the volume by its NVMe identity (never a filesystem UUID, which a snapshot clone duplicates), formats it only when blank, adds `Requires=` and `RequiresMountsFor=` to the server unit, proves the state directory is served by that volume, and refuses to shadow an existing root-disk ledger.

## How nodes reach it

GitHub still makes no inbound connection; only nodes need to reach the controller's mTLS listener, and a node needs no open port of its own.

| Path | Use it when | Cost |
|---|---|---|
| Private network or VPC peering | the node is already inside the VPC | none |
| Restricted security-group ingress | the node has a static address | fails silently when that address changes |
| A VPN or overlay (Tailscale, Headscale, WireGuard) or a reverse tunnel | you want the port unreachable from the internet | a component you run |
| A public node-wire port protected by mTLS | the node's address is not stable: a machine in a spare room | the usual rate limiter any public TLS port wants, and you open the enrollment port yourself, briefly, when you add a machine |

The last row is supported. The node wire has no unauthenticated route on it, a caller with nothing to present is refused in the handshake and never reaches billet, and the connection budget is charged only to callers that verified, so anonymous traffic cannot displace an admitted node. What stays best effort is a handshake slot, which is what a rate limiter in front is for. Enrollment lives on its own port:

```yaml
server:
  listen: 0.0.0.0:7717            # the node wire: a certificate, or no handshake
  bootstrap_listen: 0.0.0.0:7718  # /v1/ca and /v1/enroll, and nothing else
  node_tls_hosts: [billet.example]
```

`bootstrap_listen` unset is a refusal, not a default: without it you issue certificates with `billet ca issue` and copy them out of band. `node_tls_hosts` must name every address and hostname a node will dial, including the one it dials the bootstrap port by, because both listeners present one certificate. The Terraform module outputs the controller's private address as `node_wire_address`; a public posture supplies its own name. See [Adding and removing nodes](../operating/nodes.md).

## The two nodes

The local host is [a Linux Firecracker host](linux-firecracker-host.md) with `node.server_addr` pointing at the controller and a certificate. The EC2 fallback is an orchestrator node: it runs no jobs itself, launches one instance per job, and needs explicit `node.max_vcpu`/`max_memory` budgets, an IAM role, a subnet and security groups, and a region-scoped AMI ([AWS with EC2](aws-ec2.md)). It can run beside the controller or on its own small instance; neither choice changes controller availability, because no new work is scheduled while the controller is down.

One tier with ordered providers keeps one stable label:

```yaml
tiers:
  - label: billet-8vcpu-ubuntu-2404
    providers: [firecracker, ec2]
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

Provider order is policy: billet fills Firecracker first and uses EC2 only when the local node cannot accept the job, and it chooses before it accepts. `trust: untrusted` makes the network requirement visible: every Firecracker node serving the tier needs `node.firecracker.untrusted_bridge`, and every EC2 node needs `node.ec2.untrusted_security_group_ids` that cannot reach production or the control plane; without them the providers refuse the work.

## Which fallback

| Option | For | Against |
|---|---|---|
| EC2 On-Demand | available today; an isolated instance per job; survives a local power or ISP outage; no provider-imposed job ceiling | about 48 to 59 seconds cold start measured; a cold cache during failover; On-Demand price |
| EC2 Spot | cheaper | AWS may reclaim the instance, and GitHub does not requeue a job that already started, so an interruption fails the build; only for explicitly retryable work |
| A second local Firecracker server | fast warm capacity; shares the site cache with every node on the same Ceph cluster (proved on real hosts) | a host in the same building shares power and ISP; another site needs its own storage |
| CodeBuild | AWS manages hosts and gives managed macOS | a 36-hour ceiling; untrusted work refused; reserved macOS is a 24-hour minimum commitment |
| A Mac with tart | owned Apple Silicon, no duration limit | you supply the Mac and the image |

For important Linux jobs, EC2 On-Demand is the correct fallback. Add a second local server for capacity or resilience to one machine failing, at another site for resilience to a local outage.

## Bringing it up

1. Terraform: the controller, the ledger volume, connectivity, IAM, the alarm; and the EC2 fleet module for the fallback node's role, security groups and cache bucket.
2. Install the same release everywhere; build the region's AMI with `billet ami build`.
3. Create the App, write the tier catalogue with ordered providers, issue or enroll a certificate for every node, run `billet check` against each config.
4. Start the server, then the local node and the orchestrator node.
5. Run a job locally, withdraw local capacity, run it again on EC2 with the unchanged label, and confirm every instance disappeared. That exact sequence has completed against real GitHub and AWS ([AWS acceptance](../reference/records/aws-acceptance.md)).
6. Rehearse controller recovery and restore before calling the deployment recoverable.

[Reaching your hosts](reaching-hosts.md) covers how the machine running Ansible reaches the local host.
