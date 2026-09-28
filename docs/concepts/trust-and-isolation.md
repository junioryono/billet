# Trust and isolation

Read this before pointing billet at anything.

## Trust belongs to a pool, not to the event that scaled it up

GitHub registers a runner into a scale set and may give it any job waiting in that set, so billet never derives authority from `push` versus `pull_request`. Every tier is `trust: untrusted` unless you promote it. A `trusted` tier requires a non-default runner group and an exact `workflows` allowlist; billet reads GitHub's runner-group policy at startup and again before minting each registration, and refuses drift. You remain responsible for those workflows not checking out or executing untrusted revisions.

A trusted tier exists only under an **organization** target. A repository has no runner groups, so nothing on GitHub's side can restrict a pool there, and every tier under a repository target is untrusted: refused at load with `trust: trusted`, `runner_group` or `workflows`, and refused again by the control plane at startup. That also decides which backends can serve a repository target: only those that admit untrusted work in the table below.

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
- **Caches, deliberately.** A cache is a cross-job channel, so billet decides who may write one and says who may read it. See the next section.

## What runs as root

The control plane runs unprivileged as the `billet` service account with an empty capability set. The node runs as root on Linux, because Firecracker needs TAP devices, block-device nodes, cgroups and chroots, and a rootful Docker socket is host root anyway; the isolation is downstream, where the jailer drops every VMM to its own uid before it reads anything guest-controlled. On a Mac the node is a launch agent in your session, never root, because Apple's hypervisor needs your login keychain. [Identity and security](identity-and-security.md) has the rest of the privilege model.

## What a cache can leak, and to whom

Every cache is on by default where the tier can have it ([Build caches](../operating/build-caches.md)), so this section is about what running them means.

**Writes are what billet decides.** A job's writes land in its own clone and reach a published generation only under the tier's `cache.publish` rule:

- **Trusted-only.** A trusted pool publishes, and an untrusted one publishes nothing. This was every tier's rule before `cache.publish` existed, and is a trusted tier's default.
- **Default-branch.** A write is published only for a job GitHub proves ran on the repository's default branch under an event GitHub itself lets write the default branch's cache: `push`, `schedule`, `workflow_dispatch`, `repository_dispatch`, `delete`, `registry_package` and `page_build`. This is an untrusted tier's default when it has a repository scope. The proof is GitHub's record of the run, read at the completion: its head branch, head repository, event, workflow path and referenced workflows, and the repository's default branch. Anything that cannot be proved publishes nothing. That includes a fork, a pull request, a tag, `pull_request_target`, `issue_comment`, `workflow_run`, a reusable workflow pinned to another ref or another repository, and a run GitHub could not be asked about.
- **Off.** Nothing is published.

**Reads are the pool's.** Any job the pool runs reads what the pool's namespace published, before billet knows which job it is running: caches are attached before GitHub chooses the job. So what a default-branch job cached, every later job in that pool can read, a pull request's job included. That is GitHub's own model for the Actions cache, and it is why billet requires a *static* repository for default-branch publication. Narrow who can read with a repository target, or a runner group restricted to selected repositories. Scoped namespaces are separate from the trusted-only ones: an untrusted pool never reads a trusted pool's generations, and a pool of one repository never reads another's.

**What each cache can carry:**

- **Docker image store.** A private image pulled by a publishing job is readable by later jobs in the namespace.
- **Sticky disks.** Whatever a publishing job left on the disk.
- **Actions cache.** The archives `actions/cache` saved, scoped per ref as GitHub scopes them, restored from a job's own ref, then its pull request's base branch, then the default branch.
- **Git mirror.** Only what GitHub already let that job read. The node authorises every fetch with the job's own credentials before serving a byte, serves only commits or current refs' objects, and sends anything else to GitHub.
- **Bazel, Buck2 and Go caches.** Build outputs, and the action results the build recorded.

A cache blob is stored only under the digest of its own bytes, but an action result is the job's claim about an action, trusted exactly as far as the publishing job is. **Do not cache secrets**, and do not let a trusted pool's cache carry anything its readers should not have.

**What the node holds for a moment.** The Git proxy holds a job's GitHub credentials while it answers that job's request. It passes them to `git` through the environment, never argv, a file or a log, and sends them only to github.com. Default-branch publication needs the App's `actions: read`, which also lets the App read runs, logs and artifacts; `billet github-app create` requests it by default.

**The Actions cache** terminates TLS for one host only, the Actions results origin, and serves only the three cache methods; every other request on that origin goes to GitHub unchanged. Where it runs, the job gets the node's CA. [Transparent Actions cache](../operating/actions-cache.md) covers it.
