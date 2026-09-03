# Next steps

You have a job running on your own hardware. Where to go depends on what you want next.

| You want | Read |
|---|---|
| To understand what just happened | [How billet works](../concepts/how-it-works.md), then [Tiers and capacity](../concepts/tiers-and-capacity.md) |
| A real kernel boundary per job, and a persistent cache | [A Linux Firecracker host](../deploying/linux-firecracker-host.md) |
| macOS or arm64 Linux jobs | [Run jobs on a Mac](../deploying/mac-tart.md) |
| Your own hardware first, the cloud when it is not there | [Hybrid: owned hardware with cloud fallback](../deploying/hybrid-owned-hardware.md) |
| Everything in AWS | [AWS with EC2](../deploying/aws-ec2.md) or [AWS with CodeBuild](../deploying/aws-codebuild.md) |
| To run fork pull requests | [Trust and isolation](../concepts/trust-and-isolation.md) first; it decides which backend you may use |
| A repeatable install | The same deployment converged by Ansible: [single-host-docker](https://github.com/junioryono/billet/tree/main/ansible_collections/junioryono/billet/examples/single-host-docker) |
| To know what to back up | [Backup, restore and recover](../operating/backup-restore-recover.md) |
| A second machine | [Adding and removing nodes](../operating/nodes.md) |

Whatever the shape, the sequence is the same: choose it in [Choose a shape](../deploying/choose-a-shape.md), deploy it, then operate it with the pages under Operating.
