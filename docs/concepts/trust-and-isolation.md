# Trust and isolation

Read this before pointing billet at anything.

## Trust belongs to a pool, not to the event that scaled it up

GitHub registers a runner into a scale set and may give it any job waiting in that set, so billet never derives authority from `push` versus `pull_request`. Every tier is `trust: untrusted` unless you promote it. A `trusted` tier requires a non-default runner group and an exact `workflows` allowlist; billet reads GitHub's runner-group policy at startup and again before minting each registration, and refuses drift. You remain responsible for those workflows not checking out or executing untrusted revisions.

A trusted tier exists only under an **organization** target. A repository has no runner groups, so nothing on GitHub's side can restrict a pool there, and every tier under a repository target is untrusted: refused at load with `trust: trusted`, `runner_group`, `workflows` or `intercept`, and refused again by the control plane at startup. That also decides which backends can serve a repository target: only those that admit untrusted work in the table below.

**Do not use self-hosted runners with public repositories.** That is [GitHub's own guidance](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access). Fork pull requests do not receive your secrets, but they get arbitrary code execution on your hardware. billet's microVM backends help, because each job gets its own kernel, but billet does not make running untrusted code on your own machine safe and will not pretend otherwise. Private repositories with controlled workflows are the intended trusted shape.

## What each backend isolates

| Backend | Boundary | Untrusted work |
|---|---|---|
| `docker` | a container sharing the host kernel | **refused** |
| `firecracker` | a microVM under the jailer: its own kernel, chroot, unprivileged uid, cgroup, seccomp | admitted only with a separate `node.firecracker.untrusted_bridge` |
| `tart` | a VM under Apple's hypervisor: its own kernel | admitted only with `node.tart.untrusted_isolation: softnet` |
| `ec2` | one instance per job, destroyed with it | admitted only with `node.ec2.untrusted_security_group_ids` |
| `codebuild` | a build on AWS-managed hosts | **refused**: reserved fleets keep instances alive between builds and share cached data across projects by design, and even on-demand builds expose the service role to the job |

A boundary is the kernel, not the network. A guest on your ordinary bridge reaches whatever that bridge reaches, which on the reference host includes the Ceph cluster and the control plane. So the absence of the separate network setting is the refusal, and billet never rewrites host networking for you: the Ansible host role creates the separate bridges and blocks their access to the host, private networks, link-local and cloud metadata by default, and if you do not use it, that policy is yours to write before enabling untrusted work.

## What a job can reach

- **Its runner registration** is single-use and consumed before any workflow step runs. It is delivered off argv on every backend: an env file for Docker, the metadata service for Firecracker, stdin to the guest agent for tart, one-job instance user data for EC2 (with the metadata service closed to containers inside the job), a Parameter Store secret resolved by CodeBuild.
- **No Docker socket** from the host, ever. Guests run their own Docker.
- **No instance profile on EC2** unless you configure one, because a profile is readable from inside the guest and is therefore a credential handed to whatever the job runs. On CodeBuild the service role is readable from inside the build; that is one reason untrusted work is refused there.
- **Caches, deliberately.** A cache is a cross-job channel. Trust gates publication, not reads: an untrusted job may hydrate from the trusted baseline and its writes are discarded. Docker image stores are scoped by deployment, site and architecture, so a private image pulled by one trusted job is readable by later jobs in that boundary. Sticky-disk keys are exact and may cross repositories, so prefix ordinary keys with the repository. Nothing prevents a trusted job leaking into its own cache: **do not cache secrets.**

## What runs as root

The control plane runs unprivileged as the `billet` service account with an empty capability set. The node runs as root on Linux, because Firecracker needs TAP devices, block-device nodes, cgroups and chroots, and a rootful Docker socket is host root anyway; the isolation is downstream, where the jailer drops every VMM to its own uid before it reads anything guest-controlled. On a Mac the node is a launch agent in your session, never root, because Apple's hypervisor needs your login keychain. [Identity and security](identity-and-security.md) has the rest of the privilege model.

## Transparent cache interception is opt-in

`ACTIONS_RESULTS_URL` carries artifact metadata as well as cache traffic, so interception is off unless a tier says `intercept: true`, terminates TLS only for that one host, and serves only the three cache methods. Untrusted jobs get neither the interception nor its CA. [Transparent Actions cache](../operating/actions-cache.md) covers it.
